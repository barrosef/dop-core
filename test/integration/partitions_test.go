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

// The migrations create FIXED partitions and stop at November 2026. With no
// maintenance, at the turn of the following month every event write fails — on
// the outbox's path, which is to say bringing down any operation that changes
// state.
//
// This test proves both halves: that the partitions are born, and that running
// again does not break (the scheduler runs this every minute).
func TestFuturePartitionsAreCreatedAndItIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("TEST_DATABASE_URL", "postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable"))
	if err != nil || pool.Ping(ctx) != nil {
		t.Skipf("Postgres unavailable: %v", err)
	}
	// `t.Cleanup` runs AFTER the test function returns, and `defer` runs
	// BEFORE — so closing the pool with defer would leave the cleanup with no
	// connection. Registered here, the close becomes the LAST cleanup (they run
	// in reverse registration order), after the DROP further down.
	t.Cleanup(pool.Close)

	// A date well ahead, so as not to depend on what already exists today.
	future := time.Date(2027, 6, 15, 0, 0, 0, 0, time.UTC)

	// It deletes BEFORE, not only after: a test that depends on the previous
	// run's cleanup having worked fails for the wrong reason when it did not —
	// which is exactly what happened here, and the symptom ("no partition
	// created") sent us looking in the production code.
	clearPartitions(t, ctx, pool)

	created, err := postgres.EnsureMonthlyPartitions(ctx, pool, future, 3)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(created) == 0 {
		t.Fatal("no partition created — the test would prove nothing")
	}
	t.Logf("created: %v", created)

	// Idempotency is no detail: the scheduler runs every minute, forever.
	again, err := postgres.EnsureMonthlyPartitions(ctx, pool, future, 3)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the second pass created %v — it should be empty", again)
	}

	// And what really matters: it is possible to write into the month that had
	// no home before. Without this, we would prove only that the table exists.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = 'events_2027_06')`).
		Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("events_2027_06 does not exist after the creation")
	}

	t.Cleanup(func() { clearPartitions(t, context.Background(), pool) })
}

// The hole is not hypothetical: the migrations stop in October 2026, and today
// is August 2026. November does not exist. This test runs with the REAL date to
// prove the scheduler's cycle closes the hole before it hurts.
func TestTheNovemberHoleIsClosedByTheRealCycle(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("TEST_DATABASE_URL",
		"postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable"))
	if err != nil || pool.Ping(ctx) != nil {
		t.Skipf("Postgres unavailable: %v", err)
	}
	defer pool.Close()

	if _, err := postgres.EnsureMonthlyPartitions(ctx, pool, time.Now().UTC(), 3); err != nil {
		t.Fatalf("the scheduler's cycle: %v", err)
	}

	// Actually writing into the month that had no home before is what separates
	// "the table exists" from "the write works".
	//
	// Anchored on the FIRST day of the month before adding: `AddDate(0,1,0)`
	// from 31 August normalizes to 1 October — which already existed, and would
	// make this test pass without proving anything. Three months ahead falls
	// outside what the migrations created, which is precisely the hole.
	now := time.Now().UTC()
	monthsAhead := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.UTC).AddDate(0, 3, 0)
	var target string
	err = pool.QueryRow(ctx, `
		INSERT INTO events (id, account_id, aggregate, aggregate_id, type, payload, occurred_at)
		VALUES (gen_random_uuid(), NULL, 'test', gen_random_uuid(), 'dop.test.partition', '{}', $1)
		RETURNING tableoid::regclass::text`, monthsAhead).Scan(&target)
	if err != nil {
		t.Fatalf("the write into a later month failed — the hole is still open: %v", err)
	}
	t.Logf("the later month's event landed in %s", target)

	_, _ = pool.Exec(ctx, `DELETE FROM events WHERE type = 'dop.test.partition'`)
}

// clearPartitions removes the partitions this test creates.
//
// The error is REPORTED, not swallowed: the first version used `_` and the
// cleanup failure stayed invisible for whole runs — the surviving partition made
// the next run fail saying "no partition created", which points at the
// production code instead of at the cleanup.
func clearPartitions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range postgres.PartitionedTables {
		for month := 6; month <= 10; month++ {
			name := fmt.Sprintf("%s_2027_%02d", table, month)
			if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
				t.Logf("cleaning up %s failed: %v", name, err)
			}
		}
	}
}
