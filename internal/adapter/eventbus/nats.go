// An EventBus adapter over NATS JetStream (ADR-0019).
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

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

const (
	StreamName    = "DOP"
	StreamSubject = "dop.>"
	// After this many attempts the message goes to the DLQ instead of blocking
	// the queue forever.
	MaxDeliver = 5
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
		Name:      StreamName,
		Subjects:  []string{StreamSubject},
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
			if md != nil && md.NumDelivered >= MaxDeliver {
				log.Error("event exhausted its attempts, going to the DLQ",
					"error", err, "type", e.Type, "event_id", e.ID,
					"attempts", md.NumDelivered)
				_ = msg.Term()
				return
			}
			log.Warn("failed to process the event, it will be redelivered",
				"error", err, "type", e.Type, "event_id", e.ID)
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
