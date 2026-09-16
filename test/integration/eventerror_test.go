//go:build integration

// Integration tests of the event_errors table against the local environment.
//
//	go test ./test/integration/ -tags=integration -v
package integration

import (
	"context"
	"fmt"
	"time"

	"testing"

	"github.com/barrosef/dop-core/internal/adapter/postgres"
	"github.com/barrosef/dop-core/internal/domain/event"
	"github.com/barrosef/dop-core/internal/domain/ports"
)

// TestTheEventErrorTableIsIdempotent pins the fix for a DLQConsumer.Handle
// that is not idempotent: a redelivery after Record already committed a row
// (because something later in the same Handle call failed — RecordExhausted,
// for instance) must not insert a SECOND terminal row for the same (event,
// consumer). ON CONFLICT (event_id, consumer) DO NOTHING is what makes a
// repeated Record call a no-op, the same way `timeline`'s own idempotent
// INSERT already guards the projection.
func TestTheEventErrorTableIsIdempotent(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	repo := postgres.NewEventErrorRepo(pool)
	id := fmt.Sprintf("evt-idem-%d", time.Now().UnixNano())
	dl := event.DeadLetter{
		Event: ports.Event{
			ID: id, Aggregate: "test", AggregateID: "ag-" + id,
			Type: "dop.test.idem",
		},
		Consumer: "notification",
		Attempts: []event.Attempt{
			{ErrorKind: "unavailable", ErrorCode: "mail.provider_down", ErrorMessage: "boom"},
		},
		FirstFailedAt: time.Now().UTC(),
		LastFailedAt:  time.Now().UTC(),
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM event_errors WHERE event_id = $1 AND consumer = $2`, id, "notification")
	})

	for i := 0; i < 3; i++ { // the same redelivery landing here three times
		if err := repo.Record(ctx, dl, event.Irrecoverable); err != nil {
			t.Fatalf("Record, delivery %d: %v", i+1, err)
		}
	}

	var n int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM event_errors WHERE event_id = $1 AND consumer = $2`,
		id, "notification").Scan(&n)
	if n != 1 {
		t.Errorf("three deliveries produced %d rows — event_errors is not idempotent", n)
	}
}
