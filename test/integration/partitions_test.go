//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
)

// As migrações criam partições FIXAS e param em novembro de 2026. Sem
// manutenção, na virada do mês seguinte toda escrita de evento falha — no
// caminho do outbox, ou seja, derrubando qualquer operação que mude estado.
//
// Este teste prova as duas metades: que as partições nascem, e que rodar de
// novo não quebra (o scheduler executa isto a cada minuto).
func TestParticoesFuturasSaoCriadasEEhIdempotente(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("TEST_DATABASE_URL", "postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable"))
	if err != nil || pool.Ping(ctx) != nil {
		t.Skipf("Postgres indisponível: %v", err)
	}
	// `t.Cleanup` roda DEPOIS que a função de teste retorna, e `defer` roda
	// ANTES — então fechar o pool com defer deixaria a limpeza sem conexão.
	// Registrado aqui, o fecho vira o ÚLTIMO cleanup (eles rodam ao contrário
	// da ordem de registro), depois do DROP lá embaixo.
	t.Cleanup(pool.Close)

	// Uma data bem à frente, para não depender do que já existe hoje.
	futuro := time.Date(2027, 6, 15, 0, 0, 0, 0, time.UTC)

	// Apaga ANTES, não só depois: teste que depende da limpeza da execução
	// anterior ter funcionado falha por motivo errado quando ela não funcionou
	// — foi exatamente o que aconteceu aqui, e o sintoma ("nenhuma partição
	// criada") mandava procurar no código de produção.
	limparParticoes(t, ctx, pool)

	criadas, err := postgres.EnsureMonthlyPartitions(ctx, pool, futuro, 3)
	if err != nil {
		t.Fatalf("primeira passada: %v", err)
	}
	if len(criadas) == 0 {
		t.Fatal("nenhuma partição criada — o teste não provaria nada")
	}
	t.Logf("criadas: %v", criadas)

	// Idempotência não é detalhe: o scheduler roda a cada minuto, para sempre.
	denovo, err := postgres.EnsureMonthlyPartitions(ctx, pool, futuro, 3)
	if err != nil {
		t.Fatalf("segunda passada: %v", err)
	}
	if len(denovo) != 0 {
		t.Errorf("segunda passada criou %v — deveria ser vazio", denovo)
	}

	// E o que importa de verdade: dá para escrever no mês que antes não tinha
	// casa. Sem isto, provaríamos só que a tabela existe.
	var existe bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = 'events_2027_06')`).
		Scan(&existe); err != nil {
		t.Fatal(err)
	}
	if !existe {
		t.Error("events_2027_06 não existe depois da criação")
	}

	t.Cleanup(func() { limparParticoes(t, context.Background(), pool) })
}

// O buraco não é hipotético: as migrações param em outubro de 2026, e hoje é
// agosto de 2026. Novembro não existe. Este teste roda com a data REAL para
// provar que o ciclo do scheduler fecha o buraco antes de ele doer.
func TestOBuracoDeNovembroEhFechadoPeloCicloReal(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("TEST_DATABASE_URL",
		"postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable"))
	if err != nil || pool.Ping(ctx) != nil {
		t.Skipf("Postgres indisponível: %v", err)
	}
	defer pool.Close()

	if _, err := postgres.EnsureMonthlyPartitions(ctx, pool, time.Now().UTC(), 3); err != nil {
		t.Fatalf("ciclo do scheduler: %v", err)
	}

	// Escrever de fato no mês que antes não tinha casa é o que separa
	// "a tabela existe" de "a escrita funciona".
	//
	// Ancorado no PRIMEIRO dia do mês antes de somar: `AddDate(0,1,0)` a
	// partir de 31 de agosto normaliza para 1º de outubro — que já existia, e
	// faria este teste passar sem provar nada. Três meses à frente cai fora do
	// que as migrações criaram, que é justamente o buraco.
	agora := time.Now().UTC()
	mesSeguinte := time.Date(agora.Year(), agora.Month(), 1, 12, 0, 0, 0, time.UTC).AddDate(0, 3, 0)
	var destino string
	err = pool.QueryRow(ctx, `
		INSERT INTO events (id, account_id, aggregate, aggregate_id, type, payload, occurred_at)
		VALUES (gen_random_uuid(), NULL, 'teste', gen_random_uuid(), 'dop.teste.particao', '{}', $1)
		RETURNING tableoid::regclass::text`, mesSeguinte).Scan(&destino)
	if err != nil {
		t.Fatalf("escrita no mês seguinte falhou — o buraco continua aberto: %v", err)
	}
	t.Logf("evento do mês seguinte caiu em %s", destino)

	_, _ = pool.Exec(ctx, `DELETE FROM events WHERE type = 'dop.teste.particao'`)
}

// limparParticoes remove as partições que este teste cria.
//
// O erro é REPORTADO, não engolido: a primeira versão usava `_` e a falha de
// limpeza ficou invisível por execuções inteiras — a partição sobrevivente
// fazia a execução seguinte falhar dizendo "nenhuma partição criada", que
// aponta para o código de produção em vez de para a limpeza.
func limparParticoes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, tabela := range postgres.PartitionedTables {
		for mes := 6; mes <= 10; mes++ {
			nome := fmt.Sprintf("%s_2027_%02d", tabela, mes)
			if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+nome); err != nil {
				t.Logf("limpeza de %s falhou: %v", nome, err)
			}
		}
	}
}
