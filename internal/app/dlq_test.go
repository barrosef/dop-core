package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
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

// TestASuccessAfterDifferingFailureCodesIsRecordedAgainstTheDecidedCode pins
// the fix for a drift bug: the code used to DECIDE (read from the incoming
// dead letter's last attempt, and the one Get/Decide consulted) must be the
// same code the outcome is recorded against — even when the retries in
// between fail with codes OF THEIR OWN. error_signatures is unique on
// (consumer, code); crediting the eventual success to whatever code the last
// retry happened to fail with, instead of the code that was decided against,
// writes the evidence to a row nobody read and leaves the real one to keep
// climbing toward promotion on stale history.
func TestASuccessAfterDifferingFailureCodesIsRecordedAgainstTheDecidedCode(t *testing.T) {
	var calls int
	handler := func(context.Context, ports.Event) error {
		calls++
		switch calls {
		case 1:
			return errs.New(errs.KindUnavailable, "timeout").WithCode("mail.timeout", nil)
		case 2:
			return errs.New(errs.KindUnavailable, "rate limited").WithCode("mail.rate_limited", nil)
		default:
			return nil
		}
	}
	sigs := &stubSignatures{}
	errors := &stubErrors{}
	c := NewDLQConsumer(map[string]ports.Handler{"notification": handler}, sigs, errors, stubClock{})

	dl := event.DeadLetter{Event: ports.Event{ID: "ev-1"}, Consumer: "notification",
		Classification: string(event.Recoverable),
		Attempts:       []event.Attempt{{ErrorCode: "mail.timeout"}}}
	if err := c.Handle(context.Background(), deadLetterEvent(t, dl)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if calls != 3 {
		t.Fatalf("expected 2 failures then a success (3 calls), got %d", calls)
	}
	if sigs.getCode != "mail.timeout" {
		t.Fatalf("Get was called with %q, want the decided code %q", sigs.getCode, "mail.timeout")
	}
	if sigs.successCode != "mail.timeout" {
		t.Errorf("RecordSuccess was called with %q, want the decided code %q — not %q, which is what the LAST retry failed with",
			sigs.successCode, "mail.timeout", "mail.rate_limited")
	}
	if errors.recorded != 0 {
		t.Error("a successful retry still reached the terminal table")
	}
}

// TestADeadLetterWithNoCodeStillProducesANonEmptyKey pins the fix for a
// signature key that used to collapse to (consumer, ""): a dead letter built
// before eventbus.buildDeadLetter's fallback existed (or corrupted in
// transit) may still carry an empty ErrorCode. Get/RecordExhausted/
// RecordSuccess must never be called with "" — that key is shared by every
// uncoded failure of the consumer.
func TestADeadLetterWithNoCodeStillProducesANonEmptyKey(t *testing.T) {
	sigs := &stubSignatures{}
	errors := &stubErrors{}
	c := NewDLQConsumer(map[string]ports.Handler{
		"notification": func(context.Context, ports.Event) error {
			return errs.New(errs.KindUnavailable, "still down")
		},
	}, sigs, errors, stubClock{})

	dl := event.DeadLetter{Event: ports.Event{ID: "ev-1"}, Consumer: "notification",
		Classification: string(event.Recoverable),
		Attempts:       []event.Attempt{{ErrorKind: string(errs.KindUnavailable)}}}
	if err := c.Handle(context.Background(), deadLetterEvent(t, dl)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if sigs.getCode == "" {
		t.Fatal("Get was called with an empty code — the signature key collapsed to (consumer, \"\")")
	}
	if sigs.getCode != string(errs.KindUnavailable) {
		t.Fatalf("expected the fallback to be the recorded Kind (%q), got %q", errs.KindUnavailable, sigs.getCode)
	}
	if sigs.exhaustedCode != sigs.getCode {
		t.Fatalf("RecordExhausted used %q, want the same decided code %q", sigs.exhaustedCode, sigs.getCode)
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

	// getCode, successCode and exhaustedCode record the LAST code each method
	// was called with — enough to pin that every call against one dead letter
	// targets the same (consumer, code) row, however many differing codes the
	// retries in between failed with.
	getCode       string
	successCode   string
	exhaustedCode string
}

func (s *stubSignatures) Get(_ context.Context, _ string, code string) (*event.Signature, error) {
	s.getCode = code
	return s.sig, nil
}
func (s *stubSignatures) RecordExhausted(_ context.Context, _ string, code string, _ event.Classification) error {
	s.exhausted++
	s.exhaustedCode = code
	return nil
}
func (s *stubSignatures) RecordSuccess(_ context.Context, _ string, code string) error {
	s.successes++
	s.successCode = code
	return nil
}

type stubErrors struct{ recorded int }

func (s *stubErrors) Record(context.Context, event.DeadLetter, event.Classification) error {
	s.recorded++
	return nil
}

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) }
