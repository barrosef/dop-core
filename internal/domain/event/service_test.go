package event_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT Postgres and WITHOUT NATS: repository and bus
// are ports, and in-memory doubles go in here. It is the practical return on
// hexagonal architecture — including for the hard part, the replay-to-live
// splice.

var t0 = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

// ── bus double ───────────────────────────────────────────────────────────────

type fakeBus struct {
	mu sync.Mutex
	h  ports.Handler
}

func (b *fakeBus) Publish(context.Context, ports.Event) error { return nil }
func (b *fakeBus) Close() error                               { return nil }

func (b *fakeBus) Subscribe(_ context.Context, _, _ string, _ []string, h ports.Handler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.h = h
	return nil
}

// live delivers an event the way the broker would.
func (b *fakeBus) live(e ports.Event) {
	b.mu.Lock()
	h := b.h
	b.mu.Unlock()
	if h != nil {
		_ = h(context.Background(), e)
	}
}

// ── repository double ────────────────────────────────────────────────────────

type fakeRepo struct {
	log []ports.Event // in order of occurrence
	// duringRead runs INSIDE the database read: it is how the test puts an event
	// on the wire exactly in the window between subscribing and reading.
	duringRead func()
}

func (r *fakeRepo) Locate(_ context.Context, accountID, eventID string) (event.Cursor, error) {
	for _, e := range r.log {
		if e.ID == eventID && e.AccountID == accountID {
			return event.Cursor{OccurredAt: e.OccurredAt, ID: e.ID}, nil
		}
	}
	return event.Cursor{}, errs.NotFound("cursor event")
}

func (r *fakeRepo) EventsAfter(_ context.Context, accountID string, after event.Cursor, f event.Filter, limit int) ([]ports.Event, error) {
	if fn := r.duringRead; fn != nil {
		r.duringRead = nil
		fn()
	}
	out := make([]ports.Event, 0, limit)
	for _, e := range r.log {
		if e.AccountID != accountID || !e.OccurredAt.After(after.OccurredAt) || !f.Matches(e) {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ── harness ──────────────────────────────────────────────────────────────────

func ev(id, account, aggregate, kind string, minute int) ports.Event {
	return ports.Event{
		ID: id, AccountID: account, Aggregate: aggregate, AggregateID: "ag-" + id,
		Type: kind, Payload: []byte(`{}`), OccurredAt: t0.Add(time.Duration(minute) * time.Minute),
	}
}

func newService(t *testing.T, repo *fakeRepo) (*event.Service, *fakeBus) {
	t.Helper()
	bus := &fakeBus{}
	svc := event.NewService(repo, bus, fakeClock{t0})
	if err := svc.Start(context.Background(), "test", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return svc, bus
}

func ctxOfAccount(account string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		RequestID: "test", AccountID: account, ActorID: "actor", ActorKind: ctxutil.ActorUser,
	})
}

// collector provides an Emitter that stacks what arrived and signals a channel.
type collector struct {
	mu       sync.Mutex
	received []ports.Event
	signal   chan struct{}
}

func newCollector() *collector { return &collector{signal: make(chan struct{}, 1024)} }

func (c *collector) emit(e ports.Event) error {
	c.mu.Lock()
	c.received = append(c.received, e)
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
	return nil
}

// ids returns what arrived, MINUS the probes (see waitForSubscriber).
func (c *collector) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.received))
	for _, e := range c.received {
		if e.Aggregate != probeAggregate {
			out = append(out, e.ID)
		}
	}
	return out
}

func (c *collector) gotProbe() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.received {
		if e.Aggregate == probeAggregate {
			return true
		}
	}
	return false
}

// probeAggregate marks events that exist only to synchronize the test.
const probeAggregate = "__probe"

// waitForSubscriber waits for Watch to enter the fan-out.
//
// Watch registers the subscriber and ONLY THEN enters the delivery loop; an
// event published before that falls into the void. Sleeping an arbitrary
// interval here would be a race in disguise — so the test knocks with probes
// until one comes back. The assertions discard probes, which is why republishing
// is harmless.
func waitForSubscriber(t *testing.T, bus *fakeBus, account string, arrived func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for i := 0; !arrived(); i++ {
		bus.live(ev(fmt.Sprintf("probe-%d", i), account, probeAggregate, "dop.test.probe", 1))
		select {
		case <-deadline:
			t.Fatal("the subscriber never entered the fan-out")
		case <-time.After(time.Millisecond):
		}
	}
}

// waitFor blocks until the collector has n events, or fails on deadline.
func (c *collector) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		total := len(c.ids())
		if total >= n {
			return
		}
		select {
		case <-c.signal:
		case <-deadline:
			t.Fatalf("expected %d events, %d arrived: %v", n, total, c.ids())
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── THE SPLICE ───────────────────────────────────────────────────────────────

// The central guarantee: between the replay and the live stream there is no gap
// AND no duplicate.
//
// The scenario is exactly the dangerous one: E2 was already committed when the
// client asked for the replay, but was only published to the bus AFTER we
// subscribed and DURING the database read. It appears in both sources. It has to
// come out once.
func TestSpliceOfReplayAndLiveLosesNothingAndRepeatsNothing(t *testing.T) {
	e0 := ev("e0", "acct-a", "project", "dop.hierarchy.project.created", 1)
	e1 := ev("e1", "acct-a", "project", "dop.hierarchy.project.created", 2)
	e2 := ev("e2", "acct-a", "demand", "dop.demand.created", 3)
	e3 := ev("e3", "acct-a", "demand", "dop.demand.stage.advanced", 4)

	repo := &fakeRepo{log: []ports.Event{e0, e1, e2}}
	svc, bus := newService(t, repo)

	// The window: publish E2 while the replay reads the database.
	repo.duringRead = func() { bus.live(e2) }

	c := newCollector()
	ctx, cancel := context.WithCancel(ctxOfAccount("acct-a"))
	defer cancel()

	finished := make(chan error, 1)
	go func() { finished <- svc.Watch(ctx, "e0", event.Filter{}, c.emit) }()

	c.waitFor(t, 2) // replay: e1, e2

	// Now the genuinely live part, after the splice.
	bus.live(e3)
	c.waitFor(t, 3)

	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("Watch returned an error: %v", err)
	}

	want := []string{"e1", "e2", "e3"}
	if got := c.ids(); !equal(got, want) {
		t.Errorf("splice broken: received %v, expected %v", got, want)
	}
}

// With no cursor there is no history: only what comes from here onward.
func TestWithoutACursorThereIsNoReplay(t *testing.T) {
	e1 := ev("e1", "acct-a", "project", "dop.hierarchy.project.created", 1)
	e2 := ev("e2", "acct-a", "project", "dop.hierarchy.project.updated", 2)

	repo := &fakeRepo{log: []ports.Event{e1}}
	svc, bus := newService(t, repo)

	c := newCollector()
	ctx, cancel := context.WithCancel(ctxOfAccount("acct-a"))
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- svc.Watch(ctx, "", event.Filter{}, c.emit) }()
	waitForSubscriber(t, bus, "acct-a", c.gotProbe)

	bus.live(e2)
	c.waitFor(t, 1)
	cancel()
	<-finished

	if got := c.ids(); !equal(got, []string{"e2"}) {
		t.Errorf("with no cursor the history leaked: %v", got)
	}
}

// ── PER-ACCOUNT ISOLATION ────────────────────────────────────────────────────

// A subscriber of account A never receives an event of account B — not in the
// replay, not live. And an event with NO account (migration 0003:
// `identity.user.ensured` happens before the personal account exists) belongs to
// nobody: it leaks to no subscriber.
func TestPerAccountIsolation(t *testing.T) {
	a0 := ev("a0", "acct-a", "account", "dop.identity.account.created", 1)
	a1 := ev("a1", "acct-a", "project", "dop.hierarchy.project.created", 2)
	b1 := ev("b1", "acct-b", "project", "dop.hierarchy.project.created", 3)
	orphan := ev("orphan", "", "user", "dop.identity.user.ensured", 4)
	a2 := ev("a2", "acct-a", "demand", "dop.demand.created", 5)

	// The log holds events from BOTH accounts plus the orphan: replay has to cut.
	repo := &fakeRepo{log: []ports.Event{a0, a1, b1, orphan, a2}}
	svc, bus := newService(t, repo)

	c := newCollector()
	ctx, cancel := context.WithCancel(ctxOfAccount("acct-a"))
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- svc.Watch(ctx, "a0", event.Filter{}, c.emit) }()

	c.waitFor(t, 2) // account A replay: a1, a2

	// Live: only A may pass.
	bus.live(b1)
	bus.live(orphan)
	a3 := ev("a3", "acct-a", "demand", "dop.demand.stage.advanced", 6)
	bus.live(a3)
	c.waitFor(t, 3)

	cancel()
	<-finished

	got := c.ids()
	if !equal(got, []string{"a1", "a2", "a3"}) {
		t.Fatalf("ISOLATION VIOLATED: subscriber of account A received %v", got)
	}
}

// The cursor is attack surface too: an event id from another account must not
// become a valid position — nor confirm that the id exists.
func TestACursorFromAnotherAccountDoesNotResolve(t *testing.T) {
	b1 := ev("b1", "acct-b", "project", "dop.hierarchy.project.created", 1)
	repo := &fakeRepo{log: []ports.Event{b1}}
	svc, _ := newService(t, repo)

	err := svc.Watch(ctxOfAccount("acct-a"), "b1", event.Filter{}, func(ports.Event) error { return nil })
	if errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("expected not_found for a cursor from another account, got %v", err)
	}
}

// A request with no active account is invalid by definition — like the rest of
// the domain (ctxutil.MustAccount).
func TestWatchWithoutAnActiveAccountIsRefused(t *testing.T) {
	svc, _ := newService(t, &fakeRepo{})
	err := svc.Watch(context.Background(), "", event.Filter{}, func(ports.Event) error { return nil })
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("expected invalid_argument with no active account, got %v", err)
	}
}

// ── FILTER ───────────────────────────────────────────────────────────────────

// The filter has to hold identically on both paths: if replay cut one way and
// live cut another, the client would see one event and miss its sibling.
func TestFilterHoldsInReplayAndLive(t *testing.T) {
	e0 := ev("e0", "acct-a", "project", "dop.hierarchy.project.created", 1)
	proj := ev("proj", "acct-a", "project", "dop.hierarchy.project.updated", 2)
	dem := ev("dem", "acct-a", "demand", "dop.demand.created", 3)

	repo := &fakeRepo{log: []ports.Event{e0, proj, dem}}
	svc, bus := newService(t, repo)

	c := newCollector()
	ctx, cancel := context.WithCancel(ctxOfAccount("acct-a"))
	defer cancel()
	f := event.Filter{Aggregates: []string{"project"}}
	finished := make(chan error, 1)
	go func() { finished <- svc.Watch(ctx, "e0", f, c.emit) }()

	c.waitFor(t, 1) // replay: only the project one

	bus.live(ev("dem2", "acct-a", "demand", "dop.demand.created", 4))
	bus.live(ev("proj2", "acct-a", "project", "dop.hierarchy.project.updated", 5))
	c.waitFor(t, 2)

	cancel()
	<-finished

	if got := c.ids(); !equal(got, []string{"proj", "proj2"}) {
		t.Errorf("the filter diverged between replay and live: %v", got)
	}
}

func TestFilterByType(t *testing.T) {
	f := event.Filter{Types: []string{"dop.demand.created"}}
	if !f.Matches(ev("x", "c", "demand", "dop.demand.created", 1)) {
		t.Error("a listed type should match")
	}
	if f.Matches(ev("x", "c", "demand", "dop.demand.stage.advanced", 1)) {
		t.Error("a type outside the list should not match")
	}
	if !(event.Filter{}).Matches(ev("x", "c", "anything", "dop.anything", 1)) {
		t.Error("an empty filter should accept everything")
	}
}

// ── SLOW CONSUMER AND CANCELLATION ───────────────────────────────────────────

// A slow consumer does not stall the server: its queue overflows and IT drops,
// with a clear error telling it to reconnect. The bus never waits.
func TestSlowConsumerDropsInsteadOfStalling(t *testing.T) {
	svc, bus := newService(t, &fakeRepo{})

	release := make(chan struct{})
	var stalled atomic.Bool
	slow := func(ports.Event) error {
		stalled.Store(true)
		<-release // blocks on the first delivery and never leaves
		return nil
	}

	finished := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctxOfAccount("acct-a"))
	defer cancel()
	go func() { finished <- svc.Watch(ctx, "", event.Filter{}, slow) }()
	// The first probe delivered is what stalls the subscriber: `slow` only
	// returns once the test releases it.
	waitForSubscriber(t, bus, "acct-a", stalled.Load)

	// Push far more than the queue holds. If the fan-out blocked, this loop
	// would never finish — the test would blow its timeout.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 4096; i++ {
			bus.live(ev("x", "acct-a", "demand", "dop.demand.created", i+1))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fan-out stalled because of a slow subscriber")
	}

	close(release)
	select {
	case err := <-finished:
		if errs.KindOf(err) != errs.KindUnavailable {
			t.Errorf("expected unavailable for a slow subscriber, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the slow subscriber was not disconnected")
	}
}

// The client disconnects and the subscription dies with it. Watch returns with
// no error: leaving is normal behaviour, not a failure.
func TestCancellationEndsTheSubscription(t *testing.T) {
	svc, bus := newService(t, &fakeRepo{})

	ctx, cancel := context.WithCancel(ctxOfAccount("acct-a"))
	finished := make(chan error, 1)
	var delivered atomic.Bool
	go func() {
		finished <- svc.Watch(ctx, "", event.Filter{}, func(ports.Event) error {
			delivered.Store(true)
			return nil
		})
	}()
	waitForSubscriber(t, bus, "acct-a", delivered.Load)
	cancel()

	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("a client disconnect is not an error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch ignored the cancellation — goroutine leaking")
	}
}

// An Emitter error (Send failed: the client vanished) ends the stream.
func TestADeliveryErrorEndsTheStream(t *testing.T) {
	svc, bus := newService(t, &fakeRepo{})
	ctx, cancel := context.WithCancel(ctxOfAccount("acct-a"))
	defer cancel()

	broken := errs.Internal("client vanished")
	var delivered atomic.Bool
	finished := make(chan error, 1)
	go func() {
		finished <- svc.Watch(ctx, "", event.Filter{}, func(ports.Event) error {
			delivered.Store(true)
			return broken
		})
	}()
	waitForSubscriber(t, bus, "acct-a", delivered.Load)

	select {
	case err := <-finished:
		if err != broken {
			t.Errorf("expected the Emitter error back, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a delivery error did not end the stream")
	}
}

// With no Start there is no subscription: failing explicitly beats handing over
// a mute stream the operator would take hours to diagnose.
func TestWatchBeforeStartIsRefused(t *testing.T) {
	svc := event.NewService(&fakeRepo{}, &fakeBus{}, fakeClock{t0})
	err := svc.Watch(ctxOfAccount("acct-a"), "", event.Filter{}, func(ports.Event) error { return nil })
	if errs.KindOf(err) != errs.KindUnavailable {
		t.Errorf("expected unavailable before Start, got %v", err)
	}
}

// A live tail is a tail: what happened BEFORE it existed is replay business,
// through Postgres. The port delivers everything retained to a freshly created
// durable (guarantee 6); without this cut, every process start would dump that
// history onto whoever was listening.
func TestAnEventOlderThanStartDoesNotEnterTheLiveStream(t *testing.T) {
	svc, bus := newService(t, &fakeRepo{})
	c := newCollector()
	ctx, cancel := context.WithCancel(ctxOfAccount("acct-a"))
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- svc.Watch(ctx, "", event.Filter{}, c.emit) }()
	waitForSubscriber(t, bus, "acct-a", c.gotProbe)

	old := ev("old", "acct-a", "demand", "dop.demand.created", 0)
	old.OccurredAt = t0.Add(-time.Hour)
	bus.live(old)
	bus.live(ev("fresh", "acct-a", "demand", "dop.demand.created", 1))
	c.waitFor(t, 1)

	cancel()
	<-finished

	if got := c.ids(); !equal(got, []string{"fresh"}) {
		t.Errorf("o tail delivered o passado do broker: %v", got)
	}
}
