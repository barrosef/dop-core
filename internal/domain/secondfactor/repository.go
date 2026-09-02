package secondfactor

import (
	"context"
	"time"
)

// Repository is the second factor's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
type Repository interface {
	// Factors
	ListFactors(ctx context.Context, userID string) ([]Factor, error)
	FactorByID(ctx context.Context, id string) (*Factor, error)
	CreateFactor(ctx context.Context, f *Factor) (*Factor, error)
	// ActivateFactor is the confirmation: it stamps confirmed_at and moves it to
	// active. It is separate from an update because it is the transition that
	// matters, and one function that could do both would let a caller activate
	// without confirming.
	ActivateFactor(ctx context.Context, id string, at time.Time) (*Factor, error)
	RevokeFactor(ctx context.Context, id string, at time.Time) error
	TouchFactor(ctx context.Context, id string, at time.Time) error

	// Challenges
	CreateChallenge(ctx context.Context, c *Challenge) (*Challenge, error)
	ChallengeByID(ctx context.Context, id string) (*Challenge, error)
	// RegisterAttempt increments the counter and, when the answer is right,
	// consumes the challenge — in ONE operation. Two calls would leave the
	// window where an attempt is not yet counted, which is the window a script
	// uses.
	RegisterAttempt(ctx context.Context, id string, consumed bool, at time.Time) (*Challenge, error)
	// ChallengesSince answers "may I send another one?" in one call.
	//
	// `count` is EVERY challenge in the window — the ceiling is about cost, and
	// a consumed message cost the same as an unanswered one. `lastPending` is
	// the most recent challenge NOT yet consumed, and the resend floor hangs off
	// it: the floor exists so a second click does not send a second message
	// while the first is still in flight. Once a code has been used, asking for
	// another is legitimate — and making the person wait would be charging them
	// for having succeeded.
	ChallengesSince(ctx context.Context, factorID string, since time.Time) (count int, lastPending time.Time, err error)

	// Recovery codes
	ReplaceRecoveryCodes(ctx context.Context, userID string, hashes [][]byte, at time.Time) error
	// UseRecoveryCode consumes a code by its hash and answers whether it was
	// still unused. It is an atomic read-and-consume for the same reason as
	// RegisterAttempt.
	UseRecoveryCode(ctx context.Context, userID string, hash []byte, at time.Time) (bool, error)
	CountUnusedRecoveryCodes(ctx context.Context, userID string) (int, error)

	// Step-up
	SaveStepUp(ctx context.Context, s StepUp) error
	StepUpFor(ctx context.Context, userID, sessionID string) (*StepUp, error)
	DeleteStepUpsOf(ctx context.Context, userID string) error

	// Policy — the account's, read from the accounts table. It lives in this
	// port and not in identity's because it is THIS domain's question: identity
	// answers who the person is, not which factors the account accepts.
	PolicyOf(ctx context.Context, accountID string) (Policy, error)
}

// Policy is the account's requirement (ADR-0027 §7).
type Policy struct {
	Required bool
	// Allowed is which kinds this account accepts. Empty means "the platform's
	// default" — never "none": an account with no accepted kind would be an
	// account nobody can get into.
	Allowed []Kind
}

// Accepts answers whether the account takes this kind. The empty list is the
// default (all three), decided here and not at the call site so that the
// question has ONE answer.
func (p Policy) Accepts(k Kind) bool {
	if len(p.Allowed) == 0 {
		return ValidKind(k)
	}
	for _, a := range p.Allowed {
		if a == k {
			return true
		}
	}
	return false
}
