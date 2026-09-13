package eventbus

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

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
