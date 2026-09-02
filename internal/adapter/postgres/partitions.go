package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionedTables are the tables partitioned by month, on the occurred_at
// column.
//
// Adding a partitioned table and forgetting this list is the same as scheduling
// a failure for the turn of the month — which is why the list lives next to the
// code that uses it, and not scattered across migrations.
var PartitionedTables = []string{"events", "cost_usage"}

// EnsureMonthlyPartitions creates the partitions for the next `ahead` months.
//
// It exists because the migrations create FIXED partitions:
// `0001_foundation.sql` goes to November 2026 and stops. Without this, on the
// first day of the following month EVERY event write fails with "no partition of
// relation found" — and it fails on the outbox's path, that is, it brings down
// any operation that changes state. It is the worst form of bug: no symptom at
// all until a date, and total after it.
//
// Idempotent through `IF NOT EXISTS`: it runs on every scheduler cycle with no
// special care, and several replicas running together do not fight.
//
// `ahead` is slack, not a forecast: with 3 months, the scheduler can be down for
// entire weeks without anyone noticing the difference.
func EnsureMonthlyPartitions(ctx context.Context, pool *pgxpool.Pool, from time.Time, ahead int) ([]string, error) {
	var created []string
	base := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)

	for _, table := range PartitionedTables {
		for i := 0; i <= ahead; i++ {
			start := base.AddDate(0, i, 0)
			end := start.AddDate(0, 1, 0)
			name := fmt.Sprintf("%s_%04d_%02d", table, start.Year(), start.Month())

			// A table name is not parameterizable in DDL; every component comes
			// from constants and date arithmetic, never from external input —
			// there is no injection surface here.
			sql := fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				name, table, start.Format("2006-01-02"), end.Format("2006-01-02"))

			before, err := partitionExists(ctx, pool, name)
			if err != nil {
				return created, err
			}
			if _, err := pool.Exec(ctx, sql); err != nil {
				return created, Translate(err, "partition "+name)
			}
			if !before {
				created = append(created, name)
			}
		}
	}
	return created, nil
}

func partitionExists(ctx context.Context, pool *pgxpool.Pool, name string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r')`,
		name).Scan(&exists)
	if err != nil {
		return false, Translate(err, "checking a partition")
	}
	return exists, nil
}
