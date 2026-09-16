package cost

import (
	"context"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Service concentrates the cost rules. It takes only PORTS.
type Service struct {
	repo   Repository
	clock  ports.Clock
	router *Router
}

// NewService requires a clock and accepts a nil router.
//
// The asymmetry is deliberate. The clock is a PORT: accepting nil makes the
// service fall back to time.Now() internally, no period test stays
// deterministic, and nobody notices the abstraction is unproven — the panic here
// is a wiring error, caught at boot. The router is this package's POLICY, with a
// default written in ADR-0011; nil merely selects that default, switching
// nothing off.
func NewService(repo Repository, clock ports.Clock, router *Router) *Service {
	if clock == nil {
		panic("cost.NewService: clock is required — use clock.NewSystem()")
	}
	if router == nil {
		router = NewRouter(nil)
	}
	return &Service{repo: repo, clock: clock, router: router}
}

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// RecordOutcome is the result of recording consumption.
//
// BudgetExceeded is a STATE, not a transition: repeating a call returns the same
// warning as the original, because whoever asks "may I go on?" needs the right
// answer even when the write did not happen again.
type RecordOutcome struct {
	Usage          *UsageEvent
	Duplicate      bool
	BudgetExceeded bool
	// Exceeded are the scopes that overran — it is what the attention box shows
	// for the human to decide (raise the ceiling, cut scope, stop).
	Exceeded []Budget
}

// RecordUsage records model consumption.
//
// Two things this method does NOT do, and both are the decision:
//
//   - it does not refuse the write because the budget is exceeded. Measurement
//     that fails when the budget runs out is measurement that disappears exactly
//     when it matters most, and ADR-0011 §2's cut-off is SOFT: the demand pauses
//     and asks, it never dies mid-way and is never cut in silence. Whoever
//     pauses is the consumer of `dop.cost.budget.exceeded`; cost only warns;
//   - it does not deduce or invent the idempotency key. It is required here — in
//     other writes the interceptor (ADR-0017) protects and the database's UNIQUE
//     is the last barrier, but in this case the duplicate collides with nothing:
//     it would enter as legitimate consumption and the budget would become
//     fiction.
func (s *Service) RecordUsage(ctx context.Context, u UsageEvent, idempotencyKey string) (*RecordOutcome, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return nil, errs.Invalid("recording usage requires an idempotency key")
	}

	// The account comes from the context, not the body: accepting the body's
	// would allow charging consumption to somebody else's account.
	u.AccountID = accountID
	if u.Currency == "" {
		u.Currency = DefaultCurrency
	}
	if u.At.IsZero() {
		u.At = s.now()
	}
	u.At = u.At.UTC()
	if err := u.Validate(); err != nil {
		return nil, err
	}

	res, err := s.repo.RecordUsage(ctx, &u, idempotencyKey)
	if err != nil {
		return nil, err
	}

	out := &RecordOutcome{Usage: res.Usage, Duplicate: res.Duplicate}
	for _, st := range res.Budgets {
		if st.After.Exceeded() {
			out.BudgetExceeded = true
			out.Exceeded = append(out.Exceeded, st.After)
		}
	}
	return out, nil
}

// GetBudget returns the scope's budget. A scope with no ceiling defined returns
// a zero limit with the real spend — the absence of a budget is an answer, not
// an error.
func (s *Service) GetBudget(ctx context.Context, scope Scope, scopeID string) (*Budget, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	scope, scopeID, err = s.normalizeScope(accountID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return s.repo.BudgetOf(ctx, accountID, scope, scopeID)
}

// SetBudget sets the scope's ceiling, preserving the running total.
//
// Lowering the ceiling below the current spend is allowed and is an overrun: the
// event goes out in the write's own transaction, and the demand pauses down the
// usual path. Forbidding the lowering would be worse — an operator who discovers
// a demand burning money has to be able to close the tap without waiting for the
// next turn.
func (s *Service) SetBudget(ctx context.Context, b Budget) (*Budget, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	scope, scopeID, err := s.normalizeScope(accountID, b.Scope, b.ScopeID)
	if err != nil {
		return nil, err
	}
	if b.LimitMicros < 0 {
		return nil, errs.Invalid("the limit cannot be negative (zero = no ceiling)")
	}
	b.AccountID = accountID
	b.Scope, b.ScopeID = scope, scopeID
	if b.Currency == "" {
		b.Currency = DefaultCurrency
	}
	b.UpdatedAt = s.now()

	st, err := s.repo.SetBudget(ctx, &b)
	if err != nil {
		return nil, err
	}
	return &st.After, nil
}

// RouteModel returns the task→(model, effort) decision with its justification.
//
// It is a PURE function of the kind of work, and stays that way on purpose.
// demandID is in the contract (and is accepted here) because P-7's calibration
// will want to correlate decision and spend per demand — but it does NOT take
// part in the decision today, and pretending it does would hide that the policy
// is still ADR-0011's raw table.
//
// In particular, a tight budget does not downgrade the model: downgrading under
// cost pressure would run over the fixed rule that you do not save on the
// critic, and ADR-0011 §2's cut-off mechanism is to pause and ask, not to
// degrade in silence.
func (s *Service) RouteModel(ctx context.Context, taskKind TaskKind, demandID string) (*Decision, error) {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return nil, err
	}
	d, err := s.router.Route(taskKind)
	if err != nil {
		return nil, errs.Invalid("%v", err)
	}
	return &d, nil
}

// RoutingTable exposes the whole policy for auditing and for P-7's calibration
// screen — whoever is going to recalibrate needs to see what is in force.
func (s *Service) RoutingTable() []Decision { return s.router.Table() }

// Summarize aggregates by scope and period. An empty period means the current
// month, which is the billing cycle's window and what the screen asks for in 9
// openings out of 10.
func (s *Service) Summarize(ctx context.Context, scope Scope, scopeID string,
	since, until time.Time, recentLimit int) (*Summary, error) {

	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	scope, scopeID, err = s.normalizeScope(accountID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	if since.IsZero() || until.IsZero() {
		since, until = CurrentMonth(s.now())
	}
	if !until.After(since) {
		return nil, errs.Invalid("invalid period: the end has to be after the start")
	}
	if recentLimit < 0 {
		recentLimit = 0
	}
	if recentLimit > maxRecent {
		recentLimit = maxRecent
	}
	return s.repo.Summarize(ctx, accountID, scope, scopeID, since.UTC(), until.UTC(), recentLimit)
}

// maxRecent exists because the usage table is the platform's fastest growing:
// a missing limit becomes a scan of millions of rows on the first screen that
// forgets to paginate.
const maxRecent = 200

// CurrentMonth is the default aggregation window, in UTC — the process's clock
// month and the database's have to be the same month.
func CurrentMonth(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return since, since.AddDate(0, 1, 0)
}

// normalizeScope resolves the scope and refuses what is not vocabulary.
//
// An empty scope becomes 'account' with the active account: it is the common
// case, and asking the caller to repeat the account id that is already in the
// context only creates a chance for them to repeat the WRONG id.
func (s *Service) normalizeScope(accountID string, scope Scope, scopeID string) (Scope, string, error) {
	scope = Scope(strings.ToLower(strings.TrimSpace(string(scope))))
	scopeID = strings.TrimSpace(scopeID)
	if scope == "" {
		scope = ScopeAccount
	}
	if !ValidScope(scope) {
		return "", "", errs.Invalid("unknown budget scope: %q", scope)
	}
	if scope == ScopeAccount {
		// The scope's account is ALWAYS the active one. Accepting another would
		// open somebody else's budget to a read by parameter.
		return scope, accountID, nil
	}
	if scopeID == "" {
		return "", "", errs.Invalid("a demand scope requires the demand identifier")
	}
	return scope, scopeID, nil
}
