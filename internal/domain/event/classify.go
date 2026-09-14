package event

import "github.com/Digital-Business-One/dop-core/internal/platform/errs"

// Classification says whether repeating a failed call is worth anything.
//
// Retrying a permanently broken action across two budgets is waste with a delay
// attached, and it pushes a real failure hours away from the table where
// somebody would see it. So a failure is classified BEFORE it is retried.
type Classification string

const (
	// Recoverable — the world was busy; the same call may work later.
	Recoverable Classification = "recoverable"
	// Irrecoverable — the input is wrong; the same call gives the same answer.
	Irrecoverable Classification = "irrecoverable"
	// Unknown — it may be a bug or a blip. Repetition decides, and until it
	// does, the budget is spent on it.
	Unknown Classification = "unknown"
)

// Classify derives the seed from errs.Kind.
//
// DERIVED, not a hand-kept list: a list is a list somebody forgets to extend,
// and the kind already encodes the only question that matters here — does
// repeating this call change the answer? A template id that does not exist
// arrives as NotFound and is born irrecoverable with nobody registering
// anything.
//
// This is only the SEED. The signatures table learns from repetition and a
// human can overrule both.
func Classify(err error) Classification {
	if err == nil {
		return Recoverable
	}
	switch errs.KindOf(err) {
	case errs.KindUnavailable:
		return Recoverable
	case errs.KindNotFound, errs.KindInvalid, errs.KindPermission,
		errs.KindUnauthorized, errs.KindPrecondition, errs.KindConflict,
		errs.KindAlreadyExists:
		return Irrecoverable
	default:
		// KindInternal and anything errs cannot classify. Calling it
		// irrecoverable would stop retrying a blip, and silently.
		return Unknown
	}
}
