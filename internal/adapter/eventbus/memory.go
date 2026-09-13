// An in-memory EventBus adapter — NATS's pair through the port.
//
// It exists for two reasons, in this order: (1) to prove the port, which with a
// single adapter is guesswork (ADR-0001); (2) to let the core run in a single
// process — use-case tests and demo mode — with no broker in the way.
//
// What it is NOT: a mock. It really delivers, in a goroutine, with backoff
// retries, an attempt cap and discarding of unreadable messages, because that is
// exactly what JetStream does. A double that delivered synchronously and
// perfectly would hide the bugs that only show up with asynchronous delivery.
//
// The trap this file repeats on purpose: what travels is the ENVELOPE (see
// nats.go). Publish carries e.Payload's BYTES without re-encoding and the
// subscriber receives those same bytes. Re-serializing ports.Event here would
// bring back the bug that cost dearly — a Payload []byte becomes base64 in JSON
// and the other side discards everything, in silence.
package eventbus

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// memoryRetention limits the history kept for subscribers that arrive later.
//
// JetStream's stream retains 30 days; retaining everything here would be a
// memory leak in a long-running process. The number is generous for real use (a
// worker that comes up after the relay) and finite so the process survives.
const memoryRetention = 1024

// memoryBackoff is the redelivery ladder. Shorter than NATS's on purpose: with
// no network in the way, waiting a second would only make tests slow. The port
// does not promise a redelivery time — it promises THAT it redelivers.
var memoryBackoff = []time.Duration{
	5 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond,
}

type Memory struct {
	mu          sync.Mutex
	closed      bool
	history     []delivery
	subscribers []*memSubscription
	wg          sync.WaitGroup
}

func NewMemory() *Memory { return &Memory{} }

// delivery is a message on the wire: subject + raw bytes, plus the attempt
// counter. No ports.Event here — what travels is bytes.
type delivery struct {
	subject string
	data    []byte
	attempt int
}

type memSubscription struct {
	durable  string
	subjects []string
	handler  ports.Handler
	queue    *memQueue
	log      *slog.Logger
	ctx      context.Context
	// bus lets an exhausted delivery publish its dead letter through the same
	// path any other event takes — DLQSubject is a subject like any other, not
	// a second queue with its own rules.
	bus *Memory
}

func (m *Memory) Publish(_ context.Context, e ports.Event) error {
	if strings.TrimSpace(e.Type) == "" {
		return errs.Invalid("event with no type: there is no subject to publish to")
	}
	data := e.Payload
	if len(data) == 0 {
		// The same fallback as nats.go, and for the same reason: whoever
		// publishes without a ready envelope gets an envelope assembled here —
		// never a json.Marshal(ports.Event), whose Payload []byte would come out
		// in base64 and with field names the consumer cannot read.
		data = envelopeOf(e)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errs.New(errs.KindUnavailable, "in-memory bus already closed")
	}
	msg := delivery{subject: e.Type, data: data}
	m.history = append(m.history, msg)
	if len(m.history) > memoryRetention {
		m.history = m.history[len(m.history)-memoryRetention:]
	}
	for _, a := range m.subscribers {
		if matchesAny(a.subjects, msg.subject) {
			a.queue.push(msg)
		}
	}
	return nil
}

// Subscribe delivers what was retained BEFORE the new, as JetStream's durable
// consumer does. Without it, a worker that comes up after the relay would
// silently lose everything already published — and the event spine would only
// work with the right boot order, which is the definition of fragile.
func (m *Memory) Subscribe(ctx context.Context, stream, durable string, subjects []string, h ports.Handler) error {
	if stream != "" && stream != StreamName {
		return errs.NotFound("stream %q", stream)
	}
	if h == nil {
		return errs.Invalid("subscription with no handler")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errs.New(errs.KindUnavailable, "in-memory bus already closed")
	}
	a := &memSubscription{
		durable:  durable,
		subjects: append([]string(nil), subjects...),
		handler:  h,
		queue:    newMemQueue(),
		log:      logging.From(ctx).With("consumer", durable),
		ctx:      ctx,
		bus:      m,
	}
	// A snapshot of the history under the SAME lock as Publish: it is what stops
	// a concurrent publication from being delivered twice or not at all.
	for _, msg := range m.history {
		if matchesAny(a.subjects, msg.subject) {
			a.queue.push(msg)
		}
	}
	m.subscribers = append(m.subscribers, a)
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		a.consume()
	}()
	return nil
}

func (m *Memory) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	subscribers := m.subscribers
	m.mu.Unlock()

	for _, a := range subscribers {
		a.queue.close()
	}
	m.wg.Wait()
	return nil
}

func (a *memSubscription) consume() {
	for {
		msg, ok := a.queue.pop()
		if !ok {
			return
		}
		if a.ctx.Err() != nil {
			return
		}
		var env Envelope
		if err := json.Unmarshal(msg.data, &env); err != nil {
			// Unreadable never improves with a retry: discard it with a record,
			// like NATS's Term(). A poison message must not block the queue
			// forever.
			a.log.Error("unreadable event, discarded", "error", err, "subject", msg.subject)
			continue
		}
		e := eventFrom(env, msg.data) // bytes IDENTICAL to the ones published
		if err := a.handler(a.ctx, e); err != nil {
			msg.attempt++
			if msg.attempt >= MaxDeliver {
				if dlqErr := a.publishDeadLetter(e, err, msg.attempt); dlqErr != nil {
					// Reschedule, do not drop. Failing to record a loss must not
					// cause the loss — this is the in-memory adapter's equivalent
					// of NATS's NAK-instead-of-Term: the redelivery keeps the
					// message alive until the dead letter is actually recorded.
					a.log.Error("failed to publish the dead letter; the event will be redelivered",
						"error", dlqErr, "event_id", e.ID)
					wait := memoryBackoff[min(msg.attempt-1, len(memoryBackoff)-1)]
					rescheduled := msg
					time.AfterFunc(wait, func() { a.queue.push(rescheduled) })
					continue
				}
				a.log.Error("event exhausted its attempts and went to the dead-letter queue",
					"error", err, "type", e.Type, "event_id", e.ID,
					"attempts", msg.attempt, "subject", DLQSubject)
				continue
			}
			a.log.Warn("failed to process the event, it will be redelivered",
				"error", err, "type", e.Type, "event_id", e.ID)
			// Rescheduled outside the queue: while this message waits out the
			// backoff, the others keep moving (it is Nak's effect in JetStream).
			wait := memoryBackoff[min(msg.attempt-1, len(memoryBackoff)-1)]
			rescheduled := msg
			time.AfterFunc(wait, func() { a.queue.push(rescheduled) })
			continue
		}
	}
}

// publishDeadLetter records a loss on the queue that exists to hold it.
//
// Delivered through a.bus.Publish — the same path any other event takes — so
// the record inherits the same envelope shape and history-retention rules as
// everything else on this bus. No second delivery mechanism to keep honest.
func (a *memSubscription) publishDeadLetter(e ports.Event, cause error, attempts int) error {
	dl := buildDeadLetter(a.durable, e, cause, attempts)
	body, err := deadLetterEnvelope(e, dl)
	if err != nil {
		return err
	}
	return a.bus.Publish(a.ctx, ports.Event{
		ID:        e.ID,
		AccountID: e.AccountID,
		Aggregate: "dead_letter",
		Type:      DLQSubject,
		Payload:   body,
	})
}

// envelopeOf builds the wire format from the event — used only when the
// publisher did not bring a ready envelope. Shared by both adapters' Publish
// (they live in the same package): whoever calls Publish directly, without
// going through the outbox, still gets whatever context fields it already set
// on the event — leaving them out here would silently drop them on exactly
// this path.
func envelopeOf(e ports.Event) []byte {
	occurred := e.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	b, _ := json.Marshal(Envelope{
		ID: e.ID, AccountID: e.AccountID, Aggregate: e.Aggregate,
		AggregateID: e.AggregateID, AggregateKey: e.AggregateKey,
		Type: e.Type, OccurredAt: occurred,
		ActorKind: e.ActorKind, ActorID: e.ActorID,
		RequestID: e.RequestID, SessionID: e.SessionID, Caller: e.Caller,
	})
	return b
}

// matchesAny applies NATS's wildcard semantics: "*" matches ONE token, ">"
// matches the tail (at least one token). An empty list matches everything, like
// empty FilterSubjects in JetStream.
func matchesAny(patterns []string, subject string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if matches(p, subject) {
			return true
		}
	}
	return false
}

func matches(pattern, subject string) bool {
	p := strings.Split(pattern, ".")
	a := strings.Split(subject, ".")
	for i, tok := range p {
		if tok == ">" {
			return len(a) > i
		}
		if i >= len(a) {
			return false
		}
		if tok != "*" && tok != a[i] {
			return false
		}
	}
	return len(p) == len(a)
}

// memQueue is an unbounded queue with a signal.
//
// A channel with a fixed buffer does not do: Publish enqueues while holding the
// bus's lock, and a full buffer would block the publisher — in NATS, publishing
// never waits for the consumer.
type memQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []delivery
	closed bool
}

func newMemQueue() *memQueue {
	q := &memQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *memQueue) push(e delivery) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, e)
	q.cond.Signal()
}

func (q *memQueue) pop() (delivery, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return delivery{}, false
	}
	e := q.items[0]
	q.items = q.items[1:]
	return e, true
}

func (q *memQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

var _ ports.EventBus = (*Memory)(nil)
