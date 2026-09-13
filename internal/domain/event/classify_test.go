package event_test

import (
	"errors"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

func TestTheSeedComesFromTheKindAndNotFromAList(t *testing.T) {
	// A hand-kept list is a list somebody forgets to extend. errs.Kind already
	// answers the question — "does repeating this call change the answer?" —
	// so the seed is derived from it.
	cases := map[errs.Kind]event.Classification{
		errs.KindUnavailable:   event.Recoverable,
		errs.KindInternal:      event.Unknown,
		errs.KindNotFound:      event.Irrecoverable,
		errs.KindInvalid:       event.Irrecoverable,
		errs.KindPermission:    event.Irrecoverable,
		errs.KindUnauthorized:  event.Irrecoverable,
		errs.KindPrecondition:  event.Irrecoverable,
		errs.KindConflict:      event.Irrecoverable,
		errs.KindAlreadyExists: event.Irrecoverable,
	}
	for kind, want := range cases {
		if got := event.Classify(errs.New(kind, "whatever")); got != want {
			t.Errorf("%s classified as %s, expected %s", kind, got, want)
		}
	}
}

func TestAnErrorThatIsNotOursIsUnknownAndNotIrrecoverable(t *testing.T) {
	// A plain error carries no kind. Calling it irrecoverable would stop
	// retrying something that might just have been a blip, and silently.
	if got := event.Classify(errors.New("a bare error")); got != event.Unknown {
		t.Fatalf("a bare error classified as %s, expected unknown", got)
	}
}

func TestNoErrorIsNotAFailure(t *testing.T) {
	if got := event.Classify(nil); got != event.Recoverable {
		t.Fatalf("nil classified as %s", got)
	}
}
