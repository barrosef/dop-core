package eventbus

import (
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// TestDeadLetterFromIsTheInverseOfDeadLetterEnvelope pins the pairing between
// the encoder (deadLetterEnvelope, unexported) and the exported decoder every
// consumer of a dead letter now shares. Before DeadLetterFrom existed, this
// unwrap was written out by hand in three places (DLQConsumer.Handle and two
// contract test cases) — nothing forced them to agree with the encoder.
func TestDeadLetterFromIsTheInverseOfDeadLetterEnvelope(t *testing.T) {
	e := ports.Event{ID: "ev-1", AccountID: "acct-1", AggregateID: "ag-1"}
	dl := buildDeadLetter("notification", e, errs.New(errs.KindUnavailable, "boom"), 5)

	id := e.ID + ":notification"
	body, err := deadLetterEnvelope(id, e, dl)
	if err != nil {
		t.Fatalf("deadLetterEnvelope: %v", err)
	}

	got, err := DeadLetterFrom(ports.Event{ID: id, Payload: body})
	if err != nil {
		t.Fatalf("DeadLetterFrom: %v", err)
	}
	if got.Event.ID != e.ID || got.Consumer != "notification" {
		t.Fatalf("the record did not round-trip: %+v", got)
	}
	if got.BrokerAttempts != 5 {
		t.Fatalf("BrokerAttempts did not round-trip: got %d, want 5", got.BrokerAttempts)
	}
}

// TestPortsEventHasStableWireFieldNames pins the fix for a dead letter whose
// nested `event` field serialized under Go's field names instead of a name
// that survives a rename: the dead letter's envelope holds this struct as-is
// for 30 days, and a bare struct-name encoding would silently break every
// record already on the queue the day ActorID gets renamed.
func TestPortsEventHasStableWireFieldNames(t *testing.T) {
	dl := event.DeadLetter{Event: ports.Event{ID: "ev-1", ActorID: "u-1"}, Consumer: "notification"}
	body, err := deadLetterEnvelope("ev-1:notification", ports.Event{ID: "ev-1"}, dl)
	if err != nil {
		t.Fatalf("deadLetterEnvelope: %v", err)
	}
	if !strings.Contains(string(body), `"actor_id":"u-1"`) {
		t.Fatalf("the nested event did not use its stable json tag: %s", body)
	}
	if strings.Contains(string(body), `"ActorID"`) {
		t.Fatalf("the nested event serialized under the Go field name: %s", body)
	}
}

// TestBuildDeadLetterFallsBackToKindWhenTheErrorHasNoCode pins the fix for a
// signature key that used to collapse to (consumer, ""): the notifier,
// projection and notification adapters this feature classifies have ZERO
// WithCode call sites, so errs.CodeOf alone left every real failure keyed by
// an empty code — one row per consumer, not one per failure mode.
func TestBuildDeadLetterFallsBackToKindWhenTheErrorHasNoCode(t *testing.T) {
	cause := errs.New(errs.KindUnavailable, "the provider is down")

	dl := buildDeadLetter("notification", ports.Event{ID: "ev-1"}, cause, 5)

	if len(dl.Attempts) != 1 {
		t.Fatalf("expected one attempt, got %d", len(dl.Attempts))
	}
	got := dl.Attempts[0].ErrorCode
	if got == "" {
		t.Fatal("the code fell back to empty — every uncoded failure of this consumer " +
			"now shares a single signature row")
	}
	if got != string(errs.KindUnavailable) {
		t.Fatalf("expected the fallback to be the error's Kind (%q), got %q", errs.KindUnavailable, got)
	}
}

// TestBuildDeadLetterKeepsTheExplicitCodeWhenPresent makes sure the fallback
// only fires when there is nothing more specific: a real Code must not be
// discarded in favor of the coarser Kind.
func TestBuildDeadLetterKeepsTheExplicitCodeWhenPresent(t *testing.T) {
	cause := errs.New(errs.KindUnavailable, "boom").WithCode("mail.provider_down", nil)

	dl := buildDeadLetter("notification", ports.Event{ID: "ev-1"}, cause, 5)

	if got := dl.Attempts[0].ErrorCode; got != "mail.provider_down" {
		t.Fatalf("got %q, want the explicit code untouched", got)
	}
}
