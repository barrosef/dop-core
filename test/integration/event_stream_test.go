//go:build integration

// Testes de integração do STREAMING AO VIVO contra o ambiente local.
//
//	go test ./test/integration/ -tags=integration -v
//
// Exigem Postgres e NATS acessíveis (kubectl port-forward). O que os duplos em
// memória de internal/domain/event não conseguem provar é justamente o que se
// prova aqui: que a emenda continua de pé quando o caminho é o de verdade —
// transação → outbox → relay → NATS → fan-out.
package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/clock"
	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// Assunto próprio para o teste: o consumidor não precisa peneirar o tráfego
// real do ambiente local.
const assuntoDoTeste = "dop.teste.stream.>"

type arreio struct {
	pool  *pgxpool.Pool
	bus   *eventbus.NATS
	relay *postgres.Relay
	svc   *event.Service
	ctx   context.Context
}

func montarArreio(t *testing.T) *arreio {
	t.Helper()
	pool := openPool(t)
	t.Cleanup(pool.Close)

	ctx := logging.Into(context.Background(), logging.New("teste"))
	ctx = ctxutil.Into(ctx, ctxutil.Call{RequestID: "teste-stream", ActorKind: ctxutil.ActorSystem})

	bus, err := eventbus.NewNATS(ctx, env("TEST_NATS_URL", "nats://localhost:4222"))
	if err != nil {
		t.Skipf("NATS indisponível: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	svc := event.NewService(postgres.NewEventRepo(pool), bus, clock.NewSystem())
	// Consumidor único por execução: dois processos com o mesmo nome viram
	// consumer group e o evento cai só em um deles.
	consumidor := fmt.Sprintf("teste-stream-%d", time.Now().UnixNano())
	if err := svc.Start(ctx, consumidor, []string{assuntoDoTeste}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return &arreio{pool: pool, bus: bus, relay: postgres.NewRelay(pool, bus, 100), svc: svc, ctx: ctx}
}

// conta cria uma conta descartável e devolve o id.
func (a *arreio) conta(t *testing.T, rotulo string) string {
	t.Helper()
	handle := fmt.Sprintf("stream-%s-%d", rotulo, time.Now().UnixNano())
	var id string
	if err := a.pool.QueryRow(a.ctx,
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id::text`,
		handle).Scan(&id); err != nil {
		t.Fatalf("criar conta: %v", err)
	}
	t.Cleanup(func() { _, _ = a.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, id) })
	return id
}

// emitir grava o evento na mesma transação de sempre e devolve o id gerado.
// Fica PENDENTE no outbox até alguém chamar drenar — é assim que o teste
// controla o instante da publicação.
func (a *arreio) emitir(t *testing.T, accountID, agregado, agregadoID, tipo string) string {
	t.Helper()
	err := postgres.InTx(a.ctx, a.pool, func(tx pgx.Tx) error {
		return postgres.Emit(a.ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: agregado, AggregateID: agregadoID,
			Type: tipo, Payload: mustJSON(map[string]any{"marca": agregadoID}),
		})
	})
	if err != nil {
		t.Fatalf("emitir %s: %v", agregadoID, err)
	}
	var id string
	if err := a.pool.QueryRow(a.ctx,
		`SELECT id::text FROM events WHERE aggregate_id = $1 AND type = $2`, agregadoID, tipo).Scan(&id); err != nil {
		t.Fatalf("id do evento %s: %v", agregadoID, err)
	}
	t.Cleanup(func() {
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM outbox WHERE event_id = $1`, id)
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM timeline WHERE event_id = $1`, id)
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, id)
	})
	return id
}

func (a *arreio) drenar(t *testing.T) {
	t.Helper()
	if _, err := a.relay.Drain(a.ctx); err != nil {
		t.Fatalf("relay: %v", err)
	}
}

// assistir sobe um Watch em segundo plano e devolve o coletor.
func (a *arreio) assistir(t *testing.T, accountID, since string, f event.Filter) (*recebidos, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctxutil.Into(a.ctx, ctxutil.Call{
		RequestID: "teste-stream", AccountID: accountID, ActorKind: ctxutil.ActorUser,
	}))
	col := &recebidos{sinal: make(chan struct{}, 256)}
	fim := make(chan error, 1)
	go func() { fim <- a.svc.Watch(ctx, since, f, col.add) }()
	return col, cancel, fim
}

type recebidos struct {
	mu    sync.Mutex
	ids   []string
	sinal chan struct{}
}

func (r *recebidos) add(e ports.Event) error {
	r.mu.Lock()
	r.ids = append(r.ids, e.ID)
	r.mu.Unlock()
	select {
	case r.sinal <- struct{}{}:
	default:
	}
	return nil
}

func (r *recebidos) lista() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func (r *recebidos) esperar(t *testing.T, n int, prazo time.Duration) {
	t.Helper()
	limite := time.After(prazo)
	for {
		if len(r.lista()) >= n {
			return
		}
		select {
		case <-r.sinal:
		case <-time.After(200 * time.Millisecond):
		case <-limite:
			t.Fatalf("esperava %d eventos em %s, chegaram %v", n, prazo, r.lista())
		}
	}
}

// ── ISOLAMENTO POR CONTA ─────────────────────────────────────────────────────

// Um assinante da conta A jamais recebe evento da conta B — e evento sem conta
// (account_id NULL, migração 0003) não vaza para ninguém.
//
// Aqui o caminho é o real: os três eventos passam pelo MESMO outbox, pelo MESMO
// relay e pelo MESMO consumidor NATS. Quem separa é o fan-out.
func TestStreamIsolaPorConta(t *testing.T) {
	a := montarArreio(t)
	contaA := a.conta(t, "a")
	contaB := a.conta(t, "b")
	marca := time.Now().UnixNano()

	// Semente: cursor + um evento já commitado, para o replay. Esperar o replay
	// chegar é o que garante que o assinante já entrou no fan-out — sem isso o
	// teste seria uma corrida disfarçada.
	cursor := a.emitir(t, contaA, "teste", fmt.Sprintf("iso-cursor-%d", marca), "dop.teste.stream.cursor")
	semente := a.emitir(t, contaA, "teste", fmt.Sprintf("iso-semente-%d", marca), "dop.teste.stream.iso")
	// Drena a semente ANTES de assinar: assim ela só pode chegar pelo replay, e
	// o que aparecer ao vivo depois é necessariamente novo.
	a.drenar(t)

	col, cancel, fim := a.assistir(t, contaA, cursor, event.Filter{})
	defer cancel()
	col.esperar(t, 1, 10*time.Second)

	// Agora o ao vivo: um de cada procedência.
	doA := a.emitir(t, contaA, "teste", fmt.Sprintf("iso-a-%d", marca), "dop.teste.stream.iso")
	doB := a.emitir(t, contaB, "teste", fmt.Sprintf("iso-b-%d", marca), "dop.teste.stream.iso")
	semConta := a.emitir(t, "", "teste", fmt.Sprintf("iso-orfao-%d", marca), "dop.teste.stream.iso")
	a.drenar(t)

	col.esperar(t, 2, 15*time.Second)
	// Folga para o vazamento, se existisse, ter tempo de aparecer.
	time.Sleep(2 * time.Second)
	cancel()
	<-fim

	got := col.lista()
	for _, id := range got {
		if id == doB {
			t.Fatalf("ISOLAMENTO VIOLADO: evento da conta B chegou ao assinante da conta A (%s)", id)
		}
		if id == semConta {
			t.Fatalf("ISOLAMENTO VIOLADO: evento sem conta chegou a um assinante (%s)", id)
		}
	}
	if !mesmos(got, []string{semente, doA}) {
		t.Errorf("esperava exatamente [semente, doA] = %v, recebido %v", []string{semente, doA}, got)
	}
}

// ── A EMENDA ─────────────────────────────────────────────────────────────────

// Replay e ao vivo emendam sem perda e sem duplicata, com o relay de verdade.
//
// O cenário é o perigoso, montado de propósito: os eventos JÁ ESTÃO commitados
// (portanto entram no replay) e só são publicados no NATS DEPOIS que o replay
// terminou (portanto chegam também pelo tail). Quem assina antes de ler o banco
// vê os dois; a desduplicação por id é o que faz o cliente ver um só.
func TestStreamEmendaReplayAoVivo(t *testing.T) {
	a := montarArreio(t)
	conta := a.conta(t, "emenda")
	marca := time.Now().UnixNano()

	cursor := a.emitir(t, conta, "teste", fmt.Sprintf("emenda-cursor-%d", marca), "dop.teste.stream.cursor")
	a.drenar(t) // o cursor sai de cena; e1 e e2 ficam PENDENTES de propósito.

	e1 := a.emitir(t, conta, "teste", fmt.Sprintf("emenda-1-%d", marca), "dop.teste.stream.emenda")
	e2 := a.emitir(t, conta, "teste", fmt.Sprintf("emenda-2-%d", marca), "dop.teste.stream.emenda")

	col, cancel, fim := a.assistir(t, conta, cursor, event.Filter{})
	defer cancel()

	// Replay: e1 e e2 saem do Postgres.
	col.esperar(t, 2, 10*time.Second)

	// Só AGORA o relay publica e1 e e2 — que ainda estavam pendentes no outbox.
	// Eles chegam pelo tail, já tendo saído no replay: têm que ser descartados.
	a.drenar(t)

	// E um evento genuinamente novo, para provar que o tail continua vivo depois
	// da emenda (descartar duplicata não pode virar descartar tudo).
	e3 := a.emitir(t, conta, "teste", fmt.Sprintf("emenda-3-%d", marca), "dop.teste.stream.emenda")
	a.drenar(t)

	col.esperar(t, 3, 15*time.Second)
	// Folga para a duplicata, se existisse, ter tempo de aparecer.
	time.Sleep(2 * time.Second)
	cancel()
	<-fim

	esperado := []string{e1, e2, e3}
	got := col.lista()
	if len(got) != len(esperado) {
		t.Fatalf("emenda quebrada: esperava %v, recebido %v", esperado, got)
	}
	for i := range esperado {
		if got[i] != esperado[i] {
			t.Fatalf("emenda quebrada na posição %d: esperava %v, recebido %v", i, esperado, got)
		}
	}
}

// ── FILTRO E CANCELAMENTO ────────────────────────────────────────────────────

// O filtro por tipo vale igual nos dois caminhos — no WHERE do replay e no
// fan-out ao vivo.
func TestStreamFiltraPorTipo(t *testing.T) {
	a := montarArreio(t)
	conta := a.conta(t, "filtro")
	marca := time.Now().UnixNano()

	cursor := a.emitir(t, conta, "teste", fmt.Sprintf("filtro-cursor-%d", marca), "dop.teste.stream.cursor")
	querido := a.emitir(t, conta, "teste", fmt.Sprintf("filtro-sim-%d", marca), "dop.teste.stream.querido")
	a.emitir(t, conta, "teste", fmt.Sprintf("filtro-nao-%d", marca), "dop.teste.stream.ignorado")
	a.drenar(t) // semente só pelo replay.

	f := event.Filter{Types: []string{"dop.teste.stream.querido"}}
	col, cancel, fim := a.assistir(t, conta, cursor, f)
	defer cancel()
	col.esperar(t, 1, 10*time.Second)

	aoVivoQuerido := a.emitir(t, conta, "teste", fmt.Sprintf("filtro-sim2-%d", marca), "dop.teste.stream.querido")
	a.emitir(t, conta, "teste", fmt.Sprintf("filtro-nao2-%d", marca), "dop.teste.stream.ignorado")
	a.drenar(t)

	col.esperar(t, 2, 15*time.Second)
	time.Sleep(time.Second)
	cancel()
	<-fim

	if !mesmos(col.lista(), []string{querido, aoVivoQuerido}) {
		t.Errorf("o filtro deixou passar o que não devia: %v", col.lista())
	}
}

// Cliente desconectou ⇒ Watch retorna. Sem isso, cada aba fechada do cockpit
// deixaria uma goroutine e uma fila para trás.
func TestStreamMorreComOCliente(t *testing.T) {
	a := montarArreio(t)
	conta := a.conta(t, "cancel")

	_, cancel, fim := a.assistir(t, conta, "", event.Filter{})
	cancel()

	select {
	case err := <-fim:
		if err != nil {
			t.Errorf("desconexão do cliente não é erro, veio %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch não respeitou o cancelamento — goroutine vazando")
	}
}

// mesmos compara ignorando a ordem: o que importa é o conjunto entregue.
func mesmos(got, esperado []string) bool {
	if len(got) != len(esperado) {
		return false
	}
	falta := map[string]int{}
	for _, e := range esperado {
		falta[e]++
	}
	for _, g := range got {
		falta[g]--
	}
	for _, n := range falta {
		if n != 0 {
			return false
		}
	}
	return true
}
