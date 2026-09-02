package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/idem"
)

// DemandRepo implements demand.Repository. It is the ONLY place with demand SQL.
//
// Every write here follows the same shape: open a transaction, write state,
// write the event with Emit in the SAME transaction, commit (ADR-0019). There is
// no path that changes state without an event — the port's signature offers
// none.
type DemandRepo struct{ pool *pgxpool.Pool }

func NewDemandRepo(pool *pgxpool.Pool) *DemandRepo { return &DemandRepo{pool: pool} }

const demandCols = `id::text, account_id::text, project_id::text, external_key, title,
	card_type, provider_status, dop_status, flow_id, flow_version, flow_snapshot,
	created_by, created_at, updated_at`

func scanDemand(row pgx.Row) (*demand.Demand, error) {
	var d demand.Demand
	var status string
	var snap []byte
	if err := row.Scan(&d.ID, &d.AccountID, &d.ProjectID, &d.ExternalKey, &d.Title,
		&d.CardType, &d.ProviderStatus, &status, &d.Flow.FlowID, &d.Flow.Version,
		&snap, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	d.Status = demand.DopStatus(status)
	// The snapshot is the frozen flow: if it does not come back readable, the
	// stage machine loses its ruler. A loud, clear error is better than a demand
	// with no flow circulating through the system.
	if len(snap) > 0 {
		if err := json.Unmarshal(snap, &d.Flow); err != nil {
			return nil, errs.Wrap(errs.KindInternal, err,
				"the flow snapshot of demand %s is unreadable", d.ID)
		}
	}
	return &d, nil
}

const stageCols = `demand_id::text, key, name, type, gate, status, position,
	artifacts, started_at, finished_at, gate_approved, gate_comment`

func (r *DemandRepo) List(ctx context.Context, accountID, projectID string, limit int, after string) ([]demand.Demand, error) {
	if limit <= 0 {
		limit = 50
	}
	// The pagination walks by the last read row's id within the same ordering
	// (created_at, id): the pair orders totally, while created_at alone ties
	// between demands created in the same provider sync.
	rows, err := r.pool.Query(ctx, `
		SELECT `+demandCols+`
		  FROM demands
		 WHERE account_id = $1
		   AND ($2::text = '' OR project_id = $2::uuid)
		   AND ($3::text = '' OR (created_at, id) > (
		        SELECT created_at, id FROM demands WHERE id = $3::uuid AND account_id = $1))
		 ORDER BY created_at, id
		 LIMIT $4`,
		accountID, projectID, after, limit)
	if err != nil {
		return nil, Translate(err, "demands")
	}
	defer rows.Close()

	out := make([]demand.Demand, 0, limit)
	ids := make([]string, 0, limit)
	for rows.Next() {
		d, err := scanDemand(rows)
		if err != nil {
			return nil, Translate(err, "demands")
		}
		out = append(out, *d)
		ids = append(ids, d.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, Translate(err, "demands")
	}
	if len(out) == 0 {
		return out, nil
	}

	// The stages of every demand on the page in ONE query: the cockpit's list
	// must not cost a database round trip per row.
	byDemand, err := r.stagesOf(ctx, r.pool, accountID, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Stages = byDemand[out[i].ID]
	}
	return out, nil
}

func (r *DemandRepo) ByID(ctx context.Context, accountID, id string) (*demand.Demand, error) {
	if !looksLikeUUID(id) {
		return nil, errs.NotFound("demand")
	}
	d, err := scanDemand(r.pool.QueryRow(ctx,
		`SELECT `+demandCols+` FROM demands WHERE id = $1 AND account_id = $2`, id, accountID))
	if err != nil {
		if NoRows(err) {
			return nil, nil // absence is an answer, not a failure: the domain decides
		}
		return nil, Translate(err, "demand")
	}
	stages, err := r.stagesOf(ctx, r.pool, accountID, []string{d.ID})
	if err != nil {
		return nil, err
	}
	d.Stages = stages[d.ID]
	return d, nil
}

func (r *DemandRepo) ByExternalKey(ctx context.Context, accountID, projectID, externalKey string) (*demand.Demand, error) {
	if !looksLikeUUID(projectID) {
		return nil, errs.Invalid("invalid project")
	}
	d, err := scanDemand(r.pool.QueryRow(ctx, `SELECT `+demandCols+`
		  FROM demands
		 WHERE account_id = $1 AND project_id = $2 AND external_key = $3`,
		accountID, projectID, externalKey))
	if err != nil {
		if NoRows(err) {
			return nil, nil
		}
		return nil, Translate(err, "demand")
	}
	stages, err := r.stagesOf(ctx, r.pool, accountID, []string{d.ID})
	if err != nil {
		return nil, err
	}
	d.Stages = stages[d.ID]
	return d, nil
}

// stagesOf loads the stages in the frozen flow's order.
func (r *DemandRepo) stagesOf(ctx context.Context, q pgxQuerier, accountID string, demandIDs []string) (map[string][]demand.Stage, error) {
	rows, err := q.Query(ctx, `
		SELECT `+stageCols+`
		  FROM demand_stages
		 WHERE account_id = $1 AND demand_id = ANY($2::uuid[])
		 ORDER BY demand_id, position`, accountID, demandIDs)
	if err != nil {
		return nil, Translate(err, "the demand's stages")
	}
	defer rows.Close()

	out := map[string][]demand.Stage{}
	for rows.Next() {
		var demandID string
		var st demand.Stage
		var typ, gate, status string
		var artifacts []byte
		if err := rows.Scan(&demandID, &st.Key, &st.Name, &typ, &gate, &status,
			&st.Position, &artifacts, &st.StartedAt, &st.FinishedAt,
			&st.GateApproved, &st.GateComment); err != nil {
			return nil, Translate(err, "the demand's stages")
		}
		st.Type, st.Gate, st.Status = demand.StageType(typ), demand.Gate(gate), demand.StageStatus(status)
		if len(artifacts) > 0 {
			_ = json.Unmarshal(artifacts, &st.Artifacts)
		}
		out[demandID] = append(out[demandID], st)
	}
	return out, rows.Err()
}

// ── escritas ─────────────────────────────────────────────────────────────────

func (r *DemandRepo) Create(ctx context.Context, d *demand.Demand, ev demand.Emission, idemKey string) (*demand.Demand, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Demand, error) {
		saved, err := scanDemand(tx.QueryRow(ctx, `
			INSERT INTO demands (account_id, project_id, external_key, title,
			                     card_type, provider_status, dop_status,
			                     flow_id, flow_version, flow_snapshot, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING `+demandCols,
			d.AccountID, d.ProjectID, d.ExternalKey, d.Title, d.CardType,
			d.ProviderStatus, string(d.Status), d.Flow.FlowID, d.Flow.Version,
			mustJSON(d.Flow), d.CreatedBy))
		if err != nil {
			return nil, Translate(err, "demand")
		}
		for _, st := range d.Stages {
			if _, err := tx.Exec(ctx, `
				INSERT INTO demand_stages (demand_id, account_id, key, name, type,
				                           gate, status, position, artifacts)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				saved.ID, d.AccountID, st.Key, st.Name, string(st.Type),
				string(st.Gate), string(st.Status), st.Position, mustJSON(st.Artifacts)); err != nil {
				return nil, Translate(err, "a demand stage")
			}
		}
		saved.Stages = d.Stages
		return saved, emit(ctx, tx, saved.AccountID, saved.ID, ev, nil)
	})
}

func (r *DemandRepo) SaveStage(ctx context.Context, accountID, demandID string, st demand.Stage, status demand.DopStatus, ev demand.Emission, idemKey string) (*demand.Stage, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Stage, error) {
		row := tx.QueryRow(ctx, `
			UPDATE demand_stages
			   SET status = $4, started_at = $5, finished_at = $6,
			       gate_approved = $7, gate_comment = $8, artifacts = $9
			 WHERE demand_id = $1 AND key = $2 AND account_id = $3
			RETURNING `+stageCols,
			demandID, st.Key, accountID, string(st.Status), st.StartedAt,
			st.FinishedAt, st.GateApproved, st.GateComment, mustJSON(st.Artifacts))

		var demandRef, typ, gate, statusCol string
		var saved demand.Stage
		var artifacts []byte
		if err := row.Scan(&demandRef, &saved.Key, &saved.Name, &typ, &gate, &statusCol,
			&saved.Position, &artifacts, &saved.StartedAt, &saved.FinishedAt,
			&saved.GateApproved, &saved.GateComment); err != nil {
			return nil, Translate(err, "a demand stage")
		}
		saved.Type, saved.Gate, saved.Status = demand.StageType(typ), demand.Gate(gate), demand.StageStatus(statusCol)
		if len(artifacts) > 0 {
			_ = json.Unmarshal(artifacts, &saved.Artifacts)
		}

		// The demand's status is a PROJECTION of the stages — recomputed by the
		// domain and written along, in the same transaction, so the cockpit's
		// list does not have to open each demand to know where it stands.
		if _, err := tx.Exec(ctx, `
			UPDATE demands SET dop_status = $3, updated_at = now()
			 WHERE id = $1 AND account_id = $2`, demandID, accountID, string(status)); err != nil {
			return nil, Translate(err, "demand")
		}
		return &saved, emit(ctx, tx, accountID, demandID, ev, nil)
	})
}

const threadCols = `id::text, account_id::text, demand_id::text, key, purpose, tools,
	model, effort, budget_micros, state, created_by, created_at, updated_at`

func scanThread(row pgx.Row) (*demand.Thread, error) {
	var t demand.Thread
	var state string
	if err := row.Scan(&t.ID, &t.AccountID, &t.DemandID, &t.Key, &t.Card.Purpose,
		&t.Card.Tools, &t.Card.Model, &t.Card.Effort, &t.Card.BudgetMicros,
		&state, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.State = demand.ThreadState(state)
	return &t, nil
}

func (r *DemandRepo) ThreadsOf(ctx context.Context, accountID, demandID string) ([]demand.Thread, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+threadCols+`
		  FROM demand_threads
		 WHERE account_id = $1 AND demand_id = $2
		 ORDER BY created_at`, accountID, demandID)
	if err != nil {
		return nil, Translate(err, "the demand's threads")
	}
	defer rows.Close()

	out := []demand.Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, Translate(err, "the demand's threads")
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (r *DemandRepo) ThreadByID(ctx context.Context, accountID, id string) (*demand.Thread, error) {
	if !looksLikeUUID(id) {
		return nil, errs.NotFound("thread")
	}
	t, err := scanThread(r.pool.QueryRow(ctx, `SELECT `+threadCols+`
		  FROM demand_threads WHERE id = $1 AND account_id = $2`, id, accountID))
	if err != nil {
		if NoRows(err) {
			return nil, nil
		}
		return nil, Translate(err, "thread")
	}
	return t, nil
}

func (r *DemandRepo) CreateThread(ctx context.Context, t *demand.Thread, ev demand.Emission, idemKey string) (*demand.Thread, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Thread, error) {
		saved, err := scanThread(tx.QueryRow(ctx, `
			INSERT INTO demand_threads (account_id, demand_id, key, purpose, tools,
			                            model, effort, budget_micros, state, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			RETURNING `+threadCols,
			// tagsOf because the column is NOT NULL with DEFAULT '{}': a nil
			// slice becomes an explicit NULL, and an explicit NULL IGNORES the
			// default. An agent card with no tools is a common case, not an
			// error.
			t.AccountID, t.DemandID, t.Key, t.Card.Purpose, tagsOf(t.Card.Tools),
			t.Card.Model, t.Card.Effort, t.Card.BudgetMicros, string(t.State), t.CreatedBy))
		if err != nil {
			return nil, Translate(err, "a demand thread")
		}
		// The thread's id only exists after the INSERT, and the log has to be
		// able to rebuild the thread without querying the table — which is why
		// it goes into the payload here, and not in the domain.
		return saved, emit(ctx, tx, saved.AccountID, saved.DemandID, ev,
			map[string]any{"thread_id": saved.ID})
	})
}

func (r *DemandRepo) SaveThreadState(ctx context.Context, accountID, threadID string, state demand.ThreadState, ev demand.Emission, idemKey string) (*demand.Thread, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Thread, error) {
		saved, err := scanThread(tx.QueryRow(ctx, `
			UPDATE demand_threads SET state = $3, updated_at = now()
			 WHERE id = $1 AND account_id = $2
			RETURNING `+threadCols, threadID, accountID, string(state)))
		if err != nil {
			// The "conclusion with no finding" trigger arrives here as P0001
			// and Translate returns it as a precondition failure, with the
			// rule's message — not as a 500.
			return nil, Translate(err, "a demand thread")
		}
		return saved, emit(ctx, tx, saved.AccountID, saved.DemandID, ev, nil)
	})
}

// AppendMessage writes the message to the log ONLY (ADR-0006).
//
// There is no messages table: the text already travels in the event, and reading
// the conversation's history is the `timeline` projection, which indexes by
// aggregate. A table here would be the same text written twice to serve a query
// that already exists.
func (r *DemandRepo) AppendMessage(ctx context.Context, m *demand.Message, ev demand.Emission, idemKey string) (*demand.Message, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Message, error) {
		saved := *m
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&saved.ID); err != nil {
			return nil, Translate(err, "message")
		}
		// The first message takes the thread out of `open`: the conversation's
		// cycle is open → active, and what moves it is the utterance, not a
		// separate command.
		if _, err := tx.Exec(ctx, `
			UPDATE demand_threads SET state = 'ativa', updated_at = now()
			 WHERE id = $1 AND account_id = $2 AND state = 'aberta'`,
			m.ThreadID, m.AccountID); err != nil {
			return nil, Translate(err, "a demand thread")
		}
		return &saved, emit(ctx, tx, m.AccountID, m.DemandID, ev,
			map[string]any{"message_id": saved.ID, "at": saved.At})
	})
}

func (r *DemandRepo) CreateFinding(ctx context.Context, f *demand.Finding, ev demand.Emission, idemKey string) (*demand.Finding, error) {
	return writeIdem(ctx, r.pool, idemKey, ev, func(tx pgx.Tx) (*demand.Finding, error) {
		saved := *f
		if err := tx.QueryRow(ctx, `
			INSERT INTO demand_findings (account_id, demand_id, thread_id, title, payload, created_by)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id::text, created_at`,
			f.AccountID, f.DemandID, f.ThreadID, f.Title, mustJSON(f.Payload), f.CreatedBy,
		).Scan(&saved.ID, &saved.CreatedAt); err != nil {
			return nil, Translate(err, "finding")
		}
		return &saved, emit(ctx, tx, f.AccountID, f.DemandID, ev,
			map[string]any{"finding_id": saved.ID})
	})
}

func (r *DemandRepo) HasFinding(ctx context.Context, accountID, threadID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM demand_findings
		                WHERE thread_id = $1 AND account_id = $2)`,
		threadID, accountID).Scan(&exists)
	if err != nil {
		return false, Translate(err, "the thread's findings")
	}
	return exists, nil
}

// ListFindings returns the demand's findings, from the oldest to the newest.
//
// The order is chronological on purpose: the context package cuts by budget and,
// if the cut removed the OLDEST finding, it would remove precisely what the
// following ones rest on.
func (r *DemandRepo) ListFindings(ctx context.Context, accountID, demandID string) ([]demand.Finding, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, account_id, demand_id, thread_id, title, payload, created_by, created_at
		  FROM demand_findings
		 WHERE demand_id = $1 AND account_id = $2
		 ORDER BY created_at, id`, demandID, accountID)
	if err != nil {
		return nil, Translate(err, "the demand's findings")
	}
	defer rows.Close()

	var findings []demand.Finding
	for rows.Next() {
		var f demand.Finding
		var payload []byte
		if err := rows.Scan(&f.ID, &f.AccountID, &f.DemandID, &f.ThreadID,
			&f.Title, &payload, &f.CreatedBy, &f.CreatedAt); err != nil {
			return nil, Translate(err, "a demand finding")
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &f.Payload)
		}
		findings = append(findings, f)
	}
	return findings, Translate(rows.Err(), "the demand's findings")
}

// ── state + event + idempotency, in a single transaction ─────────────────────

// emit publishes the demand's event into the platform's log.
//
// The aggregate and the aggregate_id are pinned HERE, not by the caller: every
// event of the demand — a thread message and a finding included — belongs to
// that demand's log (ADR-0006). `extra` adds to the payload what only exists
// after the INSERT (ids generated by the database), so the log stays rebuildable
// on its own.
func emit(ctx context.Context, tx pgx.Tx, accountID, demandID string, ev demand.Emission, extra map[string]any) error {
	payload := make(map[string]any, len(ev.Payload)+len(extra))
	for k, v := range ev.Payload {
		payload[k] = v
	}
	for k, v := range extra {
		payload[k] = v
	}
	return Emit(ctx, tx, ports.Event{
		AccountID: accountID, Aggregate: demand.Aggregate, AggregateID: demandID,
		Type: ev.Type, Payload: mustJSON(payload),
	})
}

// writeIdem performs the write with the idempotency key reserved INSIDE the
// same transaction as the state and the event.
//
// Reserving outside the transaction would open the classic window: the key stays
// marked, the transaction fails, and the legitimate repetition starts receiving a
// response that was never written. Here the reservation, the write and the event
// share one fate — they commit together or they vanish together.
//
// The same key with different content is a CONFLICT, not a repetition: without
// that a client bug would become silent corruption (ADR-0017).
func writeIdem[T any](ctx context.Context, pool *pgxpool.Pool, key string, ev demand.Emission, fn func(pgx.Tx) (*T, error)) (*T, error) {
	var out *T
	err := InTx(ctx, pool, func(tx pgx.Tx) error {
		if key != "" {
			replay, done, err := reserveIdem(ctx, tx, key, idem.Hash(ev.Type, string(mustJSON(ev.Payload))))
			if err != nil {
				return err
			}
			if done {
				var prev T
				if err := json.Unmarshal(replay, &prev); err != nil {
					return errs.Wrap(errs.KindInternal, err,
						"the stored idempotent response is unreadable")
				}
				out = &prev
				return nil
			}
		}
		result, err := fn(tx)
		if err != nil {
			return err
		}
		out = result
		return CompleteTx(ctx, tx, key, mustJSON(result))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func reserveIdem(ctx context.Context, tx pgx.Tx, key, hash string) ([]byte, bool, error) {
	var existingHash string
	var response []byte
	var completed *time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency (key, request_hash, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (key) DO UPDATE SET key = EXCLUDED.key
		RETURNING request_hash, response, completed_at`,
		key, hash, idem.DefaultTTL.String(),
	).Scan(&existingHash, &response, &completed)
	if err != nil {
		return nil, false, errs.Wrap(errs.KindInternal, err, "failure in the idempotency control")
	}
	if existingHash != hash {
		return nil, false, errs.Conflict("the idempotency key has already been used with different content")
	}
	if completed != nil {
		return response, true, nil
	}
	return nil, false, nil
}

var _ demand.Repository = (*DemandRepo)(nil)
