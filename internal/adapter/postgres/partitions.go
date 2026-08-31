package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionedTables são as tabelas particionadas por mês, na coluna occurred_at.
//
// Acrescentar tabela particionada e esquecer desta lista é o mesmo que agendar
// uma falha para a virada do mês — por isso a lista mora junto do código que a
// usa, e não espalhada em migração.
var PartitionedTables = []string{"events", "cost_usage"}

// EnsureMonthlyPartitions cria as partições dos próximos `ahead` meses.
//
// Existe porque as migrações criam partições FIXAS: `0001_foundation.sql` vai
// até novembro de 2026 e para. Sem isto, no primeiro dia do mês seguinte TODA
// escrita de evento falha com "no partition of relation found" — e falha no
// caminho do outbox, ou seja, derruba qualquer operação que mude estado. É a
// pior forma de bug: sem sintoma nenhum até uma data, e total depois dela.
//
// Idempotente por `IF NOT EXISTS`: roda a cada ciclo do scheduler sem cuidado
// especial, e várias réplicas rodando junto não brigam.
//
// `ahead` é folga, não previsão: com 3 meses, o scheduler pode ficar fora do ar
// semanas inteiras sem que ninguém perceba a diferença.
func EnsureMonthlyPartitions(ctx context.Context, pool *pgxpool.Pool, from time.Time, ahead int) ([]string, error) {
	var criadas []string
	base := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)

	for _, tabela := range PartitionedTables {
		for i := 0; i <= ahead; i++ {
			inicio := base.AddDate(0, i, 0)
			fim := inicio.AddDate(0, 1, 0)
			nome := fmt.Sprintf("%s_%04d_%02d", tabela, inicio.Year(), inicio.Month())

			// Nome de tabela não é parametrizável em DDL; todos os componentes
			// vêm de constantes e de aritmética de data, nunca de entrada
			// externa — não há superfície de injeção aqui.
			sql := fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				nome, tabela, inicio.Format("2006-01-02"), fim.Format("2006-01-02"))

			antes, err := partitionExists(ctx, pool, nome)
			if err != nil {
				return criadas, err
			}
			if _, err := pool.Exec(ctx, sql); err != nil {
				return criadas, Translate(err, "partição "+nome)
			}
			if !antes {
				criadas = append(criadas, nome)
			}
		}
	}
	return criadas, nil
}

func partitionExists(ctx context.Context, pool *pgxpool.Pool, nome string) (bool, error) {
	var existe bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r')`,
		nome).Scan(&existe)
	if err != nil {
		return false, Translate(err, "verificação de partição")
	}
	return existe, nil
}
