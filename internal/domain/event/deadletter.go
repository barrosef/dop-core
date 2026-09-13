package event

import (
	"context"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// Attempt is one failure, as it happened.
type Attempt struct {
	At           time.Time `json:"at"`
	ErrorKind    string    `json:"error_kind"`
	ErrorCode    string    `json:"error_code"`
	ErrorMessage string    `json:"error_message"`
}

// DeadLetter is what a consumer could not process, with everything needed to
// try again — in ONE payload.
//
// The whole event travels, context included, and not a reference to it: a
// reference would make the retry depend on a second read at the worst possible
// moment, and on that read still answering the same thing.
//
// ── On the frozen plan ──────────────────────────────────────────────────────
//
// P-29 §6 describes this record as carrying the plan `Decide` produced —
// {event, rule_ref, action_name, params}. The reasoning is exact and stands:
// rules are data, data changes, and a rule edited between the failure and the
// retry would make the retry execute something DIFFERENT from what failed.
//
// `rule_ref` and `action_name` do not exist yet — there is no dispatcher, and
// the consumers are wired by hand. So this freezes what exists, {event,
// consumer}, and the two fields are added when the dispatcher lands. They are
// additive: the unit of retry is the same in both designs — one event, one
// handler. The dispatcher changes who decides WHICH handler, not what is
// retried.
type DeadLetter struct {
	Event    ports.Event `json:"event"`
	Consumer string      `json:"consumer"`
	Attempts []Attempt   `json:"attempts"`
	// Classification at the moment of routing. Recorded rather than recomputed
	// so the record says what was decided, not what would be decided today.
	Classification string    `json:"classification"`
	FirstFailedAt  time.Time `json:"first_failed_at"`
	LastFailedAt   time.Time `json:"last_failed_at"`
	// BrokerAttempts is how many times the BROKER delivered the event before
	// giving up on it — JetStream's MaxDeliver, or the in-memory adapter's
	// equivalent counter. Attempts above only holds what THIS delivery
	// witnessed (neither adapter keeps earlier failures around), so without
	// this field a reader of event_errors sees up to dlqRetries entries for an
	// event the broker actually tried MaxDeliver+dlqRetries times, with
	// nothing saying the broker burned its budget first.
	BrokerAttempts int `json:"broker_attempts"`
}

// ErrorStore holds what nobody could process. Terminal: whatever reaches it has
// already burned the retries and the DLQ rounds.
type ErrorStore interface {
	Record(ctx context.Context, dl DeadLetter, final Classification) error
}
