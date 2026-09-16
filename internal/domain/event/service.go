package event

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

const (
	// watcherBuffer is the SLOW-CONSUMER POLICY, expressed as a number.
	//
	// Each subscriber has its own bounded queue. If it fills up, the server does
	// NOT wait: it drops that subscriber with Unavailable and moves on. The
	// alternative — blocking on delivery — would let one cockpit on bad wifi
	// stall the fan-out for everybody, the broker included (the handler runs on
	// JetStream's consumption goroutine). Losing a slow client is cheap; it
	// reconnects with since_event_id and loses nothing. Stalling the server is
	// not.
	watcherBuffer = 256

	// replayPage / maxReplay bound the cost of a replay.
	//
	// A very old cursor is NOT this service's case: deep history is read from
	// the timeline projection, paginated, which exists exactly for that. Here
	// replay serves to splice a reconnection, not to rebuild the world.
	replayPage = 500
	maxReplay  = 5000
)

// Service delivers the event log live. It takes only PORTS.
//
// There is ONE bus subscription per process, not one per client: the fan-out to
// subscribers happens in memory. Subscribing per client would create a broker
// consumer for every cockpit tab opened — and the EventBus port does not even
// offer a way to undo that.
type Service struct {
	repo  Repository
	bus   ports.EventBus
	clock ports.Clock

	startOnce sync.Once
	startErr  error
	started   atomic.Bool
	// startedAt cuts the past out of the live stream — see fanout.
	startedAt time.Time

	mu       sync.Mutex
	watchers map[*watcher]struct{}
}

func NewService(repo Repository, bus ports.EventBus, clock ports.Clock) *Service {
	return &Service{repo: repo, bus: bus, clock: clock, watchers: map[*watcher]struct{}{}}
}

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now().UTC()
}

// Start opens the single bus subscription. Idempotent.
//
// The ctx is the PROCESS's, not a client's: the subscription has to outlive
// every Watch that comes and goes. The composition root is the caller.
//
// `consumer` has to be unique PER PROCESS — not per service. Two `serve`
// replicas sharing one name become a consumer group: the event lands in one of
// them and the other's clients never see it. Empty = a random name.
//
// `subjects` is decided by the composition root, as already happens in
// RegisterProjections; the domain does not write broker subject syntax.
func (s *Service) Start(ctx context.Context, consumer string, subjects []string) error {
	s.startOnce.Do(func() {
		if consumer == "" {
			consumer = "event-stream-" + randomSuffix(8)
		}
		s.startedAt = s.now()
		s.startErr = s.bus.Subscribe(ctx, "", consumer, subjects, s.fanout)
		s.started.Store(s.startErr == nil)
	})
	return s.startErr
}

// Watch delivers the active account's events as they happen.
//
// If sinceEventID is filled in, it first drains from the log whatever came after
// it and only then splices into the live stream. The order of the operations
// below is the part that matters — see the splice comment.
func (s *Service) Watch(ctx context.Context, sinceEventID string, f Filter, emit Emitter) error {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return err
	}
	if !s.started.Load() {
		return errs.New(errs.KindUnavailable, "event stream not started")
	}

	// ── THE SPLICE ───────────────────────────────────────────────────────────
	// Subscribe BEFORE reading the database. That is what closes the gap.
	//
	// Every event is published AFTER it commits (the relay reads an outbox that
	// is already written). So for any event: either it was published before we
	// subscribed — and then it committed before that, and therefore before the
	// database read, which comes later: it is in the replay — or it was
	// published after we subscribed, and it is in this watcher's queue. There is
	// no third possibility: nothing is lost.
	//
	// The price is the converse: whatever committed before the read and was only
	// published after the subscription shows up in BOTH. That is why we keep the
	// replay's ids and discard the repeat when it arrives through the queue. The
	// set is bounded by maxReplay, so it fits in memory and holds for the whole
	// stream — including a late republication from the relay.
	w := s.register(accountID, f)
	defer s.unregister(w)

	seen := map[string]struct{}{}
	if sinceEventID != "" {
		if err := s.replay(ctx, accountID, sinceEventID, f, emit, seen); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			// The client disconnected: the defer above removes the watcher from
			// the fan-out and the queue is collected with it. Nothing keeps
			// running.
			return nil
		case <-w.overflow:
			return errs.New(errs.KindUnavailable,
				"subscriber too slow: reconnect with since_event_id of the last event received")
		case e := <-w.ch:
			if _, dup := seen[e.ID]; dup {
				continue // already went out in the replay
			}
			if err := emit(e); err != nil {
				return err
			}
		}
	}
}

// replay drains from the log everything after the cursor, paginated.
func (s *Service) replay(ctx context.Context, accountID, sinceEventID string, f Filter, emit Emitter, seen map[string]struct{}) error {
	cur, err := s.repo.Locate(ctx, accountID, sinceEventID)
	if err != nil {
		return err
	}
	for {
		batch, err := s.repo.EventsAfter(ctx, accountID, cur, f, replayPage)
		if err != nil {
			return err
		}
		for i := range batch {
			e := batch[i]
			if err := emit(e); err != nil {
				return err
			}
			seen[e.ID] = struct{}{}
			cur = Cursor{OccurredAt: e.OccurredAt, ID: e.ID}
		}
		if len(batch) < replayPage {
			return nil // reached the end of the log
		}
		if len(seen) >= maxReplay {
			// Refusing is more honest than silently splicing in mid-history: the
			// client would be left with a hole and no way to know.
			return errs.Precondition(
				"cursor too old for a live replay (limit of %d events); "+
					"read the history through the timeline and reconnect with a recent cursor", maxReplay)
		}
	}
}

// ── fan-out ──────────────────────────────────────────────────────────────────

type watcher struct {
	accountID string
	filter    Filter
	ch        chan ports.Event
	overflow  chan struct{}
	once      sync.Once
}

// offer never blocks: it runs on the bus's consumption goroutine, which is
// shared by ALL subscribers.
func (w *watcher) offer(e ports.Event) {
	select {
	case w.ch <- e:
	default:
		w.once.Do(func() { close(w.overflow) })
	}
}

func (s *Service) register(accountID string, f Filter) *watcher {
	w := &watcher{
		accountID: accountID,
		filter:    f,
		ch:        make(chan ports.Event, watcherBuffer),
		overflow:  make(chan struct{}),
	}
	s.mu.Lock()
	s.watchers[w] = struct{}{}
	s.mu.Unlock()
	return w
}

func (s *Service) unregister(w *watcher) {
	s.mu.Lock()
	delete(s.watchers, w)
	s.mu.Unlock()
	// The queue is NOT closed here: the fan-out may be in the middle of an
	// offer. With no reference left it gets collected — closing would only
	// create a race for nothing.
}

// fanout is the single subscription's handler. It always returns nil:
// delivering to the cockpit is best-effort, and a slow client must not make the
// bus redeliver the event to everybody.
func (s *Service) fanout(_ context.Context, e ports.Event) error {
	// The EventBus port GUARANTEES (guarantee 6) that whatever was published
	// before the subscription is delivered once the durable appears — retention,
	// so the event spine does not depend on boot order. For a projection that is
	// exactly right; for a live tail it is exactly wrong: every process start
	// would dump the broker's retained history onto whoever was listening,
	// overflowing everyone's queue.
	//
	// So the cut is made HERE, where the meaning of a tail is known: live is what
	// happened after it existed; the past is replay's business, through Postgres,
	// which knows how to filter by account and paginate. Nothing is lost at the
	// splice because every event committed after the database read necessarily
	// occurred after Start.
	if e.OccurredAt.Before(s.startedAt) {
		return nil
	}
	e = unwrapEnvelope(e)

	s.mu.Lock()
	targets := make([]*watcher, 0, len(s.watchers))
	for w := range s.watchers {
		if belongsTo(e, w.accountID) && w.filter.Matches(e) {
			targets = append(targets, w)
		}
	}
	s.mu.Unlock()

	for _, w := range targets {
		w.offer(e)
	}
	return nil
}

// unwrapEnvelope reduces Payload to the BUSINESS payload.
//
// The event arriving through the bus carries, in Payload, the whole envelope
// (id, account_id, type, payload...); the other ports.Event fields already come
// unwrapped from the adapter. The event coming from Postgres carries the bare
// payload. Without normalizing here, the SAME event would reach the client in
// two different shapes depending on whether it came through replay or through
// the tail — and the client has no way of knowing which.
//
// The check is strict (envelope id equal to the event id) so as not to confuse
// it with a business payload that happens to have a "payload" field.
func unwrapEnvelope(e ports.Event) ports.Event {
	var env struct {
		ID      string          `json:"id"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		return e
	}
	if env.ID != "" && env.ID == e.ID && len(env.Payload) > 0 {
		e.Payload = env.Payload
	}
	return e
}

func randomSuffix(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
