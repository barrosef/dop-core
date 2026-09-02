//go:build integration

// Integration tests of the EVENT SPINE against the local environment.
//
//	go test ./test/integration/ -tags=integration -v
//
// They require Postgres and NATS to be reachable (kubectl port-forward). They
// sit behind a build tag so they do not break the `go test ./...` of whoever
// does not have the environment.
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
		t.Skipf("Postgres unavailable: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("Postgres unavailable: %v", err)
	}
	return pool
}

// The outbox's central guarantee: state and event in the SAME transaction. If
// the transaction aborts, NEITHER survives — never "I wrote but did not
// publish".
func TestTheOutboxIsAtomicWithTheState(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		RequestID: "test-atomic", AccountID: "", ActorKind: ctxutil.ActorSystem,
	})

	handle := fmt.Sprintf("test-atomic-%d", time.Now().UnixNano())

	// A transaction that fails AFTER writing state and event.
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
			handle).Scan(&id); err != nil {
			return err
		}
		if err := postgres.Emit(ctx, tx, ports.Event{
			AccountID: id, Aggregate: "account", AggregateID: id,
			Type: "dop.test.atomic", Payload: []byte(`{"x":1}`),
		}); err != nil {
			return err
		}
		return fmt.Errorf("deliberate failure after writing")
	})
	if err == nil {
		t.Fatal("the transaction should have failed")
	}

	// Neither the account nor the event may have survived.
	var accounts, events int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE handle = $1`, handle).Scan(&accounts)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'dop.test.atomic'`).Scan(&events)

	if accounts != 0 {
		t.Errorf("state survived the rollback: %d account(s)", accounts)
	}
	if events != 0 {
		t.Errorf("THE EVENT SURVIVED THE ROLLBACK: %d — the outbox would not be atomic", events)
	}
}

// The complete happy path: transaction → outbox → relay → NATS → projection.
func TestTheWholeSpineUpToTheProjection(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()

	ctx := logging.Into(context.Background(), logging.New("test"))
	ctx = ctxutil.Into(ctx, ctxutil.Call{RequestID: "test-spine", ActorKind: ctxutil.ActorSystem})

	bus, err := eventbus.NewNATS(ctx, env("TEST_NATS_URL", "nats://localhost:4222"))
	if err != nil {
		t.Skipf("NATS unavailable: %v", err)
	}
	defer bus.Close()

	// The projection's consumer, the way the worker does it.
	tl := projection.NewTimeline(pool)
	durable := fmt.Sprintf("test-timeline-%d", time.Now().Unix())
	if err := bus.Subscribe(ctx, "", durable, []string{"dop.test.>"}, tl.Handle); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	handle := fmt.Sprintf("spine-%d", time.Now().UnixNano())
	var accountID string

	// 1. State + event, in the same transaction.
	if err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
			handle).Scan(&accountID); err != nil {
			return err
		}
		return postgres.Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "account", AggregateID: accountID,
			Type:    "dop.test.spine.created",
			Payload: mustJSON(map[string]any{"handle": handle}),
		})
	}); err != nil {
		t.Fatalf("transaction: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
	})

	// 2. The event is pending in the outbox.
	var pending int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox o JOIN events e ON e.id = o.event_id
		  WHERE e.aggregate_id = $1 AND o.published_at IS NULL`, accountID).Scan(&pending)
	if pending != 1 {
		t.Fatalf("expected 1 pending event in the outbox, got %d", pending)
	}

	// 3. The relay publishes.
	relay := postgres.NewRelay(pool, bus, 10)
	if _, err := relay.Drain(ctx); err != nil {
		t.Fatalf("relay: %v", err)
	}

	var stillPending int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox o JOIN events e ON e.id = o.event_id
		  WHERE e.aggregate_id = $1 AND o.published_at IS NULL`, accountID).Scan(&stillPending)
	if stillPending != 0 {
		t.Errorf("the relay should have marked the event as published")
	}

	// 4. The projection received it — with a deadline, because delivery is
	// asynchronous.
	deadline := time.After(15 * time.Second)
	for {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM timeline WHERE aggregate_id = $1`, accountID).Scan(&n)
		if n == 1 {
			return // the whole spine
		}
		select {
		case <-deadline:
			t.Fatal("the projection did not receive the event within 15s")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// A redelivery must not duplicate a row: the projection is idempotent by
// design.
func TestTheProjectionIsIdempotent(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := logging.Into(context.Background(), logging.New("test"))

	tl := projection.NewTimeline(pool)
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	event := ports.Event{Payload: mustJSON(map[string]any{
		"id": uuidFrom(id), "aggregate": "test", "aggregate_id": "ag-" + id,
		"type": "dop.test.idem", "payload": map[string]any{}, "occurred_at": time.Now().UTC(),
	})}

	for i := 0; i < 3; i++ { // the same message delivered three times
		if err := tl.Handle(ctx, event); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM timeline WHERE aggregate_id = $1`, "ag-"+id).Scan(&n)
	if n != 1 {
		t.Errorf("three deliveries produced %d rows — the projection is not idempotent", n)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM timeline WHERE aggregate_id = $1`, "ag-"+id)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// uuidFrom builds a deterministic UUID out of a number, for the test.
func uuidFrom(n string) string {
	h := fmt.Sprintf("%032s", n)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
