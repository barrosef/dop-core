//go:build integration

// Testes de integração da ESPINHA DE EVENTOS contra o ambiente local.
//
//	go test ./test/integration/ -tags=integration -v
//
// Exigem Postgres e NATS acessíveis (kubectl port-forward). Ficam atrás de
// build tag para não quebrarem o `go test ./...` de quem não tem o ambiente.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres/projection"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := env("TEST_DATABASE_URL", "postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("Postgres indisponível: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("Postgres indisponível: %v", err)
	}
	return pool
}

// A garantia central do outbox: estado e evento na MESMA transação. Se a
// transação aborta, NENHUM dos dois sobrevive — nunca "gravei mas não publiquei".
func TestOutboxEhAtomicoComOEstado(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		RequestID: "teste-atomico", AccountID: "", ActorKind: ctxutil.ActorSystem,
	})

	handle := fmt.Sprintf("teste-atomico-%d", time.Now().UnixNano())

	// Transação que falha DEPOIS de gravar estado e evento.
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
			handle).Scan(&id); err != nil {
			return err
		}
		if err := postgres.Emit(ctx, tx, ports.Event{
			AccountID: id, Aggregate: "account", AggregateID: id,
			Type: "dop.teste.atomico", Payload: []byte(`{"x":1}`),
		}); err != nil {
			return err
		}
		return fmt.Errorf("falha proposital depois de gravar")
	})
	if err == nil {
		t.Fatal("a transação deveria ter falhado")
	}

	// Nem a conta nem o evento podem ter sobrevivido.
	var contas, eventos int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE handle = $1`, handle).Scan(&contas)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'dop.teste.atomico'`).Scan(&eventos)

	if contas != 0 {
		t.Errorf("estado sobreviveu ao rollback: %d conta(s)", contas)
	}
	if eventos != 0 {
		t.Errorf("EVENTO SOBREVIVEU AO ROLLBACK: %d — o outbox não seria atômico", eventos)
	}
}

// O caminho feliz completo: transação → outbox → relay → NATS → projeção.
func TestEspinhaCompletaAteAProjecao(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()

	ctx := logging.Into(context.Background(), logging.New("teste"))
	ctx = ctxutil.Into(ctx, ctxutil.Call{RequestID: "teste-espinha", ActorKind: ctxutil.ActorSystem})

	bus, err := eventbus.NewNATS(ctx, env("TEST_NATS_URL", "nats://localhost:4222"))
	if err != nil {
		t.Skipf("NATS indisponível: %v", err)
	}
	defer bus.Close()

	// Consumidor da projeção, como o worker faz.
	tl := projection.NewTimeline(pool)
	durable := fmt.Sprintf("teste-timeline-%d", time.Now().Unix())
	if err := bus.Subscribe(ctx, "", durable, []string{"dop.teste.>"}, tl.Handle); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	handle := fmt.Sprintf("espinha-%d", time.Now().UnixNano())
	var accountID string

	// 1. Estado + evento, na mesma transação.
	if err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
			handle).Scan(&accountID); err != nil {
			return err
		}
		return postgres.Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "account", AggregateID: accountID,
			Type:    "dop.teste.espinha.criada",
			Payload: mustJSON(map[string]any{"handle": handle}),
		})
	}); err != nil {
		t.Fatalf("transação: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
	})

	// 2. O evento está pendente no outbox.
	var pendentes int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox o JOIN events e ON e.id = o.event_id
		  WHERE e.aggregate_id = $1 AND o.published_at IS NULL`, accountID).Scan(&pendentes)
	if pendentes != 1 {
		t.Fatalf("esperava 1 evento pendente no outbox, veio %d", pendentes)
	}

	// 3. O relay publica.
	relay := postgres.NewRelay(pool, bus, 10)
	if _, err := relay.Drain(ctx); err != nil {
		t.Fatalf("relay: %v", err)
	}

	var aindaPendentes int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox o JOIN events e ON e.id = o.event_id
		  WHERE e.aggregate_id = $1 AND o.published_at IS NULL`, accountID).Scan(&aindaPendentes)
	if aindaPendentes != 0 {
		t.Errorf("o relay deveria ter marcado o evento como publicado")
	}

	// 4. A projeção recebeu — com prazo, porque a entrega é assíncrona.
	prazo := time.After(15 * time.Second)
	for {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM timeline WHERE aggregate_id = $1`, accountID).Scan(&n)
		if n == 1 {
			return // espinha completa
		}
		select {
		case <-prazo:
			t.Fatal("a projeção não recebeu o evento em 15s")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Reentrega não pode duplicar linha: a projeção é idempotente por desenho.
func TestProjecaoEhIdempotente(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := logging.Into(context.Background(), logging.New("teste"))

	tl := projection.NewTimeline(pool)
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	evento := ports.Event{Payload: mustJSON(map[string]any{
		"id": uuidFrom(id), "aggregate": "teste", "aggregate_id": "ag-" + id,
		"type": "dop.teste.idem", "payload": map[string]any{}, "occurred_at": time.Now().UTC(),
	})}

	for i := 0; i < 3; i++ { // mesma mensagem entregue três vezes
		if err := tl.Handle(ctx, evento); err != nil {
			t.Fatalf("entrega %d: %v", i+1, err)
		}
	}

	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM timeline WHERE aggregate_id = $1`, "ag-"+id).Scan(&n)
	if n != 1 {
		t.Errorf("três entregas produziram %d linhas — a projeção não é idempotente", n)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM timeline WHERE aggregate_id = $1`, "ag-"+id)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// uuidFrom gera um UUID determinístico a partir de um número, para o teste.
func uuidFrom(n string) string {
	h := fmt.Sprintf("%032s", n)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
