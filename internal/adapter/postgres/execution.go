package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/execution"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// ExecutionRepo implements execution.Repository. It is the ONLY place with
// sandbox SQL — the domain never sees a query.
//
// Three things hold for this whole file:
//
//   - account_id goes into EVERY WHERE clause. Multi-tenant isolation is a
//     constraint, not trust in the caller;
//   - every state change writes the event in the SAME transaction, through InTx
//   - Emit. A commit ⇒ state and event, or neither (ADR-0019);
//   - the destruction's irreversibility is NOT this file's responsibility: it is
//     a trigger in the database (migration 0010). Here we only translate the
//     trigger's exception into a domain error, which Translate already does
//     through P0001.
type ExecutionRepo struct{ pool *pgxpool.Pool }

func NewExecutionRepo(pool *pgxpool.Pool) *ExecutionRepo { return &ExecutionRepo{pool: pool} }

var _ execution.Repository = (*ExecutionRepo)(nil)

const sandboxCols = `id, account_id, demand_id, state, tier, namespace, endpoints,
	COALESCE(idempotency_key,''), last_active_at, suspended_at, destroyed_at,
	COALESCE(created_by::text,''), created_at, updated_at`

func scanSandbox(row pgx.Row) (*execution.Sandbox, error) {
	var s execution.Sandbox
	var state, tier string
	var endpoints []byte
	var suspended, destroyed *time.Time
	if err := row.Scan(&s.ID, &s.AccountID, &s.DemandID, &state, &tier, &s.Namespace,
		&endpoints, &s.IdempotencyKey, &s.LastActiveAt, &suspended, &destroyed,
		&s.CreatedBy, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.State = execution.State(state)
	s.Tier = ports.IsolationTier(tier)
	if suspended != nil {
		s.SuspendedAt = *suspended
	}
	if destroyed != nil {
		s.DestroyedAt = *destroyed
	}
	_ = json.Unmarshal(endpoints, &s.Endpoints)
	return &s, nil
}

// ByID returns (nil, nil) when there is no row: whether the absence is an error
// is the domain's decision.
func (r *ExecutionRepo) ByID(ctx context.Context, accountID, id string) (*execution.Sandbox, error) {
	s, err := scanSandbox(r.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE id = $1 AND account_id = $2`, id, accountID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "sandbox")
	}
	return s, nil
}

func (r *ExecutionRepo) ByIdempotencyKey(ctx context.Context, accountID, key string) (*execution.Sandbox, error) {
	if key == "" {
		return nil, nil
	}
	s, err := scanSandbox(r.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes
		  WHERE account_id = $1 AND idempotency_key = $2`, accountID, key))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "sandbox")
	}
	return s, nil
}

// LiveByDemand: alive is everything that was not destroyed. The partial index
// The sandboxes_demanda_viva_uniq index guarantees there is at most one.
func (r *ExecutionRepo) LiveByDemand(ctx context.Context, accountID, demandID string) (*execution.Sandbox, error) {
	s, err := scanSandbox(r.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes
		  WHERE account_id = $1 AND demand_id = $2 AND state <> 'destroyed'`,
		accountID, demandID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "sandbox")
	}
	return s, nil
}

// Create writes the INTENTION to provision and emits the event in the same
// transaction.
//
// The event goes out before the executor answers on purpose: it is what allows
// reconciling a half-started sandbox after a crash. Emitting only at the end
// would leave a live microVM with no record that anybody asked for it.
func (r *ExecutionRepo) Create(ctx context.Context, s *execution.Sandbox) (*execution.Sandbox, error) {
	var saved *execution.Sandbox
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO sandboxes (account_id, demand_id, state, tier, namespace,
			                       idempotency_key, last_active_at, created_by)
			VALUES ($1, $2, $3::sandbox_state, $4::isolation_tier, $5,
			        NULLIF($6,''), $7, NULLIF($8,'')::uuid)
			RETURNING `+sandboxCols,
			s.AccountID, s.DemandID, string(s.State), string(s.Tier), s.Namespace,
			s.IdempotencyKey, s.LastActiveAt, s.CreatedBy)
		var err error
		saved, err = scanSandbox(row)
		if err != nil {
			// A repeated key and "this demand already has a sandbox" become a
			// 409, not a 500: they are the client's errors, with a useful
			// answer.
			return Translate(err, "sandbox")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "sandbox", AggregateID: saved.ID,
			Type: "dop.sandbox.provisioning",
			Payload: mustJSON(map[string]any{
				"demand_id": saved.DemandID,
				"tier":      saved.Tier,
				"namespace": saved.Namespace,
			}),
		})
	})
	return saved, err
}

// MarkProvisioned records what the executor DELIVERED.
//
// The tier is written again, with the value the launcher returned, because it is
// what the client actually got — and the contract promises they see what they
// got, not what they asked for. When the two diverge, the service has already
// discarded the sandbox before reaching here.
func (r *ExecutionRepo) MarkProvisioned(ctx context.Context, accountID, id string, tier ports.IsolationTier, endpoints []execution.Endpoint) (*execution.Sandbox, error) {
	var saved *execution.Sandbox
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE sandboxes
			   SET state          = 'active',
			       tier           = $3::isolation_tier,
			       endpoints      = $4,
			       last_active_at = now(),
			       updated_at     = now()
			 WHERE id = $1 AND account_id = $2
			RETURNING `+sandboxCols,
			id, accountID, string(tier), mustJSON(endpoints))
		var err error
		saved, err = scanSandbox(row)
		if err != nil {
			return Translate(err, "sandbox")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "sandbox", AggregateID: saved.ID,
			Type: "dop.sandbox.provisioned",
			Payload: mustJSON(map[string]any{
				"demand_id": saved.DemandID,
				"tier":      saved.Tier,
				"namespace": saved.Namespace,
				"endpoints": saved.Endpoints,
			}),
		})
	})
	return saved, err
}

// Transition applies the state change and emits the corresponding event.
//
// The event carries `preserva_trabalho`, and not only the new state, because
// that is the question the audit will ask six months from now: "when this
// demand's sandbox went away, did the work go with it?". Leaving that implicit
// in the type's name would force every consumer to rediscover the rule.
func (r *ExecutionRepo) Transition(ctx context.Context, accountID, id string, t execution.Transition) (*execution.Sandbox, error) {
	var saved *execution.Sandbox
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE sandboxes
			   SET state          = $3::sandbox_state,
			       suspended_at   = CASE WHEN $3 = 'suspended' THEN now() ELSE suspended_at END,
			       destroyed_at   = CASE WHEN $3 = 'destroyed' THEN now() ELSE destroyed_at END,
			       last_active_at = CASE WHEN $3 = 'active'    THEN now() ELSE last_active_at END,
			       updated_at     = now()
			 WHERE id = $1 AND account_id = $2
			RETURNING `+sandboxCols,
			id, accountID, string(t.To))
		var err error
		saved, err = scanSandbox(row)
		if err != nil {
			// The irreversibility trigger speaks through RAISE EXCEPTION
			// (P0001): Translate returns it as a Precondition, with the whole
			// message.
			return Translate(err, "sandbox")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "sandbox", AggregateID: saved.ID,
			Type: transitionEvent(t),
			Payload: mustJSON(map[string]any{
				"demand_id":         saved.DemandID,
				"namespace":         saved.Namespace,
				"tier":              saved.Tier,
				"preserva_trabalho": t.PreservesWork(),
				"reversivel":        t.Reversible,
			}),
		})
	})
	return saved, err
}

func transitionEvent(t execution.Transition) string {
	switch t.To {
	case execution.StateSuspended:
		return "dop.sandbox.suspended"
	case execution.StateActive:
		return "dop.sandbox.resumed"
	case execution.StateDestroyed:
		return "dop.sandbox.destroyed"
	}
	return "dop.sandbox.changed"
}

// TouchActivity pushes back the idle suspension. It emits NO event: an activity
// heartbeat is the most frequent thing that happens to a sandbox, and in the
// event log it would drown the demand's dossier — which exists to tell the
// story, not to record every time somebody opened the terminal.
func (r *ExecutionRepo) TouchActivity(ctx context.Context, accountID, id string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE sandboxes SET last_active_at = now(), updated_at = now()
		 WHERE id = $1 AND account_id = $2 AND state <> 'destroyed'`, id, accountID)
	return Translate(err, "sandbox")
}

// ListIdle feeds the cost-saving sweeper. The cut-off comes from the DOMAIN in
// seconds — the SQL has no opinion about how long "idle" is.
// AccountsWithIdle returns NO sandbox at all — only the accounts that have one
// stopped. Returning the rows here would give the system caller the read the
// port denies everyone.
func (r *ExecutionRepo) AccountsWithIdle(ctx context.Context, olderThanSeconds int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT account_id::text
		  FROM sandboxes
		 WHERE state = 'active'
		   AND last_active_at < now() - make_interval(secs => $1)`, olderThanSeconds)
	if err != nil {
		return nil, Translate(err, "accounts with an idle sandbox")
	}
	defer rows.Close()

	var accounts []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, Translate(err, "an account with an idle sandbox")
		}
		accounts = append(accounts, id)
	}
	return accounts, Translate(rows.Err(), "accounts with an idle sandbox")
}

func (r *ExecutionRepo) ListIdle(ctx context.Context, accountID string, olderThanSeconds int) ([]execution.Sandbox, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+sandboxCols+`
		  FROM sandboxes
		 WHERE account_id = $1
		   AND state = 'active'
		   AND last_active_at < now() - make_interval(secs => $2)
		 ORDER BY last_active_at`, accountID, olderThanSeconds)
	if err != nil {
		return nil, Translate(err, "sandboxes")
	}
	defer rows.Close()

	var out []execution.Sandbox
	for rows.Next() {
		s, err := scanSandbox(rows)
		if err != nil {
			return nil, Translate(err, "sandboxes")
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}
