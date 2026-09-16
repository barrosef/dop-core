package app

import (
	"context"

	"github.com/barrosef/dop-core/internal/adapter/eventbus"
	"github.com/barrosef/dop-core/internal/domain/event"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/logging"
)

// dlqRetries is how many more times the dead-letter consumer tries, ON TOP of
// the five JetStream already spent. Small on purpose: past this, a person or an
// agent decides, and the table is where they see it.
const dlqRetries = 3

// DLQConsumer re-executes what failed, through the SAME registry the dispatcher
// uses — it has no logic of its own about what an action means.
//
// That is not tidiness: rules are data and data changes, so a consumer that
// decided again could execute something different from what failed. It calls
// the handler the record names, with the event the record froze.
type DLQConsumer struct {
	handlers map[string]ports.Handler
	sigs     event.SignatureStore
	errors   event.ErrorStore
	clock    ports.Clock
}

func NewDLQConsumer(handlers map[string]ports.Handler, sigs event.SignatureStore,
	errors event.ErrorStore, clock ports.Clock) *DLQConsumer {
	return &DLQConsumer{handlers: handlers, sigs: sigs, errors: errors, clock: clock}
}

// Handle processes one dead letter.
//
// It returns nil in every terminal case, INCLUDING the failures — a returned
// error would put the record back on the queue, and the queue is not where an
// unprocessable record belongs. What it returns an error for is its own
// inability to read or record against the signature store, which deserves a
// redelivery.
func (c *DLQConsumer) Handle(ctx context.Context, e ports.Event) error {
	log := logging.From(ctx).With("dlq_event_id", e.ID)

	// e.Payload is the WIRE ENVELOPE, like every consumer receives — never the
	// inner payload directly (see eventbus.Envelope and eventFrom). Task 3's
	// publishDeadLetter wraps the DeadLetter JSON inside that envelope's own
	// `payload` field precisely so a bare, field-less object here does not
	// decode into a zero-valued DeadLetter with no error at all — the exact
	// silent failure this component exists to stop. eventbus.DeadLetterFrom is
	// the ONE place that unwraps it: this used to be two json.Unmarshal calls
	// written out by hand here, a second copy of the shape publishDeadLetter
	// already encodes, and a copy that could silently drift from it.
	dl, err := eventbus.DeadLetterFrom(e)
	if err != nil {
		// Terminal by nature: no retry improves broken JSON. Logged and
		// dropped, never redelivered.
		log.Error("unreadable dead letter, discarded", "error", err)
		return nil
	}

	code := ""
	if n := len(dl.Attempts); n > 0 {
		code = dl.Attempts[n-1].ErrorCode
		if code == "" {
			// The producer (eventbus.buildDeadLetter) now falls back to the
			// error's Kind itself, so a fresh record never gets here empty.
			// This is defense for a record built before that fix: an empty
			// code would still collapse the signature key to (consumer, ""),
			// so fall back to the recorded Kind rather than trust CodeOf on
			// an error object we no longer have — only the two strings this
			// record froze survive the wire.
			code = dl.Attempts[n-1].ErrorKind
		}
	}
	// decisionCode is the identity of the row this delivery decided against —
	// captured ONCE, here, and used for every write this Handle makes to the
	// signature store. The loop below reassigns `code` as each retry fails
	// with its own code, which is right for the record's attempt history but
	// wrong as a store key: error_signatures is unique on (consumer, code), so
	// crediting a success or an exhaustion to whatever code failed LAST would
	// write it to a different row than the one `Get`/`Decide` just consulted —
	// evidence landing on a signature that was never the one classified.
	decisionCode := code

	seed := event.Classification(dl.Classification)
	if seed == "" {
		seed = event.Unknown
	}
	var sig event.Signature
	stored, err := c.sigs.Get(ctx, dl.Consumer, decisionCode)
	if err != nil {
		// The store being down is not the event's fault: ask for a redelivery.
		return err
	}
	if stored != nil {
		sig = *stored
	}
	final := event.Decide(sig, seed)

	handler, known := c.handlers[dl.Consumer]
	if !known {
		// A consumer that no longer exists — renamed, removed. Looping on it
		// would spend a budget on something nothing can handle.
		log.Warn("dead letter names a consumer that does not exist", "consumer", dl.Consumer)
		return c.errors.Record(ctx, dl, final)
	}
	if final == event.Irrecoverable {
		// Retrying what cannot work spends the budget that belongs to what
		// can, and pushes the real failure hours away from the table where
		// somebody sees it.
		log.Info("irrecoverable failure, not retried", "consumer", dl.Consumer, "code", code)
		return c.errors.Record(ctx, dl, final)
	}

	for attempt := 1; attempt <= dlqRetries; attempt++ {
		err := handler(ctx, dl.Event)
		if err == nil {
			// Evidence against the mark, and it is recorded even when the mark
			// was right until now — against decisionCode, the row that was
			// actually decided against, not whatever code the last failure
			// (if any) happened to carry.
			if sErr := c.sigs.RecordSuccess(ctx, dl.Consumer, decisionCode); sErr != nil {
				log.Warn("the retry worked and the success was not recorded", "error", sErr)
			}
			log.Info("dead letter recovered", "consumer", dl.Consumer, "attempt", attempt)
			return nil
		}
		// Same fallback as the producer (eventbus.buildDeadLetter): a retry's
		// own attempt record must not go back to an empty code either.
		failedCode := errs.CodeOrKind(err)
		dl.Attempts = append(dl.Attempts, event.Attempt{
			At:           c.clock.Now(),
			ErrorKind:    string(errs.KindOf(err)),
			ErrorCode:    failedCode,
			ErrorMessage: err.Error(),
		})
		dl.LastFailedAt = c.clock.Now()
		code = failedCode
	}

	// RecordExhausted also targets decisionCode, for the same reason RecordSuccess
	// does above — `code` at this point is the LAST attempt's code, which may
	// differ from what was decided against.
	if err := c.sigs.RecordExhausted(ctx, dl.Consumer, decisionCode, seed); err != nil {
		log.Warn("the exhaustion was not recorded against the signature", "error", err)
	}
	log.Error("dead letter exhausted the recovery budget", "consumer", dl.Consumer, "code", code)
	return c.errors.Record(ctx, dl, final)
}
