//go:build integration

// Integration tests of LIVE STREAMING against the local environment.
//
//	go test ./test/integration/ -tags=integration -v
//
// They require Postgres and NATS to be reachable (kubectl port-forward). What
// the in-memory doubles of internal/domain/event cannot prove is precisely what
// is proven here: that the splice still holds when the path is the real one —
// transaction → outbox → relay → NATS → fan-out.
package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/adapter/clock"
	"github.com/barrosef/dop-core/internal/adapter/eventbus"
	"github.com/barrosef/dop-core/internal/adapter/postgres"
	"github.com/barrosef/dop-core/internal/domain/event"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/logging"
)

// A subject of the test's own: the consumer does not have to sieve the local
// environment's real traffic.
const testSubject = "dop.test.stream.>"

type harness struct {
	pool  *pgxpool.Pool
	bus   *eventbus.NATS
	relay *postgres.Relay
	svc   *event.Service
	ctx   context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := openPool(t)
	t.Cleanup(pool.Close)

	ctx := logging.Into(context.Background(), logging.New("test"))
	ctx = ctxutil.Into(ctx, ctxutil.Call{RequestID: "test-stream", ActorKind: ctxutil.ActorSystem})

	bus, err := eventbus.NewNATS(ctx, env("TEST_NATS_URL", "nats://localhost:4222"))
	if err != nil {
		t.Skipf("NATS unavailable: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	svc := event.NewService(postgres.NewEventRepo(pool), bus, clock.NewSystem())
	// A unique consumer per run: two processes with the same name become a
	// consumer group and the event lands on only one of them.
	consumer := fmt.Sprintf("test-stream-%d", time.Now().UnixNano())
	if err := svc.Start(ctx, consumer, []string{testSubject}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return &harness{pool: pool, bus: bus, relay: postgres.NewRelay(pool, bus, 100), svc: svc, ctx: ctx}
}

// account creates a disposable account and returns its id.
func (a *harness) account(t *testing.T, label string) string {
	t.Helper()
	handle := fmt.Sprintf("stream-%s-%d", label, time.Now().UnixNano())
	var id string
	if err := a.pool.QueryRow(a.ctx,
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id::text`,
		handle).Scan(&id); err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	t.Cleanup(func() { _, _ = a.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, id) })
	return id
}

// emit writes the event in the same transaction as always and returns the
// generated id. It stays PENDING in the outbox until somebody calls drain —
// that is how the test controls the instant of publication.
func (a *harness) emit(t *testing.T, accountID, aggregate, aggregateID, kind string) string {
	t.Helper()
	err := postgres.InTx(a.ctx, a.pool, func(tx pgx.Tx) error {
		return postgres.Emit(a.ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: aggregate, AggregateID: aggregateID,
			Type: kind, Payload: mustJSON(map[string]any{"mark": aggregateID}),
		})
	})
	if err != nil {
		t.Fatalf("emitting %s: %v", aggregateID, err)
	}
	var id string
	if err := a.pool.QueryRow(a.ctx,
		`SELECT id::text FROM events WHERE aggregate_id = $1 AND type = $2`, aggregateID, kind).Scan(&id); err != nil {
		t.Fatalf("the id of event %s: %v", aggregateID, err)
	}
	t.Cleanup(func() {
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM outbox WHERE event_id = $1`, id)
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM timeline WHERE event_id = $1`, id)
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, id)
	})
	return id
}

func (a *harness) drain(t *testing.T) {
	t.Helper()
	if _, err := a.relay.Drain(a.ctx); err != nil {
		t.Fatalf("relay: %v", err)
	}
}

// watch brings up a Watch in the background and returns the collector.
func (a *harness) watch(t *testing.T, accountID, since string, f event.Filter) (*received, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctxutil.Into(a.ctx, ctxutil.Call{
		RequestID: "test-stream", AccountID: accountID, ActorKind: ctxutil.ActorUser,
	}))
	col := &received{signal: make(chan struct{}, 256)}
	done := make(chan error, 1)
	go func() { done <- a.svc.Watch(ctx, since, f, col.add) }()
	return col, cancel, done
}

type received struct {
	mu     sync.Mutex
	ids    []string
	signal chan struct{}
}

func (r *received) add(e ports.Event) error {
	r.mu.Lock()
	r.ids = append(r.ids, e.ID)
	r.mu.Unlock()
	select {
	case r.signal <- struct{}{}:
	default:
	}
	return nil
}

func (r *received) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func (r *received) waitFor(t *testing.T, n int, deadline time.Duration) {
	t.Helper()
	limit := time.After(deadline)
	for {
		if len(r.list()) >= n {
			return
		}
		select {
		case <-r.signal:
		case <-time.After(200 * time.Millisecond):
		case <-limit:
			t.Fatalf("expected %d events within %s, got %v", n, deadline, r.list())
		}
	}
}

// ── ISOLATION BY ACCOUNT ─────────────────────────────────────────────────────

// A subscriber of account A never receives an event of account B — and an event
// with no account (account_id NULL, migration 0003) leaks to nobody.
//
// Here the path is the real one: all three events go through the SAME outbox,
// the SAME relay and the SAME NATS consumer. What separates them is the fan-out.
func TestTheStreamIsolatesByAccount(t *testing.T) {
	a := newHarness(t)
	accountA := a.account(t, "a")
	accountB := a.account(t, "b")
	mark := time.Now().UnixNano()

	// The seed: a cursor + an already committed event, for the replay. Waiting
	// for the replay to arrive is what guarantees the subscriber is already in
	// the fan-out — without it the test would be a disguised race.
	cursor := a.emit(t, accountA, "test", fmt.Sprintf("iso-cursor-%d", mark), "dop.test.stream.cursor")
	seed := a.emit(t, accountA, "test", fmt.Sprintf("iso-seed-%d", mark), "dop.test.stream.iso")
	// It drains the seed BEFORE subscribing: that way it can only arrive
	// through the replay, and whatever shows up live afterwards is necessarily
	// new.
	a.drain(t)

	col, cancel, done := a.watch(t, accountA, cursor, event.Filter{})
	defer cancel()
	col.waitFor(t, 1, 10*time.Second)

	// Now the live part: one of each provenance.
	fromA := a.emit(t, accountA, "test", fmt.Sprintf("iso-a-%d", mark), "dop.test.stream.iso")
	fromB := a.emit(t, accountB, "test", fmt.Sprintf("iso-b-%d", mark), "dop.test.stream.iso")
	noAccount := a.emit(t, "", "test", fmt.Sprintf("iso-orphan-%d", mark), "dop.test.stream.iso")
	a.drain(t)

	col.waitFor(t, 2, 15*time.Second)
	// Slack so that the leak, if there were one, has time to show up.
	time.Sleep(2 * time.Second)
	cancel()
	<-done

	got := col.list()
	for _, id := range got {
		if id == fromB {
			t.Fatalf("ISOLATION VIOLATED: an event of account B reached a subscriber of account A (%s)", id)
		}
		if id == noAccount {
			t.Fatalf("ISOLATION VIOLATED: an event with no account reached a subscriber (%s)", id)
		}
	}
	if !sameSet(got, []string{seed, fromA}) {
		t.Errorf("expected exactly [seed, fromA] = %v, received %v", []string{seed, fromA}, got)
	}
}

// ── THE SPLICE ───────────────────────────────────────────────────────────────

// Replay and live splice with no loss and no duplicate, with the real relay.
//
// The scenario is the dangerous one, set up on purpose: the events are ALREADY
// committed (so they enter the replay) and are only published to NATS AFTER the
// replay has finished (so they also arrive through the tail). Whoever subscribes
// before reading the database sees both; deduplication by id is what makes the
// client see only one.
func TestTheStreamSplicesReplayAndLive(t *testing.T) {
	a := newHarness(t)
	account := a.account(t, "splice")
	mark := time.Now().UnixNano()

	cursor := a.emit(t, account, "test", fmt.Sprintf("splice-cursor-%d", mark), "dop.test.stream.cursor")
	a.drain(t) // the cursor leaves the stage; e1 and e2 stay PENDING on purpose.

	e1 := a.emit(t, account, "test", fmt.Sprintf("splice-1-%d", mark), "dop.test.stream.splice")
	e2 := a.emit(t, account, "test", fmt.Sprintf("splice-2-%d", mark), "dop.test.stream.splice")

	col, cancel, done := a.watch(t, account, cursor, event.Filter{})
	defer cancel()

	// Replay: e1 and e2 come out of Postgres.
	col.waitFor(t, 2, 10*time.Second)

	// Only NOW does the relay publish e1 and e2 — which were still pending in
	// the outbox. They arrive through the tail, having already come out in the
	// replay: they have to be discarded.
	a.drain(t)

	// And a genuinely new event, to prove the tail is still alive after the
	// splice (discarding a duplicate must not become discarding everything).
	e3 := a.emit(t, account, "test", fmt.Sprintf("splice-3-%d", mark), "dop.test.stream.splice")
	a.drain(t)

	col.waitFor(t, 3, 15*time.Second)
	// Slack so that the duplicate, if there were one, has time to show up.
	time.Sleep(2 * time.Second)
	cancel()
	<-done

	want := []string{e1, e2, e3}
	got := col.list()
	if len(got) != len(want) {
		t.Fatalf("the splice is broken: expected %v, received %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the splice is broken at position %d: expected %v, received %v", i, want, got)
		}
	}
}

// ── FILTER AND CANCELLATION ──────────────────────────────────────────────────

// The filter by type holds the same on both paths — in the replay's WHERE and
// in the live fan-out.
func TestTheStreamFiltersByType(t *testing.T) {
	a := newHarness(t)
	account := a.account(t, "filter")
	mark := time.Now().UnixNano()

	cursor := a.emit(t, account, "test", fmt.Sprintf("filter-cursor-%d", mark), "dop.test.stream.cursor")
	wanted := a.emit(t, account, "test", fmt.Sprintf("filter-yes-%d", mark), "dop.test.stream.wanted")
	a.emit(t, account, "test", fmt.Sprintf("filter-no-%d", mark), "dop.test.stream.ignored")
	a.drain(t) // the seed only through the replay.

	f := event.Filter{Types: []string{"dop.test.stream.wanted"}}
	col, cancel, done := a.watch(t, account, cursor, f)
	defer cancel()
	col.waitFor(t, 1, 10*time.Second)

	liveWanted := a.emit(t, account, "test", fmt.Sprintf("filter-yes2-%d", mark), "dop.test.stream.wanted")
	a.emit(t, account, "test", fmt.Sprintf("filter-no2-%d", mark), "dop.test.stream.ignored")
	a.drain(t)

	col.waitFor(t, 2, 15*time.Second)
	time.Sleep(time.Second)
	cancel()
	<-done

	if !sameSet(col.list(), []string{wanted, liveWanted}) {
		t.Errorf("the filter let through what it should not have: %v", col.list())
	}
}

// The client disconnected ⇒ Watch returns. Without this, every closed cockpit
// tab would leave a goroutine and a queue behind.
func TestTheStreamDiesWithTheClient(t *testing.T) {
	a := newHarness(t)
	account := a.account(t, "cancel")

	_, cancel, done := a.watch(t, account, "", event.Filter{})
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a client disconnection is not an error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not honour the cancellation — a goroutine is leaking")
	}
}

// sameSet compares ignoring the order: what matters is the set delivered.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	missing := map[string]int{}
	for _, e := range want {
		missing[e]++
	}
	for _, g := range got {
		missing[g]--
	}
	for _, n := range missing {
		if n != 0 {
			return false
		}
	}
	return true
}
