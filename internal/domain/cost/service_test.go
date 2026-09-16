package cost_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/domain/cost"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT a database: the repository is a port, and an
// in-memory double goes in here reproducing the guarantee the adapter has to honour.

const (
	accountA = "11111111-1111-1111-1111-111111111111"
	demandA  = "22222222-2222-2222-2222-222222222222"
)

var instant = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// fixedClock is the Clock double. It lives here, and not in internal/adapter/clock,
// because the architecture test fails ANY adapter import under
// internal/domain — including in a _test.go file.
type fixedClock struct{ t time.Time }

func (r fixedClock) Now() time.Time { return r.t }

// ── repository double ────────────────────────────────────────────────────────

// memRepo reproduces the only guarantee that matters here: the idempotency key
// is the guard, and a repeat does NOT accumulate the budget again. If this double
// contasse duas vezes, o teste passaria a medir o double em vez da regra.
type memRepo struct {
	keys    map[string]*cost.UsageEvent // account|key → usage stored
	budgets map[string]*cost.Budget     // account|scope|id → budget
	usages  []cost.UsageEvent
	failure error // injectable, for the repository's failure path
}

func novoRepo() *memRepo {
	return &memRepo{
		keys:    map[string]*cost.UsageEvent{},
		budgets: map[string]*cost.Budget{},
	}
}

func chaveOrc(accountID string, s cost.Scope, id string) string {
	return accountID + "|" + string(s) + "|" + id
}

func (r *memRepo) scopesOf(u *cost.UsageEvent) []cost.Budget {
	out := []cost.Budget{{AccountID: u.AccountID, Scope: cost.ScopeAccount, ScopeID: u.AccountID}}
	if u.DemandID != "" {
		out = append(out, cost.Budget{AccountID: u.AccountID, Scope: cost.ScopeDemand, ScopeID: u.DemandID})
	}
	return out
}

func (r *memRepo) row(b cost.Budget) *cost.Budget {
	k := chaveOrc(b.AccountID, b.Scope, b.ScopeID)
	if cur, ok := r.budgets[k]; ok {
		return cur
	}
	newOne := b
	newOne.Currency = cost.DefaultCurrency
	r.budgets[k] = &newOne
	return &newOne
}

func (r *memRepo) RecordUsage(_ context.Context, u *cost.UsageEvent, key string) (*cost.RecordResult, error) {
	if r.failure != nil {
		return nil, r.failure
	}
	res := &cost.RecordResult{}

	if stored, ok := r.keys[u.AccountID+"|"+key]; ok {
		// A repeat: nothing is written, the budget is returned as it stands.
		res.Duplicate = true
		res.Usage = stored
		for _, e := range r.scopesOf(stored) {
			cur := *r.row(e)
			res.Budgets = append(res.Budgets, cost.BudgetState{Before: cur, After: cur})
		}
		return res, nil
	}

	u.ID = "uso-" + key
	r.usages = append(r.usages, *u)
	stored := *u
	r.keys[u.AccountID+"|"+key] = &stored
	res.Usage = &stored

	for _, e := range r.scopesOf(u) {
		row := r.row(e)
		before := *row
		row.SpentMicros += u.CostMicros
		res.Budgets = append(res.Budgets, cost.BudgetState{Before: before, After: *row})
	}
	return res, nil
}

func (r *memRepo) BudgetOf(_ context.Context, accountID string, s cost.Scope, id string) (*cost.Budget, error) {
	if r.failure != nil {
		return nil, r.failure
	}
	b := *r.row(cost.Budget{AccountID: accountID, Scope: s, ScopeID: id})
	return &b, nil
}

func (r *memRepo) SetBudget(_ context.Context, b *cost.Budget) (*cost.BudgetState, error) {
	if r.failure != nil {
		return nil, r.failure
	}
	row := r.row(*b)
	before := *row
	row.LimitMicros = b.LimitMicros // the accumulated total PRESERVED
	row.UpdatedAt = b.UpdatedAt
	return &cost.BudgetState{Before: before, After: *row}, nil
}

func (r *memRepo) Summarize(_ context.Context, accountID string, s cost.Scope, id string,
	since, until time.Time, recentLimit int) (*cost.Summary, error) {
	if r.failure != nil {
		return nil, r.failure
	}
	out := &cost.Summary{Scope: s, ScopeID: id, Since: since, Until: until, Currency: cost.DefaultCurrency}
	for _, u := range r.usages {
		if u.AccountID != accountID {
			continue
		}
		if s == cost.ScopeDemand && u.DemandID != id {
			continue
		}
		if u.At.Before(since) || !u.At.Before(until) {
			continue
		}
		out.TotalMicros += u.CostMicros
		out.Calls++
		out.InputTokens += u.InputTokens
		out.OutputTokens += u.OutputTokens
		out.CacheReadTokens += u.CacheReadTokens
		out.CacheCreationTokens += u.CacheCreationTokens
		if len(out.Recent) < recentLimit {
			out.Recent = append(out.Recent, u)
		}
	}
	return out, nil
}

var _ cost.Repository = (*memRepo)(nil)

// ── auxiliares ───────────────────────────────────────────────────────────────

func ctxConta() context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		RequestID: "req-1", AccountID: accountA,
		ActorID: "ator-1", ActorKind: ctxutil.ActorAgent,
	})
}

func novoServico(r *memRepo) *cost.Service {
	return cost.NewService(r, fixedClock{t: instant}, nil)
}

func usageOf(micros cost.Micros) cost.UsageEvent {
	return cost.UsageEvent{
		DemandID: demandA, ThreadID: "th-1", Model: "claude-opus",
		InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 8000,
		CostMicros: micros, At: instant,
	}
}

// ════════════════════════════════════════════════════════════════════════════
// The central guarantee: resending does not count twice.
// ════════════════════════════════════════════════════════════════════════════

func TestResentRecordUsageDoesNotCountTwice(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	ctx := ctxConta()

	primeira, err := svc.RecordUsage(ctx, usageOf(500_000), "key-1")
	if err != nil {
		t.Fatalf("the first record failed: %v", err)
	}
	if primeira.Duplicate {
		t.Fatal("the first record must not be marked as a repeat")
	}

	// Mesma chamada, reenviada — retentativa de rede, redelivery do broker,
	// impatient client. Any of the three would double the budget without this.
	segunda, err := svc.RecordUsage(ctx, usageOf(500_000), "key-1")
	if err != nil {
		t.Fatalf("the resend failed (it should be accepted as a repeat): %v", err)
	}
	if !segunda.Duplicate {
		t.Error("a resend of the SAME key should be recognized as a repeat")
	}

	b, err := svc.GetBudget(ctx, cost.ScopeAccount, "")
	if err != nil {
		t.Fatalf("reading the budget failed: %v", err)
	}
	if b.SpentMicros != 500_000 {
		t.Errorf("accumulated spend = %d micros, want 500000 — the resend counted twice",
			b.SpentMicros)
	}
	if len(repo.usages) != 1 {
		t.Errorf("records stored = %d, want 1", len(repo.usages))
	}

	// A DIFFERENT key is different consumption — the guard must not become a gag.
	if _, err := svc.RecordUsage(ctx, usageOf(500_000), "key-2"); err != nil {
		t.Fatalf("the second legitimate consumption failed: %v", err)
	}
	b, _ = svc.GetBudget(ctx, cost.ScopeAccount, "")
	if b.SpentMicros != 1_000_000 {
		t.Errorf("spend after new consumption = %d, expected 1000000", b.SpentMicros)
	}
}

func TestRecordUsageRequiresAnIdempotencyKey(t *testing.T) {
	svc := novoServico(novoRepo())
	// With no key the duplicate collides with nothing: it would enter as legitimate
	// consumption and the budget would become fiction. Refusing is the decision.
	_, err := svc.RecordUsage(ctxConta(), usageOf(1), "   ")
	if err == nil {
		t.Fatal("a record with no idempotency key should be refused")
	}
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("failure = %v (%s), want invalid_argument", err, errs.KindOf(err))
	}
}

func TestRecordUsageRequiresAnActiveAccount(t *testing.T) {
	svc := novoServico(novoRepo())
	if _, err := svc.RecordUsage(context.Background(), usageOf(1), "k"); err == nil {
		t.Fatal("a record with no active account should be refused")
	}
}

func TestRecordUsageIgnoresTheAccountInTheBody(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	u := usageOf(10)
	u.AccountID = "account-do-vizinho"
	if _, err := svc.RecordUsage(ctxConta(), u, "k"); err != nil {
		t.Fatalf("the record failed: %v", err)
	}
	if repo.usages[0].AccountID != accountA {
		t.Errorf("account recorded = %q, want the context's (%q)", repo.usages[0].AccountID, accountA)
	}
}

// ════════════════════════════════════════════════════════════════════════════
// An overrun PAUSES — it does not kill.
// ════════════════════════════════════════════════════════════════════════════

func TestABudgetOverrunPausesInsteadOfKilling(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	ctx := ctxConta()

	if _, err := svc.SetBudget(ctx, cost.Budget{
		Scope: cost.ScopeDemand, ScopeID: demandA, LimitMicros: 1_000_000,
	}); err != nil {
		t.Fatalf("setting the ceiling failed: %v", err)
	}

	// Consumption below the cap: nothing happens.
	dentro, err := svc.RecordUsage(ctx, usageOf(400_000), "k1")
	if err != nil {
		t.Fatalf("a record within the cap failed: %v", err)
	}
	if dentro.BudgetExceeded {
		t.Error("400000 of 1000000 should not report an overrun")
	}

	// Usage that crosses the cap: the write STILL COUNTS and the warning comes
	// with it. An error here would lose the measurement exactly when it matters most.
	estoura, err := svc.RecordUsage(ctx, usageOf(700_000), "k2")
	if err != nil {
		t.Fatalf("an overrun must NOT fail the write — work in progress does not die in silence: %v", err)
	}
	if !estoura.BudgetExceeded {
		t.Fatal("1100000 of 1000000 should report an overrun")
	}
	if len(estoura.Exceeded) == 0 {
		t.Error("the exceeded scope has to come back in the response — it is the attention box item")
	}

	// And work in progress keeps recording: pausing is the decision of whoever
	// consumes the event, not a refusal from this write.
	after, err := svc.RecordUsage(ctx, usageOf(100_000), "k3")
	if err != nil {
		t.Fatalf("recording after the overrun failed — that would be killing instead of pausing: %v", err)
	}
	if !after.BudgetExceeded {
		t.Error("the overrun warning has to persist while the budget is exceeded")
	}

	b, _ := svc.GetBudget(ctx, cost.ScopeDemand, demandA)
	if b.SpentMicros != 1_200_000 {
		t.Errorf("spend = %d, want 1200000 — no write may have been lost", b.SpentMicros)
	}
	if len(repo.usages) != 3 {
		t.Errorf("records stored = %d, want 3", len(repo.usages))
	}
}

func TestACeilingLoweredBelowTheSpendOverruns(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	ctx := ctxConta()

	if _, err := svc.RecordUsage(ctx, usageOf(900_000), "k1"); err != nil {
		t.Fatalf("the record failed: %v", err)
	}
	// An operator closes the tap on a demand that is burning money.
	before := cost.Budget{AccountID: accountA, Scope: cost.ScopeDemand, ScopeID: demandA,
		LimitMicros: 0, SpentMicros: 900_000}
	after := before
	after.LimitMicros = 500_000
	if !cost.NewlyExceeded(before, after) {
		t.Error("lowering the ceiling below the spend is as real an overrun as spending past it")
	}

	b, err := svc.SetBudget(ctx, cost.Budget{
		Scope: cost.ScopeDemand, ScopeID: demandA, LimitMicros: 500_000,
	})
	if err != nil {
		t.Fatalf("lowering the cap failed: %v", err)
	}
	// The running total belongs to the system: setting a ceiling does not zero the spend.
	if b.SpentMicros != 900_000 {
		t.Errorf("spend after SetBudget = %d, expected 900000 (preserved)", b.SpentMicros)
	}
	if !b.Exceeded() {
		t.Error("a budget lowered below the spend should be exceeded")
	}
}

func TestAZeroLimitIsNoCeilingNotACeilingOfZero(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()

	// A new account, with no budget defined: the first token must NOT be born
	// exceeded, otherwise nothing works before somebody sets a ceiling.
	out, err := svc.RecordUsage(ctx, usageOf(999_999_999), "k1")
	if err != nil {
		t.Fatalf("the record failed: %v", err)
	}
	if out.BudgetExceeded {
		t.Error("the absence of a budget must not become a budget of zero")
	}
	b := cost.Budget{LimitMicros: 0, SpentMicros: 1}
	if !b.Unlimited() || b.Exceeded() {
		t.Error("limite zero significa SEM TETO")
	}
}

func TestNewlyExceededIsATransitionNotAState(t *testing.T) {
	estourado := cost.Budget{LimitMicros: 100, SpentMicros: 150}
	maisEstourado := cost.Budget{LimitMicros: 100, SpentMicros: 200}
	// Already exceeded produces ONE attention box item, not one per turn.
	if cost.NewlyExceeded(estourado, maisEstourado) {
		t.Error("an already exceeded budget should not emit an overrun again")
	}
	if !cost.NewlyExceeded(cost.Budget{LimitMicros: 100, SpentMicros: 50}, estourado) {
		t.Error("crossing the ceiling should be a transition")
	}
}

func TestAnInvalidScopeIsRefused(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()
	if _, err := svc.GetBudget(ctx, "galaxia", "x"); err == nil {
		t.Error("a scope outside the vocabulary should be refused")
	}
	if _, err := svc.GetBudget(ctx, cost.ScopeDemand, ""); err == nil {
		t.Error("a demand scope with no identifier should be refused")
	}
	// An account scope ALWAYS uses the active account, even if another is asked for.
	b, err := svc.GetBudget(ctx, cost.ScopeAccount, "account-do-vizinho")
	if err != nil {
		t.Fatalf("the read failed: %v", err)
	}
	if b.ScopeID != accountA {
		t.Errorf("account scope = %q, want the active account (%q)", b.ScopeID, accountA)
	}
}

func TestNewServiceRefusesANilClock(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a nil clock should panic at boot, not go silent in production")
		}
	}()
	cost.NewService(novoRepo(), nil, nil)
}

// ════════════════════════════════════════════════════════════════════════════
// The router's table produces a decision WITH a justification.
// ════════════════════════════════════════════════════════════════════════════

func TestTheRouterTableProducesADecisionWithAJustification(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()

	casos := []struct {
		kind   cost.TaskKind
		classe cost.ModelClass
		effort cost.Effort
	}{
		{cost.TaskMechanical, cost.ClassCheap, cost.EffortLow},
		{cost.TaskInvestigation, cost.ClassMedium, cost.EffortMedium},
		{cost.TaskImplementation, cost.ClassStrong, cost.EffortHigh},
		{cost.TaskCritic, cost.ClassStrong, cost.EffortMax},
	}
	for _, c := range casos {
		d, err := svc.RouteModel(ctx, c.kind, demandA)
		if err != nil {
			t.Fatalf("%s: routing failed: %v", c.kind, err)
		}
		if d.Class != c.classe || d.Effort != c.effort {
			t.Errorf("%s → (%s, %s), want (%s, %s) — ADR-0011 §3",
				c.kind, d.Class, d.Effort, c.classe, c.effort)
		}
		if d.Model == "" {
			t.Errorf("%s: the class has to resolve to a concrete model", c.kind)
		}
		// Without the why, nobody audits and nobody calibrates.
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("%s: decision with no justification", c.kind)
		}
		// And the justification says where it came from — including that it is still a draft.
		if !strings.Contains(d.Reason, "ADR-0011") || !strings.Contains(d.Reason, "P-7") {
			t.Errorf("%s: justification %q declares neither provenance nor the calibration item",
				c.kind, d.Reason)
		}
	}
}

func TestYouDoNotSaveOnTheCritic(t *testing.T) {
	svc := novoServico(novoRepo())
	critic, err := svc.RouteModel(ctxConta(), cost.TaskCritic, "")
	if err != nil {
		t.Fatalf("routing failed: %v", err)
	}
	// The FIXED rule of ADR-0011/0012: the critic is the brake, and the brake is the last
	// the place where the saving happens.
	if critic.Class != cost.ClassStrong || critic.Effort != cost.EffortMax {
		t.Errorf("critic = (%s, %s), expected (strong, max)", critic.Class, critic.Effort)
	}
}

func TestAnUnknownKindOfWorkFallsToTheExpensiveSide(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()

	d, err := svc.RouteModel(ctx, "arqueologia", "")
	if err != nil {
		t.Fatalf("an unknown kind must not stop work in progress: %v", err)
	}
	if d.Class != cost.ClassStrong {
		t.Errorf("fallback = %s, expected strong — when in doubt you do not save", d.Class)
	}
	if !strings.Contains(d.Reason, "outside the vocabulary") {
		t.Errorf("the fallback has to SAY it fell back; justification = %q", d.Reason)
	}

	// An EMPTY kind is a different thing: an incomplete request.
	if _, err := svc.RouteModel(ctx, "", ""); err == nil {
		t.Error("an empty work kind should be refused")
	}
}

func TestTheRouterIsATableInOnePlace(t *testing.T) {
	// Table() is what P-7's calibration screen reads. If the policy stops being a
	// table, this test is the first to notice.
	tabela := cost.NewRouter(nil).Table()
	if len(tabela) != 4 {
		t.Fatalf("the table has %d rows, want 4 (ADR-0011 §3)", len(tabela))
	}
	vistos := map[cost.TaskKind]bool{}
	for _, d := range tabela {
		if vistos[d.TaskKind] {
			t.Errorf("kind %s appears twice in the table", d.TaskKind)
		}
		vistos[d.TaskKind] = true
	}
}

func TestTheCatalogueIsReplaceableWithoutTouchingThePolicy(t *testing.T) {
	// The leading model name changes every six months (ADR-0001); the policy does
	// may change with it.
	r := cost.NewRouter(cost.ModelCatalog{
		cost.ClassCheap:  "modelo-barato-do-fornecedor-x",
		cost.ClassMedium: "modelo-medio-do-fornecedor-x",
		cost.ClassStrong: "modelo-forte-do-fornecedor-x",
	})
	d, err := r.Route(cost.TaskCritic)
	if err != nil {
		t.Fatalf("routing failed: %v", err)
	}
	if d.Model != "modelo-forte-do-fornecedor-x" {
		t.Errorf("model = %q, the catalogue was not honoured", d.Model)
	}
	if d.Effort != cost.EffortMax {
		t.Error("swapping the catalogue must not touch the policy")
	}
}

// ════════════════════════════════════════════════════════════════════════════
// Cache telemetry — the calibration material (ADR-0012, P-7).
// ════════════════════════════════════════════════════════════════════════════

func TestCacheHitRatioUsesTheWholePrompt(t *testing.T) {
	s := cost.Summary{InputTokens: 1000, CacheReadTokens: 9000, CacheCreationTokens: 0}
	if got := s.CacheHitRatio(); got < 0.89 || got > 0.91 {
		t.Errorf("rate = %.3f, want ~0.900 (9000 of the prompt's 10000)", got)
	}
	if (cost.Summary{}).CacheHitRatio() != 0 {
		t.Error("a period with no usage should return zero, not NaN")
	}
}

func TestTheSilentCacheInvalidatorAlert(t *testing.T) {
	// A large prefix with NO cache read at all: somebody is paying 10× the
	// the same prefix (ADR-0012 §1).
	suspeito := cost.UsageEvent{InputTokens: 50_000, CacheReadTokens: 0}
	if !suspeito.SuspectCacheMiss() {
		t.Error("a large prompt with no cache read should be suspicious")
	}
	// A turn with the cache served: normal.
	ok := cost.UsageEvent{InputTokens: 50_000, CacheReadTokens: 40_000}
	if ok.SuspectCacheMiss() {
		t.Error("a turn with a cache read is not suspicious")
	}
	// A small prompt: not worth caching, not an alert.
	pequeno := cost.UsageEvent{InputTokens: 10, CacheReadTokens: 0}
	if pequeno.SuspectCacheMiss() {
		t.Error("a prompt too small to cache is not an alert")
	}
}

func TestSummarizeDefaultsToTheCurrentMonth(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	ctx := ctxConta()

	if _, err := svc.RecordUsage(ctx, usageOf(250_000), "k1"); err != nil {
		t.Fatalf("the record failed: %v", err)
	}
	// Last month's consumption does not fall in the default window.
	antigo := usageOf(999_000)
	antigo.At = instant.AddDate(0, -1, 0)
	if _, err := svc.RecordUsage(ctx, antigo, "k0"); err != nil {
		t.Fatalf("the old record failed: %v", err)
	}

	var zero time.Time
	sum, err := svc.Summarize(ctx, cost.ScopeAccount, "", zero, zero, 10)
	if err != nil {
		t.Fatalf("the summary failed: %v", err)
	}
	if sum.TotalMicros != 250_000 {
		t.Errorf("total = %d, expected 250000 (the current month only)", sum.TotalMicros)
	}
	if sum.Calls != 1 {
		t.Errorf("calls = %d, want 1", sum.Calls)
	}

	from, until := cost.CurrentMonth(instant)
	if from != time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) || until != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("default window = [%s, %s), expected August 2026", from, until)
	}
}

func TestUsageValidation(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()

	semModelo := usageOf(1)
	semModelo.Model = ""
	if _, err := svc.RecordUsage(ctx, semModelo, "k1"); err == nil {
		t.Error("usage with no model should be refused — without it there is no calibration")
	}

	negativo := usageOf(1)
	negativo.InputTokens = -1
	if _, err := svc.RecordUsage(ctx, negativo, "k2"); err == nil {
		t.Error("a negative token count should be refused")
	}

	// A missing instant is filled in by the PORT's clock, never by time.Now.
	semInstante := usageOf(1)
	semInstante.At = time.Time{}
	out, err := svc.RecordUsage(ctx, semInstante, "k3")
	if err != nil {
		t.Fatalf("a record with no instant failed: %v", err)
	}
	if !out.Usage.At.Equal(instant) {
		t.Errorf("instant = %s, expected the injected clock (%s)", out.Usage.At, instant)
	}
}
