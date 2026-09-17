// An EventBus adapter over NATS JetStream (ADR-0014).
//
// The choice: lightweight (one container), runs identically on k3s and GKE,
// persistent, with consumer groups, a DLQ and replay. Kafka would be a truck for
// our load; Pub/Sub would tie us to GCP — it stays as a second adapter for
// whoever wants a managed one.
package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/barrosef/dop-core/internal/domain/event"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/logging"
)

const (
	StreamName    = "DOP"
	StreamSubject = "dop.>"
	// After this many attempts the message goes to the DLQ instead of blocking
	// the queue forever.
	MaxDeliver = 5
	// DLQSubject is where an exhausted event goes. It lives OUTSIDE `dop.>` on
	// purpose: `timeline` (and, until this fix, the live-event service too)
	// subscribe to `dop.>` — EVERY subject the platform uses. A dead letter
	// published inside that wildcard is redelivered to consumers that have no
	// idea what a DeadLetter envelope is: `timeline` tried to INSERT the
	// record's id into a `uuid` column, failed forever (22P02), exhausted its
	// own budget, and published its OWN dead letter — which it received again.
	// Seven generations were reproduced in five seconds on the memory bus.
	// The stream still retains it for the same 30 days: `dlq.>` is added to
	// the stream's subject list below, alongside `dop.>`, instead of nesting
	// the DLQ inside the subject every ordinary consumer already wildcards.
	DLQSubject = "dlq.event"
)

// Envelope is the event's WIRE FORMAT: what the relay publishes and what the
// consumer receives. Explicit on purpose — before, it was implicit and the
// consumer tried to deserialize into ports.Event, whose Payload []byte expects
// base64; the envelope has the payload as an OBJECT. It silenced every
// delivery.
type Envelope struct {
	ID           string          `json:"id"`
	AccountID    string          `json:"account_id"`
	Aggregate    string          `json:"aggregate"`
	AggregateID  string          `json:"aggregate_id"`
	AggregateKey string          `json:"aggregate_key,omitempty"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload"`
	OccurredAt   time.Time       `json:"occurred_at"`
	ActorKind    string          `json:"actor_kind,omitempty"`
	ActorID      string          `json:"actor_id,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	Caller       string          `json:"caller,omitempty"`
}

// eventFrom builds the port's Event from the wire envelope. One function, used
// by both adapters' consumers, so they can never disagree about which fields
// cross.
func eventFrom(env Envelope, raw []byte) ports.Event {
	return ports.Event{
		ID:           env.ID,
		AccountID:    env.AccountID,
		Aggregate:    env.Aggregate,
		AggregateID:  env.AggregateID,
		AggregateKey: env.AggregateKey,
		Type:         env.Type,
		Payload:      raw,
		OccurredAt:   env.OccurredAt,
		ActorKind:    env.ActorKind,
		ActorID:      env.ActorID,
		RequestID:    env.RequestID,
		SessionID:    env.SessionID,
		Caller:       env.Caller,
	}
}

// buildDeadLetter assembles the record for an event that exhausted its
// attempts. Shared by both adapters so they can never disagree on shape: the
// contract asserts NATS and the in-memory bus produce the same record for the
// same failure, and a function called from both is how that stays true instead
// of being hoped for.
//
// The attempt history has ONE entry here, and that is honest: neither adapter
// keeps the earlier failures around — JetStream redelivers without telling the
// process what they were, and the in-memory queue does not persist them either.
// The count is real (the caller's `attempts`) and travels as BrokerAttempts —
// the history is what this delivery witnessed, the count is how many the
// broker actually made before handing it here.
func buildDeadLetter(consumer string, e ports.Event, cause error, attempts int) event.DeadLetter {
	now := time.Now().UTC()
	// errs.CodeOrKind, not CodeOf: the notifier, projection and notification
	// adapters this feature classifies have ZERO WithCode call sites, so a
	// bare CodeOf would leave ErrorCode empty for every real failure. The
	// signature key is (consumer, code) — an empty code collapses every
	// failure of a consumer into ONE row, and one broken template promotes
	// the whole consumer to irrecoverable, skipping the retry even for a
	// transient timeout that arrives later.
	code := errs.CodeOrKind(cause)
	return event.DeadLetter{
		Event:    e,
		Consumer: consumer,
		Attempts: []event.Attempt{{
			At:           now,
			ErrorKind:    string(errs.KindOf(cause)),
			ErrorCode:    code,
			ErrorMessage: cause.Error(),
		}},
		Classification: string(event.Classify(cause)),
		FirstFailedAt:  now,
		LastFailedAt:   now,
		BrokerAttempts: attempts,
	}
}

// deadLetterEnvelope wraps a DeadLetter in the wire envelope every consumer
// already expects.
//
// A bare DeadLetter JSON as the message body would decode into an Envelope
// with every field empty and NO error at all — Subscribe's handler always
// unmarshals the body into Envelope, and an object with none of Envelope's
// keys is a syntactically valid, semantically empty one. That silent failure
// is the entire reason this task exists; the DLQ record must not reintroduce
// it. So the DeadLetter JSON becomes the envelope's inner `payload`, and the
// identifying fields come from the event that failed.
//
// id is the RECORD's identity, not the failed event's — see the comment on
// its construction in publishDeadLetter. It is deliberately a separate
// parameter from e.ID so nothing here can quietly go back to using the bare
// event id.
func deadLetterEnvelope(id string, e ports.Event, dl event.DeadLetter) ([]byte, error) {
	dlBody, err := json.Marshal(dl)
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable dead letter")
	}
	body, err := json.Marshal(Envelope{
		ID:           id,
		AccountID:    e.AccountID,
		Aggregate:    "dead_letter",
		AggregateID:  e.AggregateID,
		AggregateKey: e.AggregateKey,
		Type:         DLQSubject,
		Payload:      dlBody,
		OccurredAt:   dl.LastFailedAt,
	})
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "unreadable dead-letter envelope")
	}
	return body, nil
}

// DeadLetterFrom is deadLetterEnvelope's inverse: it decodes the ports.Event a
// subscription delivers for a dead letter back into the domain type.
//
// Exported and called from every place that reads a dead letter back —
// DLQConsumer.Handle, and the contract suite's own assertions — instead of
// each one re-deriving the two-step unwrap by hand. Before this existed, the
// shape lived independently in three places (this encoder, DLQConsumer's own
// unmarshal, and the contract suite's), and changing the encoder here could
// leave one of the others decoding a zero-valued record with no error at
// all — the exact silent failure this whole feature exists to remove,
// reachable by a second route.
func DeadLetterFrom(e ports.Event) (event.DeadLetter, error) {
	var env Envelope
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		return event.DeadLetter{}, errs.Wrap(errs.KindInvalid, err, "unreadable dead-letter envelope")
	}
	var dl event.DeadLetter
	if err := json.Unmarshal(env.Payload, &dl); err != nil {
		return event.DeadLetter{}, errs.Wrap(errs.KindInvalid, err, "unreadable dead letter")
	}
	return dl, nil
}

type NATS struct {
	conn   *nats.Conn
	js     jetstream.JetStream
	stream jetstream.Stream
}

func NewNATS(ctx context.Context, url string) (*NATS, error) {
	conn, err := nats.Connect(url,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to connect to NATS")
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to open JetStream")
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: StreamName,
		// dlq.> is a SEPARATE branch from dop.>, not a subject under it — see
		// the comment on DLQSubject for why that separation is load-bearing.
		// Both live in the same stream, so they share the same retention.
		Subjects:  []string{StreamSubject, "dlq.>"},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		MaxAge:    30 * 24 * time.Hour,
		Discard:   jetstream.DiscardOld,
	})
	if err != nil {
		conn.Close()
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to create the stream")
	}
	return &NATS{conn: conn, js: js, stream: stream}, nil
}

func (n *NATS) Publish(ctx context.Context, e ports.Event) error {
	payload := e.Payload
	if len(payload) == 0 {
		// With no ready envelope (whoever publishes directly, without going
		// through the outbox): build one. There used to be a json.Marshal(e)
		// here — which is the SAME bug the Envelope exists to kill, only on the
		// publisher's side: ports.Event serializes Payload []byte in base64 and
		// with field names the consumer cannot read, so AccountID, AggregateID
		// and OccurredAt arrived empty on the other side, with no error at all.
		payload = envelopeOf(e)
	}
	// MsgId gives deduplication on the broker's side: the relay can republish
	// without producing a double delivery within JetStream's dedup window.
	_, err := n.js.PublishMsg(ctx, &nats.Msg{
		Subject: e.Type,
		Data:    payload,
		Header:  nats.Header{jetstream.MsgIDHeader: []string{e.ID}},
	})
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to publish to NATS")
	}
	return nil
}

// Subscribe creates a durable consumer. The handler MUST be idempotent:
// delivery is at-least-once.
func (n *NATS) Subscribe(ctx context.Context, stream, durable string, subjects []string, h ports.Handler) error {
	if stream == "" {
		stream = StreamName
	}
	cons, err := n.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:        durable,
		FilterSubjects: subjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		MaxDeliver:     MaxDeliver,
		AckWait:        30 * time.Second,
		BackOff:        []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, time.Minute},
	})
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to create consumer %q", durable)
	}

	log := logging.From(ctx).With("consumer", durable)
	_, err = cons.Consume(func(msg jetstream.Msg) {
		var env Envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			// An unreadable message never improves with a retry — discard it
			// with a record.
			log.Error("unreadable event, discarded", "error", err, "subject", msg.Subject())
			_ = msg.Term()
			return
		}
		// The handler receives the fields already unwrapped; Payload carries the
		// whole envelope, for whoever wants the raw data.
		e := eventFrom(env, msg.Data())
		if err := h(ctx, e); err != nil {
			md, _ := msg.Metadata()
			attempts := 1
			if md != nil {
				attempts = int(md.NumDelivered)
			}
			if attempts >= MaxDeliver {
				if dlqErr := n.publishDeadLetter(ctx, durable, e, err, attempts); dlqErr != nil {
					// NAK, not Term. Failing to record a loss must not cause the
					// loss: JetStream keeps the message and tries again. Losing a
					// redelivery is cheaper than losing the record of a loss.
					log.Error("failed to publish the dead letter; the event stays in the queue",
						"error", dlqErr, "event_id", e.ID)
					_ = msg.Nak()
					return
				}
				log.Error("event exhausted its attempts and went to the dead-letter queue",
					"error", err, "type", e.Type, "event_id", e.ID,
					"attempts", attempts, "subject", DLQSubject)
				_ = msg.Term()
				return
			}
			log.Warn("failed to process the event, it will be redelivered",
				"error", err, "type", e.Type, "event_id", e.ID, "attempt", attempts)
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to consume %q", durable)
	}
	return nil
}

// publishDeadLetter records a loss on the queue that exists to hold it.
//
// The dead letter is published through the SAME Publish an ordinary event
// uses, on DLQSubject, so it gets the same MsgId-based dedup and lands in the
// same stream (the dlq.> branch alongside dop.>, see NewNATS) with the same
// 30-day retention — no second code path to keep honest.
func (n *NATS) publishDeadLetter(ctx context.Context, consumer string, e ports.Event, cause error, attempts int) error {
	dl := buildDeadLetter(consumer, e, cause, attempts)
	// The dead letter's identity is the PAIR (event, consumer), not the event
	// alone: `timeline` subscribes to dop.> — every subject — so it overlaps
	// every other consumer on every subject those handle, and the SAME event
	// can fail in more than one of them. Publish's MsgId dedup is keyed by
	// this id; a bare e.ID would make the second consumer's dead letter
	// collide with the first's within JetStream's dedup window, and the
	// broker would drop it — one consumer's failure vanishing with no error
	// anywhere, the exact silent loss this task exists to remove, one layer
	// up. Widening the key to "<event id>:<consumer>" keeps the property
	// worth keeping (a retried publish of the SAME (event, consumer) failure
	// still dedups) while telling the two consumers' records apart.
	id := e.ID + ":" + consumer
	body, err := deadLetterEnvelope(id, e, dl)
	if err != nil {
		return err
	}
	return n.Publish(ctx, ports.Event{
		ID:        id,
		AccountID: e.AccountID,
		Aggregate: "dead_letter",
		Type:      DLQSubject,
		Payload:   body,
	})
}

func (n *NATS) Close() error {
	if n.conn != nil {
		return n.conn.Drain()
	}
	return nil
}

var _ ports.EventBus = (*NATS)(nil)

func StreamInfo(ctx context.Context, n *NATS) (string, error) {
	info, err := n.stream.Info(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("stream=%s msgs=%d bytes=%d consumers=%d",
		info.Config.Name, info.State.Msgs, info.State.Bytes, info.State.Consumers), nil
}
