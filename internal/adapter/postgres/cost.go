package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/cost"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// CostRepo implements cost.Repository. It is the ONLY place with cost SQL — the
// domain never sees a query.
//
// Three things hold for the whole file:
//
//   - account_id goes into EVERY WHERE clause, with no exception. Multi-tenant
//     isolation is a constraint, not trust in the caller;
//   - every state change writes the event in the SAME transaction, through InTx
//   - Emit: a commit ⇒ state and event, or neither (ADR-0019);
//   - deciding WHEN to emit an overrun belongs to the domain
//     (cost.BudgetState.JustExceeded), not to this file. A business rule in SQL
//     is a rule nobody finds later.
type CostRepo struct{ pool *pgxpool.Pool }

// rowQuerier is the minimum for a single-row read: both the pool and a tx
// satisfy it. It exists so the SAME budget loader works inside and outside a
// transaction — reading the budget through two different paths is how the two
// reads end up diverging.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func NewCostRepo(pool *pgxpool.Pool) *CostRepo { return &CostRepo{pool: pool} }

// ── registro de uso ──────────────────────────────────────────────────────────

// RecordUsage is the platform's hot path and the one point where idempotency is
// the difference between a budget and fiction.
//
// The guard is the INSERT into cost_usage_keys: it is the transaction's first
// thing and, when it returns no row, the key has already been used — the
// transaction goes on only to READ the current state, writing nothing and
// accumulating no budget. The key table is separate from cost_usage because
// cost_usage is partitioned and every UNIQUE on a partitioned table has to
// contain the partition key; the long justification is in
// migrations/0008_cost.sql.
func (r *CostRepo) RecordUsage(ctx context.Context, u *cost.UsageEvent, idempotencyKey string) (*cost.RecordResult, error) {
	out := &cost.RecordResult{}

	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var usageID string
		var at time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO cost_usage_keys (account_id, idempotency_key, usage_id, occurred_at)
			VALUES ($1, $2, gen_random_uuid(), $3)
			ON CONFLICT (account_id, idempotency_key) DO NOTHING
			RETURNING usage_id, occurred_at`,
			u.AccountID, idempotencyKey, u.At).Scan(&usageID, &at)

		if NoRows(err) {
			// A legitimate repetition. It returns what was written the first
			// time and the budget AS IT IS — before = after, so nothing "has
			// just blown", but whoever asked still learns whether it is
			// blown.
			return carregarRepeticao(ctx, tx, u, idempotencyKey, out)
		}
		if err != nil {
			return Translate(err, "usage key")
		}

		saved, err := insertUsage(ctx, tx, usageID, at, u)
		if err != nil {
			return err
		}
		out.Usage = saved

		out.Budgets, err = accumulate(ctx, tx, saved)
		if err != nil {
			return err
		}
		return emitUsageEvents(ctx, tx, saved, out.Budgets)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// carregarRepeticao devolve o estado corrente sem escrever nada.
func carregarRepeticao(ctx context.Context, tx pgx.Tx, u *cost.UsageEvent,
	idempotencyKey string, out *cost.RecordResult) error {

	out.Duplicate = true

	var usageID string
	var at time.Time
	if err := tx.QueryRow(ctx, `
		SELECT usage_id, occurred_at FROM cost_usage_keys
		 WHERE account_id = $1 AND idempotency_key = $2`,
		u.AccountID, idempotencyKey).Scan(&usageID, &at); err != nil {
		return Translate(err, "usage key")
	}

	saved, err := usageByID(ctx, tx, u.AccountID, usageID, at)
	if err != nil {
		return err
	}
	out.Usage = saved

	scopes := affectedScopes(saved)
	for _, sc := range scopes {
		b, err := budgetOf(ctx, tx, saved.AccountID, sc.scope, sc.id)
		if err != nil {
			return err
		}
		// Before = after: the write did not happen, so there was no transition.
		out.Budgets = append(out.Budgets, cost.BudgetState{Before: *b, After: *b})
	}
	return nil
}

const usageCols = `id, account_id, COALESCE(demand_id::text,''), thread_id, model,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
	cost_micros, currency, occurred_at`

func scanUsage(row pgx.Row) (*cost.UsageEvent, error) {
	var u cost.UsageEvent
	var micros int64
	if err := row.Scan(&u.ID, &u.AccountID, &u.DemandID, &u.ThreadID, &u.Model,
		&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheCreationTokens,
		&micros, &u.Currency, &u.At); err != nil {
		return nil, err
	}
	u.CostMicros = cost.Micros(micros)
	return &u, nil
}

// insertUsage writes the row with the id the guard has already reserved — it is
// what ties the idempotency key to the corresponding record.
//
// An occurred_at outside the declared partitions fails here, with a constraint
// violation. That is the right behaviour: better to refuse the record than to
// write a cost into a month nobody sweeps.
func insertUsage(ctx context.Context, tx pgx.Tx, id string, at time.Time, u *cost.UsageEvent) (*cost.UsageEvent, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO cost_usage (id, account_id, demand_id, thread_id, model,
			input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			cost_micros, currency, occurred_at)
		VALUES ($1, $2, NULLIF($3,'')::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+usageCols,
		id, u.AccountID, u.DemandID, u.ThreadID, u.Model,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens,
		int64(u.CostMicros), u.Currency, at)

	saved, err := scanUsage(row)
	if err != nil {
		return nil, Translate(err, "uso")
	}
	return saved, nil
}

// usageByID reads by the full PK (id, occurred_at): without the partition key
// the planner would scan ALL the partitions to find one row.
func usageByID(ctx context.Context, tx pgx.Tx, accountID, id string, at time.Time) (*cost.UsageEvent, error) {
	saved, err := scanUsage(tx.QueryRow(ctx,
		`SELECT `+usageCols+` FROM cost_usage
		  WHERE id = $1 AND occurred_at = $2 AND account_id = $3`, id, at, accountID))
	if err != nil {
		return nil, Translate(err, "uso")
	}
	return saved, nil
}

type scopeRef struct {
	scope cost.Scope
	id    string
}

// affectedScopes: every consumption touches the account's budget; a consumption
// with a demand touches that demand's too. The order is fixed so the overrun
// event always comes out in the same sequence — an event log with an unstable
// order is a log that is hard to read.
func affectedScopes(u *cost.UsageEvent) []scopeRef {
	scopes := []scopeRef{{cost.ScopeAccount, u.AccountID}}
	if u.DemandID != "" {
		scopes = append(scopes, scopeRef{cost.ScopeDemand, u.DemandID})
	}
	return scopes
}

// accumulate adds the consumption in each scope and returns the BEFORE and the
// AFTER.
//
// The before comes out of the same UPDATE (spent_micros - $delta), and not of an
// earlier SELECT: two commands would open a window for two concurrent writes to
// read the same "before" and for neither to see the overrun's transition.
func accumulate(ctx context.Context, tx pgx.Tx, u *cost.UsageEvent) ([]cost.BudgetState, error) {
	delta := int64(u.CostMicros)
	var out []cost.BudgetState

	for _, sc := range affectedScopes(u) {
		var limit, before, after int64
		var currency string
		var updatedAt time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO cost_budgets (account_id, scope, scope_id, limit_micros, spent_micros, currency)
			VALUES ($1, $2, $3, 0, $4, $5)
			ON CONFLICT (account_id, scope, scope_id) DO UPDATE
			   SET spent_micros = cost_budgets.spent_micros + EXCLUDED.spent_micros,
			       updated_at   = now()
			RETURNING limit_micros, spent_micros - $4, spent_micros, currency, updated_at`,
			u.AccountID, string(sc.scope), sc.id, delta, u.Currency,
		).Scan(&limit, &before, &after, &currency, &updatedAt); err != nil {
			return nil, Translate(err, "budget")
		}

		base := cost.Budget{
			AccountID: u.AccountID, Scope: sc.scope, ScopeID: sc.id,
			LimitMicros: cost.Micros(limit), Currency: currency, UpdatedAt: updatedAt,
		}
		antes, depois := base, base
		antes.SpentMicros = cost.Micros(before)
		depois.SpentMicros = cost.Micros(after)
		out = append(out, cost.BudgetState{Before: antes, After: depois})
	}
	return out, nil
}

// emitUsageEvents writes the events in the write's transaction.
//
// There are two kinds and they have different readers: `dop.cost.recorded` feeds
// measurement and calibration (P-7); `dop.cost.budget.exceeded` is what makes
// the demand PAUSE and become an attention-box item (ADR-0011 §2). That is why
// the second only comes out on the TRANSITION — one event per overrun, not one
// per turn after it.
func emitUsageEvents(ctx context.Context, tx pgx.Tx, u *cost.UsageEvent, states []cost.BudgetState) error {
	aggregate, aggregateID := "account", u.AccountID
	if u.DemandID != "" {
		aggregate, aggregateID = "demand", u.DemandID
	}

	if err := Emit(ctx, tx, ports.Event{
		AccountID: u.AccountID, Aggregate: aggregate, AggregateID: aggregateID,
		Type:       "dop.cost.recorded",
		OccurredAt: u.At,
		Payload: mustJSON(map[string]any{
			"usage_id": u.ID, "thread_id": u.ThreadID, "model": u.Model,
			"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
			"cache_read_tokens": u.CacheReadTokens, "cache_creation_tokens": u.CacheCreationTokens,
			"cost_micros": int64(u.CostMicros), "currency": u.Currency,
			// Zero cache on a large prompt is ADR-0012 §1's silent invalidator
			// — it goes in the event so it can become an alert without anyone
			// having to reprocess the whole table to find out.
			"suspect_cache_miss": u.SuspectCacheMiss(),
		}),
	}); err != nil {
		return err
	}

	for _, st := range states {
		if !st.JustExceeded() {
			continue
		}
		if err := emitExceeded(ctx, tx, st.After); err != nil {
			return err
		}
	}
	return nil
}

func emitExceeded(ctx context.Context, tx pgx.Tx, b cost.Budget) error {
	aggregate := "account"
	if b.Scope == cost.ScopeDemand {
		aggregate = "demand"
	}
	return Emit(ctx, tx, ports.Event{
		AccountID: b.AccountID, Aggregate: aggregate, AggregateID: b.ScopeID,
		Type: "dop.cost.budget.exceeded",
		Payload: mustJSON(map[string]any{
			"scope": string(b.Scope), "scope_id": b.ScopeID,
			"limit_micros": int64(b.LimitMicros), "spent_micros": int64(b.SpentMicros),
			"currency": b.Currency,
		}),
	})
}

// ── budget ───────────────────────────────────────────────────────────────────

func (r *CostRepo) BudgetOf(ctx context.Context, accountID string, scope cost.Scope, scopeID string) (*cost.Budget, error) {
	return budgetOf(ctx, r.pool, accountID, scope, scopeID)
}

// budgetOf serves the pool and a transaction through the same path.
//
// A scope with no row in cost_budgets is NOT "not found": it means nobody has
// set a ceiling yet, and the real spend is still a legitimate question. The sum
// over cost_usage only runs in that case — on the normal path there is a row
// with the accumulated total, and the system's largest table is not touched.
func budgetOf(ctx context.Context, q rowQuerier, accountID string, scope cost.Scope, scopeID string) (*cost.Budget, error) {

	b := cost.Budget{AccountID: accountID, Scope: scope, ScopeID: scopeID}
	var limit, spent int64

	err := q.QueryRow(ctx, `
		SELECT limit_micros, spent_micros, currency, updated_at
		  FROM cost_budgets
		 WHERE account_id = $1 AND scope = $2 AND scope_id = $3`,
		accountID, string(scope), scopeID).
		Scan(&limit, &spent, &b.Currency, &b.UpdatedAt)

	if NoRows(err) {
		return budgetFromUsage(ctx, q, accountID, scope, scopeID)
	}
	if err != nil {
		return nil, Translate(err, "budget")
	}
	b.LimitMicros, b.SpentMicros = cost.Micros(limit), cost.Micros(spent)
	return &b, nil
}

func budgetFromUsage(ctx context.Context, q rowQuerier, accountID string, scope cost.Scope, scopeID string) (*cost.Budget, error) {

	var spent int64
	var currency string
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0), COALESCE(MIN(currency), 'USD')
		  FROM cost_usage
		 WHERE account_id = $1
		   AND ($2::uuid IS NULL OR demand_id = $2::uuid)`,
		accountID, demandFilter(scope, scopeID)).Scan(&spent, &currency); err != nil {
		return nil, Translate(err, "budget")
	}
	// LimitMicros zero: NO CEILING. An absent budget never becomes a zero ceiling.
	return &cost.Budget{
		AccountID: accountID, Scope: scope, ScopeID: scopeID,
		SpentMicros: cost.Micros(spent), Currency: currency,
	}, nil
}

// demandFilter returns nil in the account scope — the queries' `$n::uuid IS
// NULL` turns that into "no demand filter" without assembling SQL by hand.
func demandFilter(scope cost.Scope, scopeID string) any {
	if scope == cost.ScopeDemand && scopeID != "" {
		return scopeID
	}
	return nil
}

// SetBudget writes the ceiling preserving the accumulated total, and returns
// before/after.
//
// The `antes` CTE reads the OLD row: within the same command it sees the
// snapshot prior to the upsert, which is exactly what is needed to know whether
// lowering the ceiling has just blown the budget.
func (r *CostRepo) SetBudget(ctx context.Context, b *cost.Budget) (*cost.BudgetState, error) {
	var st cost.BudgetState

	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var limAntes, gastoAntes, limDepois, gastoDepois int64
		var currency string
		var updatedAt time.Time

		if err := tx.QueryRow(ctx, `
			WITH antes AS (
			  SELECT limit_micros, spent_micros FROM cost_budgets
			   WHERE account_id = $1 AND scope = $2 AND scope_id = $3
			), upsert AS (
			  INSERT INTO cost_budgets (account_id, scope, scope_id, limit_micros, spent_micros, currency)
			  VALUES ($1, $2, $3, $4, 0, $5)
			  ON CONFLICT (account_id, scope, scope_id) DO UPDATE
			     SET limit_micros = EXCLUDED.limit_micros,
			         currency     = EXCLUDED.currency,
			         updated_at   = now()
			  RETURNING limit_micros, spent_micros, currency, updated_at
			)
			SELECT COALESCE((SELECT limit_micros FROM antes), 0),
			       COALESCE((SELECT spent_micros FROM antes), 0),
			       u.limit_micros, u.spent_micros, u.currency, u.updated_at
			  FROM upsert u`,
			b.AccountID, string(b.Scope), b.ScopeID, int64(b.LimitMicros), b.Currency,
		).Scan(&limAntes, &gastoAntes, &limDepois, &gastoDepois, &currency, &updatedAt); err != nil {
			return Translate(err, "budget")
		}

		st.Before = cost.Budget{
			AccountID: b.AccountID, Scope: b.Scope, ScopeID: b.ScopeID,
			LimitMicros: cost.Micros(limAntes), SpentMicros: cost.Micros(gastoAntes),
			Currency: currency,
		}
		st.After = cost.Budget{
			AccountID: b.AccountID, Scope: b.Scope, ScopeID: b.ScopeID,
			LimitMicros: cost.Micros(limDepois), SpentMicros: cost.Micros(gastoDepois),
			Currency: currency, UpdatedAt: updatedAt,
		}

		if err := Emit(ctx, tx, ports.Event{
			AccountID: b.AccountID, Aggregate: budgetAggregate(b.Scope), AggregateID: b.ScopeID,
			Type: "dop.cost.budget.set",
			Payload: mustJSON(map[string]any{
				"scope": string(b.Scope), "scope_id": b.ScopeID,
				"limit_micros": int64(limDepois), "previous_limit_micros": int64(limAntes),
				"spent_micros": int64(gastoDepois), "currency": currency,
			}),
		}); err != nil {
			return err
		}

		// A ceiling lowered below the spend is a real overrun, and it has to
		// pause the demand through the same path as an overrun by consumption.
		if st.JustExceeded() {
			return emitExceeded(ctx, tx, st.After)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func budgetAggregate(s cost.Scope) string {
	if s == cost.ScopeDemand {
		return "demand"
	}
	return "account"
}

// ── aggregation ──────────────────────────────────────────────────────────────

// Summarize aggregates in the DATABASE, not in memory: bringing the period's
// rows over to sum them in Go would mean moving millions of records across the
// network to produce six numbers.
func (r *CostRepo) Summarize(ctx context.Context, accountID string, scope cost.Scope, scopeID string,
	since, until time.Time, recentLimit int) (*cost.Summary, error) {

	s := cost.Summary{Scope: scope, ScopeID: scopeID, Since: since, Until: until}
	var total int64
	demand := demandFilter(scope, scopeID)

	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0), COUNT(*),
		       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0),
		       COALESCE(MIN(currency), 'USD')
		  FROM cost_usage
		 WHERE account_id = $1
		   AND occurred_at >= $2 AND occurred_at < $3
		   AND ($4::uuid IS NULL OR demand_id = $4::uuid)`,
		accountID, since, until, demand,
	).Scan(&total, &s.Calls, &s.InputTokens, &s.OutputTokens,
		&s.CacheReadTokens, &s.CacheCreationTokens, &s.Currency); err != nil {
		return nil, Translate(err, "cost summary")
	}
	s.TotalMicros = cost.Micros(total)

	if recentLimit <= 0 {
		return &s, nil
	}

	rows, err := r.pool.Query(ctx, `
		SELECT `+usageCols+`
		  FROM cost_usage
		 WHERE account_id = $1
		   AND occurred_at >= $2 AND occurred_at < $3
		   AND ($4::uuid IS NULL OR demand_id = $4::uuid)
		 ORDER BY occurred_at DESC
		 LIMIT $5`, accountID, since, until, demand, recentLimit)
	if err != nil {
		return nil, Translate(err, "cost summary")
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, Translate(err, "cost summary")
		}
		s.Recent = append(s.Recent, *u)
	}
	return &s, rows.Err()
}

var _ cost.Repository = (*CostRepo)(nil)
