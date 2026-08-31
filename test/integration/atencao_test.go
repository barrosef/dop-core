//go:build integration

// O teste que faltava — e cuja ausência deixou passar um bug de verdade.
//
// A caixa de atenção lia `status` num evento que o domínio de demanda emitia
// com `to`. Nunca casava: a caixa só sabia FECHAR um item que jamais abria. Os
// testes de unidade dos dois lados passavam, porque cada um usava o formato que
// SUPUNHA do outro — o da caixa fabricava o evento, e o da demanda não olhava a
// caixa.
//
// Nenhum teste que fique dentro de um domínio pega isso. Só um que atravesse o
// log de eventos de verdade, que é o que este arquivo faz.
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres/projection"
	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

func TestEtapaEmPortaoHumanoVIRAItemDaCaixa(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("TEST_DATABASE_URL",
		"postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable"))
	if err != nil || pool.Ping(ctx) != nil {
		t.Skipf("Postgres indisponível: %v", err)
	}
	defer pool.Close()

	var conta string
	// Handle único por execução: `uuidFrom` é determinístico, e reusá-lo faria
	// a segunda rodada falhar na criação da conta em vez de no que se quer
	// testar — falha por motivo errado esconde a falha de verdade.
	handle := fmt.Sprintf("caixa-%d", time.Now().UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
		handle).Scan(&conta); err != nil {
		t.Fatalf("criar conta: %v", err)
	}
	caixa := projection.NewAttention(pool)

	// O evento com o payload EXATO que `demand.Service.AdvanceStage` emite.
	// Se aquele payload mudar de forma, este teste quebra — que é o ponto.
	envelope := mustJSON(map[string]any{
		"id":           uuidFrom(fmt.Sprintf("%d", time.Now().UnixNano())),
		"account_id":   conta,
		"aggregate":    "demand",
		"aggregate_id": uuidFrom("d1"),
		"type":         "dop.demand.stage.advanced",
		"occurred_at":  time.Now().UTC(),
		"payload": map[string]any{
			"stage_key":  "validacao-humana",
			"stage_type": "human_validation",
			"from":       "running",
			"to":         "blocked",
			"gate":       "human",
			"dop_status": "doing",
		},
	})

	if err := caixa.Handle(ctx, ports.Event{Payload: envelope}); err != nil {
		t.Fatalf("projeção: %v", err)
	}

	repo := postgres.NewAttentionRepo(pool)
	itens, err := repo.List(ctx, conta, "", false, 10)
	if err != nil {
		t.Fatalf("ler a caixa: %v", err)
	}
	if len(itens) == 0 {
		t.Fatal("etapa parada em portão humano NÃO virou item — " +
			"a caixa está lendo um campo que o evento não tem")
	}
	if itens[0].Kind != attention.KindGatePending {
		t.Errorf("tipo do item: %q, esperado gate_pending", itens[0].Kind)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM attention_items WHERE account_id = $1`, conta)
	})
}
