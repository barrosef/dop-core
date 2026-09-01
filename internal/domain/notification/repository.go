package notification

import (
	"context"
	"time"
)

// DeliveryKey is the composite idempotency key (ADR-0025). It exists as a type
// so that no caller can assemble half a key.
type DeliveryKey struct {
	EventID string
	Rule    string
	Action  Action
}

// Claim is the RESERVATION of one action execution.
//
// The claim happens BEFORE the send, and that ordering is the whole design of
// the idempotency: claiming after sending leaves a window in which JetStream's
// redelivery sends the second email. Claiming first closes the window and, at
// worst, loses a notice that stays RECORDED — the correct asymmetry, because a
// duplicate email is visible to the user and has no undo.
type Claim struct {
	DeliveryKey
	AccountID  string
	Kind       Kind
	Channel    string
	Recipients []string
}

// Outcome is ONE message's outcome, covering the keys it served.
//
// Keys is plural because the digest groups: N box items become one email.
// Idempotency stays per (event, rule, action) — one row per item — and the
// grouping shows up in the `batch_id` the repository stamps.
type Outcome struct {
	AccountID  string
	Keys       []DeliveryKey
	Kind       Kind
	Recipients []string
	State      State
	Provider   string
	Reference  string
	// Error is the failure message ALREADY REDACTED by the adapter (the port's
	// guarantee 4). The domain redacts nothing: it does not know what the secret
	// is.
	Error string
}

// Repository is the trigger's persistence PORT.
//
// There is no loose `Create` and no loose `Update`: the only two writes are
// CLAIM and SETTLE, because those are the only two things that happen. A port
// with a generic write would be an invitation to record a send by hand, and the
// record would stop reflecting what actually went out.
type Repository interface {
	// Claim reserves the key. Returns `true` when THIS call took it.
	//
	// Returns `false` — with no error — when the key has already been served:
	// that is redelivery's normal path, and turning it into an error would make
	// every redelivered message look like a defect. A claim in StateError is
	// RESUMED (and `attempts` goes up), because a send failure is the only
	// situation in which retrying is right.
	Claim(ctx context.Context, c Claim, maxAttempts int) (bool, error)

	// Settle records the keys' outcome and emits the event in the SAME
	// transaction. It returns the stamped `batch_id`, which is what ties the rows
	// to the message.
	Settle(ctx context.Context, o Outcome) (string, error)

	// Recipients returns who receives the account's notices: members with a known
	// address. An empty list is a legitimate answer — an account whose members
	// signed in by phone or through SSO without profile scope has no address.
	Recipients(ctx context.Context, accountID string) ([]Recipient, error)

	// AccountsWithRipeAttention says WHICH accounts have a mature item waiting
	// for a notice. It exists separately from RipeAttention for the same reason
	// as the sandbox sweeper: the scheduler has no active account, and the way
	// out is to visit account by account, each visit with that account in the
	// context — isolation is not loosened, only who decides the visiting order
	// changes.
	AccountsWithRipeAttention(ctx context.Context, rule string, action Action, olderThan time.Time, maxAttempts int) ([]string, error)

	// RipeAttention returns the active account's items that have been OPEN for
	// longer than the delay and have not yet become a notice.
	//
	// "Still open" is the entire rule of the delay: an item resolved before the
	// cutoff simply does not appear here, and therefore never becomes an email.
	// There is no scheduler and no cancellation — there is a query that only sees
	// what survived the wait.
	RipeAttention(ctx context.Context, accountID, rule string, action Action, olderThan time.Time, maxAttempts, limit int) ([]AttentionNotice, error)
}
