package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// deadLetterEvent builds the ports.Event a real subscription would deliver for
// one dead letter: the DeadLetter JSON wrapped in the SAME wire envelope
// eventbus.deadLetterEnvelope produces (nats.go), not the bare DeadLetter
// bytes. A bare DeadLetter as the payload would decode into an Envelope with
// every field empty and no error at all — the silent failure this consumer
// exists to stop — so the test must exercise the real shape, not the
// convenient one.
func deadLetterEvent(t *testing.T, dl event.DeadLetter) ports.Event {
	t.Helper()
	dlBody, err := json.Marshal(dl)
	if err != nil {
		t.Fatalf("marshal dead letter: %v", err)
	}
	envID := dl.Event.ID + ":" + dl.Consumer
	env := eventbus.Envelope{ID: envID, Type: eventbus.DLQSubject, Payload: dlBody}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return ports.Event{ID: envID, Type: eventbus.DLQSubject, Payload: body}
}

func TestAnIrrecoverableFailureIsNotRetried(t *testing.T) {
	// Retrying what cannot work spends the budget that belongs to what can, and
	// pushes the real failure hours away from the table where somebody sees it.
	var called bool
	sigs := &stubSignatures{sig: &event.Signature{
		Classification: event.Irrecoverable, ClassifiedBy: event.ByHuman,
	}}
	errors := &stubErrors{}
	c := NewDLQConsumer(map[string]ports.Handler{
		"notification": func(context.Context, ports.Event) error { called = true; return nil },
	}, sigs, errors, stubClock{})

	dl := event.DeadLetter{Event: ports.Event{ID: "ev-1"}, Consumer: "notification",
		Attempts: []event.Attempt{{ErrorCode: "mail.no_template"}}}
	if err := c.Handle(context.Background(), deadLetterEvent(t, dl)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if called {
		t.Error("an irrecoverable failure was retried")
	}
	if errors.recorded != 1 {
		t.Errorf("it did not reach the terminal table: %d records", errors.recorded)
	}
}

func TestARecoverableFailureIsRetriedAndASuccessIsRecorded(t *testing.T) {
	var called bool
	sigs := &stubSignatures{}
	errors := &stubErrors{}
	c := NewDLQConsumer(map[string]ports.Handler{
		"notification": func(context.Context, ports.Event) error { called = true; return nil },
	}, sigs, errors, stubClock{})

	dl := event.DeadLetter{Event: ports.Event{ID: "ev-1"}, Consumer: "notification",
		Classification: string(event.Recoverable),
		Attempts:       []event.Attempt{{ErrorCode: "mail.provider_down"}}}
	if err := c.Handle(context.Background(), deadLetterEvent(t, dl)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !called {
		t.Error("a recoverable failure was not retried")
	}
	if errors.recorded != 0 {
		t.Error("a successful retry still reached the terminal table")
	}
	if sigs.successes != 1 {
		t.Error("the success was not recorded against the signature")
	}
}

func TestAnUnknownConsumerGoesStraightToTheTable(t *testing.T) {
	// A consumer that no longer exists — renamed, removed — must not make the
	// DLQ loop on something nothing can handle.
	sigs := &stubSignatures{}
	errors := &stubErrors{}
	c := NewDLQConsumer(map[string]ports.Handler{}, sigs, errors, stubClock{})

	dl := event.DeadLetter{Event: ports.Event{ID: "ev-1"}, Consumer: "gone"}
	if err := c.Handle(context.Background(), deadLetterEvent(t, dl)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if errors.recorded != 1 {
		t.Errorf("it did not reach the terminal table: %d", errors.recorded)
	}
}

func TestAnUnreadableDeadLetterIsNotRedelivered(t *testing.T) {
	// Returning an error here would put the malformed record back on the queue
	// forever. It is terminal by nature: no retry improves broken JSON.
	c := NewDLQConsumer(map[string]ports.Handler{}, &stubSignatures{}, &stubErrors{}, stubClock{})

	err := c.Handle(context.Background(), ports.Event{ID: "ev-1", Payload: []byte(`{`)})

	if err != nil {
		t.Fatalf("an unreadable dead letter asked to be redelivered: %v", err)
	}
}

type stubSignatures struct {
	sig       *event.Signature
	exhausted int
	successes int
}

func (s *stubSignatures) Get(context.Context, string, string) (*event.Signature, error) {
	return s.sig, nil
}
func (s *stubSignatures) RecordExhausted(context.Context, string, string, event.Classification) error {
	s.exhausted++
	return nil
}
func (s *stubSignatures) RecordSuccess(context.Context, string, string) error {
	s.successes++
	return nil
}

type stubErrors struct{ recorded int }

func (s *stubErrors) Record(context.Context, event.DeadLetter, event.Classification) error {
	s.recorded++
	return nil
}

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) }
