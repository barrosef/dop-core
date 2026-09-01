package cost

import (
	"context"
	"time"
)

// Repository is the cost domain's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
//
// Every operation takes accountID explicitly: multi-tenant isolation is a
// required parameter of the port, not something the adapter could forget.
type Repository interface {
	// RecordUsage writes the consumption and accumulates the affected budgets in a
	// SINGLE transaction, together with the events (ADR-0019). The contract the
	// adapter has to honour:
	//
	//  1. REAL IDEMPOTENCY by (accountID, idempotencyKey): a repeat writes no new
	//     row, does NOT accumulate the budget again, and returns Duplicate=true
	//     with the budgets' current state. Without it the budget becomes fiction
	//     — one network retry would count twice;
	//  2. the affected budgets are the account's and, when there is a demand, the
	//     demand's. The running total goes up in the SAME statement that returns
	//     the previous value, so the transition stays visible;
	//  3. it emits `dop.cost.recorded` and, for every budget where
	//     BudgetState.JustExceeded(), `dop.cost.budget.exceeded` — all in the
	//     write's own transaction.
	RecordUsage(ctx context.Context, u *UsageEvent, idempotencyKey string) (*RecordResult, error)

	// BudgetOf returns the scope's budget. A scope with no budget defined is NOT
	// an error: it returns a zero limit (no ceiling) with the real spend, because
	// "how much has been spent" is a legitimate question before a ceiling
	// exists.
	BudgetOf(ctx context.Context, accountID string, scope Scope, scopeID string) (*Budget, error)

	// SetBudget writes the ceiling PRESERVING the running total, and emits
	// `dop.cost.budget.set` in the same transaction. It returns before and after:
	// lowering the ceiling below the current spend is as real an overrun as
	// spending past the ceiling, and without the pair that would go unnoticed.
	SetBudget(ctx context.Context, b *Budget) (*BudgetState, error)

	// Summarize aggregates by scope and period. recentLimit ≤ 0 skips the list of
	// recent events — the aggregation alone is cheap, the list is not.
	Summarize(ctx context.Context, accountID string, scope Scope, scopeID string,
		since, until time.Time, recentLimit int) (*Summary, error)
}

// RecordResult is what the write returns.
//
// Budgets carries the before/after of each scope touched; Duplicate says whether
// it was a repeat. Note there is no error field for an overrun: exceeding a
// budget NEVER fails the write (ADR-0011 §2) — the demand is stopped with a
// pause, not with a lost measurement.
type RecordResult struct {
	Usage     *UsageEvent
	Duplicate bool
	Budgets   []BudgetState
}
