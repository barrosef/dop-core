// Package cost is the domain of LLM spend governance: how much was spent, how
// much may be spent, and which model serves each kind of work (ADR-0008).
//
// House rule: this package knows nothing of Postgres, gRPC or any SDK. It
// declares what it needs as a PORT (repository.go) and the composition root
// wires it.
//
// ADR-0008 has two firm parts and one still in draft, and the code separates the
// three on purpose: measurement and budget are rules (entity.go, service.go);
// the task→model policy is an informed guess and lives alone in router.go, so it
// can be recalibrated in one place when telemetry arrives (P-7).
package cost

import (
	"time"

	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Micros is a monetary value in 10^-6 of the currency's unit.
//
// Money is NOT a float here, and the reason is arithmetic, not style: summing
// millions of rows in binary floating point accumulates error, and a budget that
// is wrong is not a budget. An integer in the smallest unit is exact by
// construction, compares and adds without surprise, and it is what the contract
// already speaks (Money.amount_micros, api/proto/dop/v1/common.proto) —
// converting at the edge would create two vocabularies of money in one system.
//
// Micros, and not cents, because a single output token costs a fraction of a
// cent: in cents, every individual record would round to zero and the total
// would be systematically lower than the invoice.
type Micros int64

// The currency ALWAYS travels with the value. A bare Micros invites adding
// dollars to another currency and discovering it on the invoice.
const DefaultCurrency = "USD"

// Scope is the budget's unit. There are two, on purpose: the per-thread slice
// comes from the subagent's brief (ADR-0007), it is not a budget of its own.
type Scope string

const (
	ScopeAccount Scope = "account"
	ScopeDemand  Scope = "demand"
)

func ValidScope(s Scope) bool { return s == ScopeAccount || s == ScopeDemand }

// UsageEvent is ONE model consumption: a turn, a subagent, a call.
//
// The four token counters are separate because their prices differ by orders of
// magnitude (ADR-0008): reading a cached prefix costs ~0.1× the input and
// writing to cache 1.25×. Keeping only a "token total" would throw away exactly
// the information that calibrates the router (P-7) and that exposes a silent
// cache invalidator.
type UsageEvent struct {
	ID        string
	AccountID string
	DemandID  string // empty = account consumption, with no demand (batch, indexing)
	ThreadID  string
	Model     string

	InputTokens         int64 // input NOT cached
	OutputTokens        int64
	CacheReadTokens     int64 // prefix served from cache — the cheap one
	CacheCreationTokens int64 // prefix written to cache — 1.25× the input

	CostMicros Micros
	Currency   string
	At         time.Time
}

// PromptTokens is everything that went into the prompt, cached or not. It is the
// honest denominator of the cache hit ratio: the numerator only makes sense
// against the whole prompt, not against the uncached input.
func (u UsageEvent) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// SuspectCacheMiss raises the ADR-0008 §1 alert: a large prefix going in with NO
// cache read at all. Either the prefix changed (a volatile byte in the context
// package) or the 5-minute TTL expired — in both cases somebody is paying 10×
// for the same prefix and nobody noticed.
//
// It is a heuristic, avowedly: the floor exists so it does not shout on a
// thread's first turn, which legitimately has no cache to read.
func (u UsageEvent) SuspectCacheMiss() bool {
	return u.CacheReadTokens == 0 && u.InputTokens >= cacheMissFloor
}

// cacheMissFloor: below this the prompt is too small to be worth caching — the
// API's minimum cacheable size is of that order. Recalibrate with P-7.
const cacheMissFloor = 2048

func (u UsageEvent) Validate() error {
	if u.AccountID == "" {
		return errs.Invalid("usage with no account")
	}
	if u.Model == "" {
		// With no model there is no calibration possible: the record would enter
		// as an anonymous cost row and drop out of P-7's telemetry.
		return errs.Invalid("usage with no model")
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 ||
		u.CacheReadTokens < 0 || u.CacheCreationTokens < 0 {
		return errs.Invalid("a token count cannot be negative")
	}
	if u.CostMicros < 0 {
		return errs.Invalid("a cost cannot be negative")
	}
	return nil
}

// Budget is a scope's ceiling and running total.
type Budget struct {
	AccountID   string
	Scope       Scope
	ScopeID     string
	LimitMicros Micros
	SpentMicros Micros
	Currency    string
	UpdatedAt   time.Time
}

// Unlimited: a zero limit is the ABSENCE of a ceiling, not a ceiling of zero.
// The distinction is the difference between "a new account works" and "a new
// account is born paused on its first token".
func (b Budget) Unlimited() bool { return b.LimitMicros <= 0 }

// Exceeded is the current state. Exceeded does NOT mean blocked: what happens on
// an overrun is the service's decision (a pause, ADR-0008 §2), and never a
// refusal to record.
func (b Budget) Exceeded() bool { return !b.Unlimited() && b.SpentMicros >= b.LimitMicros }

// Remaining is never negative: whoever consumes that number wants to know how
// much is still spendable, and "minus twenty" does not answer that question.
func (b Budget) Remaining() Micros {
	if b.Unlimited() {
		return 0
	}
	if b.SpentMicros >= b.LimitMicros {
		return 0
	}
	return b.LimitMicros - b.SpentMicros
}

// NewlyExceeded is the EMISSION rule for the overrun event, and it lives here
// because the adapter needs it inside the transaction — leaving it in SQL would
// scatter business rules across the schema.
//
// What matters is the TRANSITION, not the state: an already-exceeded budget
// produces one item in the attention box, not one per turn until somebody looks.
// It holds both for spend crossing the ceiling and for a ceiling lowered under
// the spend — the two are the same event seen from different sides.
func NewlyExceeded(before, after Budget) bool {
	return !before.Exceeded() && after.Exceeded()
}

// BudgetState is the before/after pair of a write. It exists because the
// decision to emit depends on the transition, and the transition is only visible
// if both sides cross the boundary together.
type BudgetState struct {
	Before Budget
	After  Budget
}

func (s BudgetState) JustExceeded() bool { return NewlyExceeded(s.Before, s.After) }

// Summary is the aggregation by scope and period.
type Summary struct {
	Scope    Scope
	ScopeID  string
	Since    time.Time
	Until    time.Time
	Currency string

	TotalMicros Micros
	Calls       int64

	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64

	Recent []UsageEvent
}

// CacheHitRatio is the fraction of the prompt that came from cache in the
// period.
//
// The denominator is the WHOLE prompt (input + cache read + cache write) and not
// the input alone: it is the only way for the number to answer "how much of what
// I sent went out cheap". Near zero in an agent flow means an unstable prefix
// (ADR-0008 §1), which is the most expensive invoice there is.
func (s Summary) CacheHitRatio() float64 {
	total := s.InputTokens + s.CacheReadTokens + s.CacheCreationTokens
	if total <= 0 {
		return 0
	}
	return float64(s.CacheReadTokens) / float64(total)
}
