package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// EventBusSuite verifies the nine guarantees documented on the port.
//
// Guarantee 1 is the reason this suite exists: the wire format is the ENVELOPE,
// and an adapter that re-serializes ports.Event sends Payload []byte in base64.
// The consumer does not recognize it, discards everything and DOES NOT COMPLAIN
// — the system goes mute and green at the same time. Only a test that compares
// the BYTES catches that.
//
// Every subject and every event ID is unique per subtest: against a real broker
// the suite shares the stream with earlier runs, and JetStream deduplicates by
// ID.
func EventBusSuite(t *testing.T, name string, newBus func(t *testing.T) ports.EventBus) {
	t.Run(name, func(t *testing.T) {
		t.Run("1_payload_arrives_byte_for_byte_and_fields_come_from_the_envelope", func(t *testing.T) {
			bus := newBus(t)
			subject := uniqueSubject("intacto")
			c := newCollector()
			subscribe(t, bus, subject, c.handler)

			data := envelopeJSON(uniqueEventID(), subject, `{"quantity":42,"text":"acentuação"}`)
			publish(t, bus, subject, data)

			c.waitFor(t, 1, "the published event never arrived")
			got := c.events()[0]

			if !bytes.Equal(got.Payload, data) {
				t.Fatalf("the envelope's bytes did not survive the transport.\n"+
					"published: %s\nrecebido:  %s\n"+
					"(a base64 payload on the other side = somebody re-serialized ports.Event)",
					data, got.Payload)
			}
			var published, recebido eventbus.Envelope
			_ = json.Unmarshal(data, &published)
			if err := json.Unmarshal(got.Payload, &recebido); err != nil {
				t.Fatalf("the delivered payload is not the JSON envelope: %v", err)
			}
			if string(recebido.Payload) != string(published.Payload) {
				t.Fatalf("the business data inside the envelope changed: %s != %s",
					recebido.Payload, published.Payload)
			}
			// The delivered event's fields come from the envelope, not from the published struct.
			if got.ID != published.ID || got.AccountID != published.AccountID ||
				got.Aggregate != published.Aggregate || got.AggregateID != published.AggregateID ||
				got.Type != published.Type {
				t.Fatalf("the unwrapped fields diverge from the envelope:\nreceived: %+v\nenvelope: %+v", got, published)
			}
			if !got.OccurredAt.Equal(published.OccurredAt) {
				t.Errorf("OccurredAt divergente: %v != %v", got.OccurredAt, published.OccurredAt)
			}
		})

		t.Run("2_publish_with_no_payload_preserves_the_identification", func(t *testing.T) {
			bus := newBus(t)
			ctx := context.Background()
			subject := uniqueSubject("no-payload")
			c := newCollector()
			subscribe(t, bus, subject, c.handler)

			e := ports.Event{
				ID: uniqueEventID(), AccountID: "acct-1", Aggregate: "account",
				AggregateID: "ag-1", Type: subject, OccurredAt: nowJSON(),
			}
			if err := bus.Publish(ctx, e); err != nil {
				t.Fatalf("Publish: %v", err)
			}

			c.waitFor(t, 1, "an event with no payload never arrived")
			got := c.events()[0]
			// This is where json.Marshal(ports.Event) gives itself away: the
			// struct's field names do not match the envelope's and everything
			// arrives empty.
			if got.ID != e.ID || got.AccountID != e.AccountID ||
				got.Aggregate != e.Aggregate || got.AggregateID != e.AggregateID || got.Type != e.Type {
				t.Fatalf("identification lost along the way:\nreceived: %+v\npublished: %+v", got, e)
			}
			var env eventbus.Envelope
			if err := json.Unmarshal(got.Payload, &env); err != nil {
				t.Fatalf("the fallback should build a JSON envelope, got: %s", got.Payload)
			}
		})

		t.Run("3_filters_by_subject", func(t *testing.T) {
			bus := newBus(t)
			base := uniqueSubject("filtro")
			alvo := base + ".alvo"
			outro := base + ".outro"
			c := newCollector()
			subscribe(t, bus, base+".alvo.>", c.handler)

			publish(t, bus, outro+".um", envelopeJSON(uniqueEventID(), outro+".um", `{}`))
			publish(t, bus, alvo+".um", envelopeJSON(uniqueEventID(), alvo+".um", `{}`))

			c.waitFor(t, 1, "the event of the subscribed subject never arrived")
			// Room for the intruder to show up, if the filter has a hole.
			time.Sleep(500 * time.Millisecond)
			for _, e := range c.events() {
				if e.Type != alvo+".um" {
					t.Fatalf("an event of an unsubscribed subject arrived: %q", e.Type)
				}
			}
		})

		t.Run("4_delivers_what_was_published_before_the_subscription", func(t *testing.T) {
			bus := newBus(t)
			subject := uniqueSubject("retido")
			id := uniqueEventID()

			// It publishes BEFORE a subscriber exists: it is the real order when
			// the relay comes up before the projection worker.
			publish(t, bus, subject, envelopeJSON(id, subject, `{}`))

			c := newCollector()
			subscribe(t, bus, subject, c.handler)
			c.waitFor(t, 1, "the event published before the subscription was lost — "+
				"a espinha de events dependeria da ordem de boot")
			if got := c.events()[0].ID; got != id {
				t.Fatalf("another event arrived: %q != %q", got, id)
			}
		})

		t.Run("5_a_handler_error_causes_a_redelivery", func(t *testing.T) {
			bus := newBus(t)
			subject := uniqueSubject("reentrega")
			var tentativas atomic.Int64
			subscribe(t, bus, subject, func(_ context.Context, e ports.Event) error {
				if tentativas.Add(1) == 1 {
					return fmt.Errorf("falha proposital na primeira entrega")
				}
				return nil
			})

			publish(t, bus, subject, envelopeJSON(uniqueEventID(), subject, `{}`))
			waitUntil(t, 30*time.Second, func() bool { return tentativas.Load() >= 2 },
				"the handler failed and the event was NOT redelivered — at-least-once delivery broken")
		})

		t.Run("6_success_does_not_cause_an_immediate_redelivery", func(t *testing.T) {
			bus := newBus(t)
			subject := uniqueSubject("ack")
			c := newCollector()
			subscribe(t, bus, subject, c.handler)

			publish(t, bus, subject, envelopeJSON(uniqueEventID(), subject, `{}`))
			c.waitFor(t, 1, "the event never arrived")
			// A short window: it catches the adapter that redelivers in a hot
			// loop. A late redelivery (JetStream's AckWait) is out of a fast
			// test's reach, and the port allows duplicates anyway.
			time.Sleep(1 * time.Second)
			if n := len(c.events()); n > 1 {
				t.Fatalf("a successful delivery was repeated %d times in 1s — the ack is not happening", n)
			}
		})

		t.Run("7_an_unreadable_message_does_not_block_the_queue", func(t *testing.T) {
			bus := newBus(t)
			subject := uniqueSubject("veneno")
			c := newCollector()
			subscribe(t, bus, subject, c.handler)

			// Bytes that are not JSON: no number of retries fixes that.
			publish(t, bus, subject, []byte("{this is not json"))
			good := uniqueEventID()
			publish(t, bus, subject, envelopeJSON(good, subject, `{}`))

			c.waitFor(t, 1, "the unreadable message blocked the queue: the next event did not arrive")
			if got := c.events()[0].ID; got != good {
				t.Fatalf("o handler recebeu algo inesperado: %q", got)
			}
		})

		t.Run("8_concurrent_publish", func(t *testing.T) {
			bus := newBus(t)
			ctx := context.Background()
			subject := uniqueSubject("concorrente")
			c := newCollector()
			subscribe(t, bus, subject, c.handler)

			const total = 50
			ids := make(map[string]bool, total)
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := 0; i < total; i++ {
				id := uniqueEventID()
				mu.Lock()
				ids[id] = true
				mu.Unlock()
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					if err := bus.Publish(ctx, ports.Event{
						ID: id, Type: subject, Payload: envelopeJSON(id, subject, `{}`),
					}); err != nil {
						t.Errorf("Publish concorrente: %v", err)
					}
				}(id)
			}
			wg.Wait()

			waitUntil(t, 30*time.Second, func() bool { return len(c.distinct()) >= total },
				fmt.Sprintf("of the %d events published in parallel, %d arrived", total, len(c.distinct())))
			// A duplicate is allowed (at-least-once); a loss is not.
			for id := range ids {
				if !c.distinct()[id] {
					t.Fatalf("event %s was lost", id)
				}
			}
		})

		t.Run("9_an_event_with_no_type_is_refused", func(t *testing.T) {
			bus := newBus(t)
			if err := bus.Publish(context.Background(), ports.Event{ID: uniqueEventID()}); err == nil {
				t.Fatal("an event with no Type has no subject to go to and should be refused")
			}
		})

		t.Run("10_close_finishes_with_no_error", func(t *testing.T) {
			bus := newBus(t)
			if err := bus.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// ── the suite's helpers ──────────────────────────────────────────────────────

var eventoSeq atomic.Int64

// uniqueSubject returns a subject under "dop." (JetStream's stream only accepts
// dop.>), unique per run: the stream is persistent and survives the test.
func uniqueSubject(label string) string {
	return fmt.Sprintf("dop.contrato.%s.%d-%d", label, time.Now().UnixNano(), eventoSeq.Add(1))
}

// uniqueEventID returns a fresh ID — JetStream deduplicates by ID within a
// window, so reusing an ID would make the second event vanish "on its own".
func uniqueEventID() string {
	return fmt.Sprintf("evt-%d-%d", time.Now().UnixNano(), eventoSeq.Add(1))
}

// nowJSON truncates to milliseconds: the instant has to survive JSON's RFC3339
// without losing equality on the way back.
func nowJSON() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func envelopeJSON(id, subject, payload string) []byte {
	b, err := json.Marshal(eventbus.Envelope{
		ID: id, AccountID: "acct-contrato", Aggregate: "contrato", AggregateID: "ag-" + id,
		Type: subject, Payload: json.RawMessage(payload), OccurredAt: nowJSON(),
	})
	if err != nil {
		panic(err)
	}
	return b
}

func subscribe(t *testing.T, bus ports.EventBus, subject string, h ports.Handler) {
	t.Helper()
	durable := fmt.Sprintf("contrato-%d-%d", time.Now().UnixNano(), eventoSeq.Add(1))
	if err := bus.Subscribe(context.Background(), "", durable, []string{subject}, h); err != nil {
		t.Fatalf("Subscribe(%s): %v", subject, err)
	}
}

func publish(t *testing.T, bus ports.EventBus, subject string, data []byte) {
	t.Helper()
	var env eventbus.Envelope
	_ = json.Unmarshal(data, &env)
	id := env.ID
	if id == "" {
		id = uniqueEventID()
	}
	if err := bus.Publish(context.Background(), ports.Event{ID: id, Type: subject, Payload: data}); err != nil {
		t.Fatalf("Publish(%s): %v", subject, err)
	}
}

type collector struct {
	mu   sync.Mutex
	vist []ports.Event
}

func newCollector() *collector { return &collector{} }

func (c *collector) handler(_ context.Context, e ports.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vist = append(c.vist, e)
	return nil
}

func (c *collector) events() []ports.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.Event(nil), c.vist...)
}

func (c *collector) distinct() map[string]bool {
	out := map[string]bool{}
	for _, e := range c.events() {
		out[e.ID] = true
	}
	return out
}

func (c *collector) waitFor(t *testing.T, n int, msg string) {
	t.Helper()
	waitUntil(t, 30*time.Second, func() bool { return len(c.events()) >= n }, msg)
}

// waitUntil replaces a fixed sleep: the delivery is asynchronous in both
// adapters, and a generous deadline with frequent checks is fast when it passes
// and clear when it fails.
func waitUntil(t *testing.T, deadline time.Duration, ok func() bool, msg string) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for {
		if ok() {
			return
		}
		if time.Now().After(limit) {
			t.Fatalf("%s (the %s deadline ran out)", msg, deadline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
