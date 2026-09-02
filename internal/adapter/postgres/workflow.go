package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// WorkflowRepo implements workflow.Repository and workflow.Ancestry. It is the
// ONLY place with flow SQL — the domain never sees a query.
//
// Both ports live in the same type because the lineage is a query over the SAME
// tree tables the resolution already has to sweep. Splitting them into two
// adapters would duplicate the pool and would not really separate anything.
type WorkflowRepo struct{ pool *pgxpool.Pool }

func NewWorkflowRepo(pool *pgxpool.Pool) *WorkflowRepo { return &WorkflowRepo{pool: pool} }

// flowCols joins identity (flows) and frozen content (flow_versions). The flow
// the domain manipulates is always ONE version — never the flows row on its own,
// which has no stage at all.
const flowCols = `f.id, COALESCE(f.account_id::text,''), f.owner_scope::text,
	COALESCE(f.owner_id::text,''), v.name, COALESCE(v.description,''), v.version,
	v.stages, COALESCE(v.created_by::text,''), f.created_at, v.created_at`

// currentJoin ties the CURRENT version; frozenJoin ties a requested version.
const currentJoin = ` FROM flows f JOIN flow_versions v
	ON v.flow_id = f.id AND v.version = f.current_version`

// visibleToAccount is the READS' multi-tenant filter.
//
// The platform's catalog is included because it has no owner and applies to
// every account — it is the chain's level 0. Every WRITE uses a plain
// `f.account_id = $1`: no account writes into the catalog.
const visibleToAccount = ` WHERE (f.account_id = $1 OR f.owner_scope = 'platform')`

func scanFlow(row pgx.Row) (*workflow.Flow, error) {
	var (
		f      workflow.Flow
		scope  string
		stages []byte
	)
	if err := row.Scan(&f.ID, &f.AccountID, &scope, &f.OwnerID, &f.Name, &f.Description,
		&f.Version, &stages, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, err
	}
	f.OwnerScope = workflow.Scope(scope)
	f.Stages = decodeStages(stages)
	return &f, nil
}

// ── reads ────────────────────────────────────────────────────────────────────

func (r *WorkflowRepo) List(ctx context.Context, accountID string, scope workflow.Scope, ownerID string) ([]workflow.Flow, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+flowCols+currentJoin+visibleToAccount+`
		  AND ($2 = '' OR f.owner_scope::text = $2)
		  AND ($3 = '' OR COALESCE(f.owner_id::text,'') = $3)
		ORDER BY f.owner_scope, v.name`, accountID, string(scope), ownerID)
	if err != nil {
		return nil, Translate(err, "flows")
	}
	defer rows.Close()

	var out []workflow.Flow
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, Translate(err, "flows")
		}
		out = append(out, *f)
	}
	return out, Translate(rows.Err(), "flows")
}

func (r *WorkflowRepo) ByID(ctx context.Context, accountID, id string) (*workflow.Flow, error) {
	f, err := scanFlow(r.pool.QueryRow(ctx,
		`SELECT `+flowCols+currentJoin+visibleToAccount+` AND f.id = $2`, accountID, id))
	if err != nil {
		return nil, Translate(err, "flow")
	}
	return f, nil
}

// VersionOf reads a frozen version. It does not go through current_version on
// purpose: whoever asks for version 3 wants 3, even if the flow is already on 7
// — it is how a running demand keeps seeing what it signed up to.
func (r *WorkflowRepo) VersionOf(ctx context.Context, accountID, id string, version int32) (*workflow.Flow, error) {
	f, err := scanFlow(r.pool.QueryRow(ctx, `SELECT `+flowCols+`
		  FROM flows f JOIN flow_versions v ON v.flow_id = f.id
		 WHERE (f.account_id = $1 OR f.owner_scope = 'platform')
		   AND f.id = $2 AND v.version = $3`, accountID, id, version))
	if err != nil {
		return nil, Translate(err, "the flow's version")
	}
	return f, nil
}

// ByOwners brings the flows of every level of the chain in one sweep. See the
// comment on the port: the resolution happens on every demand opening.
func (r *WorkflowRepo) ByOwners(ctx context.Context, accountID string, refs []workflow.ScopeRef) ([]workflow.Flow, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	scopes := make([]string, 0, len(refs))
	owners := make([]string, 0, len(refs))
	for _, ref := range refs {
		scopes = append(scopes, string(ref.Scope))
		owners = append(owners, ref.ID)
	}
	rows, err := r.pool.Query(ctx, `SELECT `+flowCols+currentJoin+visibleToAccount+`
		  AND (f.owner_scope::text, COALESCE(f.owner_id::text,''))
		      IN (SELECT * FROM unnest($2::text[], $3::text[]))`,
		accountID, scopes, owners)
	if err != nil {
		return nil, Translate(err, "the chain's flows")
	}
	defer rows.Close()

	var out []workflow.Flow
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, Translate(err, "the chain's flows")
		}
		out = append(out, *f)
	}
	return out, Translate(rows.Err(), "the chain's flows")
}

// ── writes ───────────────────────────────────────────────────────────────────

// Create writes the flow and version 1 in the event's SAME transaction
// (ADR-0019).
//
// The repetition is resolved by the idempotency key and not by "query before
// inserting": another request fits between the query and the insert, and the
// result would be two flows at the same level.
func (r *WorkflowRepo) Create(ctx context.Context, f *workflow.Flow, idempotencyKey string) (*workflow.Flow, error) {
	var saved *workflow.Flow
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		id, created, err := insertFlowRow(ctx, tx, f, idempotencyKey)
		if err != nil {
			return err
		}
		if !created {
			saved, err = flowByKey(ctx, tx, idempotencyKey)
			return err
		}
		if _, err := insertVersion(ctx, tx, id, 1, f, idempotencyKey+":1"); err != nil {
			return err
		}
		saved, err = loadFlow(ctx, tx, f.AccountID, id)
		if err != nil {
			return err
		}
		return Emit(ctx, tx, flowEvent("dop.workflow.flow.created", saved))
	})
	return saved, err
}

// AppendVersion writes the next version WITHOUT touching the previous one — the
// database's trigger would refuse anyway, and it is good that it does: the
// protection has to hold for the path nobody anticipated too.
func (r *WorkflowRepo) AppendVersion(ctx context.Context, accountID string, f *workflow.Flow, baseVersion int32, idempotencyKey string) (*workflow.Flow, error) {
	var saved *workflow.Flow
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// FOR UPDATE serializes two simultaneous edits: without the lock, both
		// would read version 3 and both would try to write 4.
		var current int32
		err := tx.QueryRow(ctx,
			`SELECT current_version FROM flows WHERE id = $1 AND account_id = $2 FOR UPDATE`,
			f.ID, accountID).Scan(&current)
		if err != nil {
			return Translate(err, "flow")
		}
		if current != baseVersion {
			return errs.Conflict("the flow is already on version %d; the change was written against %d",
				current, baseVersion)
		}

		next := current + 1
		created, err := insertVersion(ctx, tx, f.ID, next, f, idempotencyKey)
		if err != nil {
			return err
		}
		if !created {
			// An identical resend: it returns the version already written.
			saved, err = flowVersionByKey(ctx, tx, idempotencyKey)
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE flows SET current_version = $3, updated_at = $4 WHERE id = $1 AND account_id = $2`,
			f.ID, accountID, next, f.UpdatedAt); err != nil {
			return Translate(err, "flow")
		}
		saved, err = loadFlow(ctx, tx, accountID, f.ID)
		if err != nil {
			return err
		}
		return Emit(ctx, tx, flowEvent("dop.workflow.flow.version.created", saved))
	})
	return saved, err
}

// Promote publishes the content at the target level. If the target already has
// a flow, the content goes in as a NEW version of it — the target's flow is not
// replaced, it is versioned, for the usual reason: somebody may be following the
// current version.
func (r *WorkflowRepo) Promote(ctx context.Context, accountID string, src *workflow.Flow, target workflow.ScopeRef, idempotencyKey string) (*workflow.Flow, error) {
	var saved *workflow.Flow
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			targetID string
			current  int32
		)
		err := tx.QueryRow(ctx, `
			SELECT id, current_version FROM flows
			 WHERE account_id = $1 AND owner_scope = $2::flow_scope AND owner_id = $3
			 FOR UPDATE`, accountID, string(target.Scope), target.ID).Scan(&targetID, &current)

		promoted := *src
		promoted.AccountID = accountID
		promoted.OwnerScope, promoted.OwnerID = target.Scope, target.ID

		switch {
		case NoRows(err):
			promoted.ID = ""
			promoted.Version = 1
			promoted.CreatedAt = src.UpdatedAt
			id, created, err := insertFlowRow(ctx, tx, &promoted, idempotencyKey)
			if err != nil {
				return err
			}
			if !created {
				saved, err = flowByKey(ctx, tx, idempotencyKey)
				return err
			}
			if _, err := insertVersion(ctx, tx, id, 1, &promoted, idempotencyKey+":1"); err != nil {
				return err
			}
			targetID = id
		case err != nil:
			return Translate(err, "the target level's flow")
		default:
			promoted.ID = targetID
			promoted.Version = current + 1
			created, err := insertVersion(ctx, tx, targetID, promoted.Version, &promoted, idempotencyKey)
			if err != nil {
				return err
			}
			if !created {
				saved, err = flowVersionByKey(ctx, tx, idempotencyKey)
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE flows SET current_version = $3, updated_at = $4 WHERE id = $1 AND account_id = $2`,
				targetID, accountID, promoted.Version, src.UpdatedAt); err != nil {
				return Translate(err, "the target level's flow")
			}
		}

		saved, err = loadFlow(ctx, tx, accountID, targetID)
		if err != nil {
			return err
		}
		ev := flowEvent("dop.workflow.flow.promoted", saved)
		ev.Payload = mustJSON(map[string]any{
			"name": saved.Name, "version": saved.Version,
			"owner_scope": string(saved.OwnerScope), "owner_id": saved.OwnerID,
			// The promotion's provenance: without it, nobody explains where the
			// flow that came to apply to the whole level came from.
			"promoted_from_flow_id": src.ID,
			"promoted_from_scope":   string(src.OwnerScope),
			"promoted_from_owner":   src.OwnerID,
			"promoted_from_version": src.Version,
		})
		return Emit(ctx, tx, ev)
	})
	return saved, err
}

// ── linhagem (workflow.Ancestry) ─────────────────────────────────────────────

// ChainOf returns the chain of levels above the target, from the most generic to
// the most specific. Each hop filters by account: another account's id simply
// does not exist from here, and comes back as NotFound.
func (r *WorkflowRepo) ChainOf(ctx context.Context, accountID string, target workflow.ScopeRef) ([]workflow.ScopeRef, error) {
	base := []workflow.ScopeRef{{Scope: workflow.ScopePlatform}}
	switch target.Scope {
	case workflow.ScopePlatform:
		return base, nil
	case workflow.ScopeAccount:
		if target.ID != accountID {
			return nil, errs.NotFound("account %s", target.ID)
		}
		return append(base, workflow.ScopeRef{Scope: workflow.ScopeAccount, ID: accountID}), nil
	}
	base = append(base, workflow.ScopeRef{Scope: workflow.ScopeAccount, ID: accountID})

	switch target.Scope {
	case workflow.ScopeWorkspace:
		var id string
		if err := r.pool.QueryRow(ctx,
			`SELECT id FROM workspaces WHERE id = $1 AND account_id = $2`,
			target.ID, accountID).Scan(&id); err != nil {
			return nil, Translate(err, "workspace")
		}
		return append(base, workflow.ScopeRef{Scope: workflow.ScopeWorkspace, ID: id}), nil

	case workflow.ScopeProject:
		var workspaceID string
		if err := r.pool.QueryRow(ctx,
			`SELECT workspace_id FROM projects WHERE id = $1 AND account_id = $2`,
			target.ID, accountID).Scan(&workspaceID); err != nil {
			return nil, Translate(err, "project")
		}
		return append(base,
			workflow.ScopeRef{Scope: workflow.ScopeWorkspace, ID: workspaceID},
			workflow.ScopeRef{Scope: workflow.ScopeProject, ID: target.ID}), nil

	case workflow.ScopeDemand:
		return r.demandChain(ctx, accountID, base, target.ID)
	}
	return nil, errs.Invalid("unknown level: %q", target.Scope)
}

// demandChain climbs from the demand up to the account.
//
// The demands table arrives in ANOTHER domain's migration, which has not landed
// in this database yet. Asking about its existence before querying trades a
// "relation does not exist" (which reaches the user as an internal failure) for
// a refusal that says what is missing — and it disappears on its own when the
// table is born.
func (r *WorkflowRepo) demandChain(ctx context.Context, accountID string, base []workflow.ScopeRef, demandID string) ([]workflow.ScopeRef, error) {
	var ready bool
	if err := r.pool.QueryRow(ctx,
		`SELECT to_regclass('public.demands') IS NOT NULL`).Scan(&ready); err != nil {
		return nil, Translate(err, "demand")
	}
	if !ready {
		return nil, errs.Precondition(
			"the demand domain does not exist in this database yet: resolve the flow through the project until the demand migration is applied")
	}
	var projectID, workspaceID string
	if err := r.pool.QueryRow(ctx, `
		SELECT d.project_id, p.workspace_id
		  FROM demands d JOIN projects p ON p.id = d.project_id
		 WHERE d.id = $1 AND d.account_id = $2`, demandID, accountID).
		Scan(&projectID, &workspaceID); err != nil {
		return nil, Translate(err, "demand")
	}
	return append(base,
		workflow.ScopeRef{Scope: workflow.ScopeWorkspace, ID: workspaceID},
		workflow.ScopeRef{Scope: workflow.ScopeProject, ID: projectID},
		workflow.ScopeRef{Scope: workflow.ScopeDemand, ID: demandID}), nil
}

// ── auxiliares ───────────────────────────────────────────────────────────────

// insertFlowRow writes the flow's identity. created=false means the idempotency
// key had already been used — a repetition, not an error.
func insertFlowRow(ctx context.Context, tx pgx.Tx, f *workflow.Flow, key string) (string, bool, error) {
	var accountID, ownerID any
	if f.AccountID != "" {
		accountID = f.AccountID
	}
	if f.OwnerID != "" {
		ownerID = f.OwnerID
	}
	var createdBy any
	if f.CreatedBy != "" {
		createdBy = f.CreatedBy
	}

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO flows (account_id, owner_scope, owner_id, current_version,
		                   idempotency_key, created_by, created_at, updated_at)
		VALUES ($1, $2::flow_scope, $3, 1, $4, $5, $6, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		accountID, string(f.OwnerScope), ownerID, key, createdBy, f.CreatedAt).Scan(&id)
	if NoRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, Translate(err, "flow")
	}
	return id, true, nil
}

func insertVersion(ctx context.Context, tx pgx.Tx, flowID string, version int32, f *workflow.Flow, key string) (bool, error) {
	var createdBy any
	if f.CreatedBy != "" {
		createdBy = f.CreatedBy
	}
	var got int32
	err := tx.QueryRow(ctx, `
		INSERT INTO flow_versions (flow_id, version, name, description, stages,
		                           idempotency_key, created_by, created_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING version`,
		flowID, version, f.Name, f.Description, encodeStages(f.Stages), key, createdBy, f.UpdatedAt).
		Scan(&got)
	if NoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, Translate(err, "the flow's version")
	}
	return true, nil
}

func loadFlow(ctx context.Context, tx pgx.Tx, accountID, id string) (*workflow.Flow, error) {
	f, err := scanFlow(tx.QueryRow(ctx,
		`SELECT `+flowCols+currentJoin+visibleToAccount+` AND f.id = $2`, accountID, id))
	if err != nil {
		return nil, Translate(err, "flow")
	}
	return f, nil
}

// flowByKey and flowVersionByKey are the repetition's return: the already
// written key points at what the caller wanted to create, and returning that is
// what makes the write genuinely idempotent (ADR-0017).
func flowByKey(ctx context.Context, tx pgx.Tx, key string) (*workflow.Flow, error) {
	f, err := scanFlow(tx.QueryRow(ctx, `SELECT `+flowCols+currentJoin+
		` WHERE f.idempotency_key = $1`, key))
	if err != nil {
		return nil, Translate(err, "flow")
	}
	return f, nil
}

func flowVersionByKey(ctx context.Context, tx pgx.Tx, key string) (*workflow.Flow, error) {
	f, err := scanFlow(tx.QueryRow(ctx, `SELECT `+flowCols+
		` FROM flows f JOIN flow_versions v ON v.flow_id = f.id
		  WHERE v.idempotency_key = $1`, key))
	if err != nil {
		return nil, Translate(err, "the flow's version")
	}
	return f, nil
}

func flowEvent(kind string, f *workflow.Flow) ports.Event {
	return ports.Event{
		AccountID: f.AccountID, Aggregate: "workflow", AggregateID: f.ID, Type: kind,
		Payload: mustJSON(map[string]any{
			"name": f.Name, "version": f.Version,
			"owner_scope": string(f.OwnerScope), "owner_id": f.OwnerID,
			"stages": len(f.Stages),
		}),
	}
}

// stageDoc is the shape in the database. It exists separately from the domain
// type on purpose: renaming a domain field must not silently rewrite the JSON of
// versions that have been frozen for months.
type stageDoc struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Artifacts []string `json:"artifacts"`
	Gate      string   `json:"gate"`
	Subtypes  []string `json:"subtypes"`
}

func encodeStages(stages []workflow.StageSpec) []byte {
	docs := make([]stageDoc, 0, len(stages))
	for _, st := range stages {
		artifacts := make([]string, 0, len(st.Artifacts))
		for _, a := range st.Artifacts {
			artifacts = append(artifacts, string(a))
		}
		subtypes := st.Subtypes
		if subtypes == nil {
			subtypes = []string{}
		}
		docs = append(docs, stageDoc{
			Key: st.Key, Name: st.Name, Type: string(st.Type),
			Artifacts: artifacts, Gate: string(st.Gate), Subtypes: subtypes,
		})
	}
	return mustJSON(docs)
}

func decodeStages(raw []byte) []workflow.StageSpec {
	if len(raw) == 0 {
		return nil
	}
	var docs []stageDoc
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil
	}
	out := make([]workflow.StageSpec, 0, len(docs))
	for _, d := range docs {
		artifacts := make([]workflow.ArtifactKind, 0, len(d.Artifacts))
		for _, a := range d.Artifacts {
			artifacts = append(artifacts, workflow.ArtifactKind(a))
		}
		out = append(out, workflow.StageSpec{
			Key: d.Key, Name: d.Name, Type: workflow.StageType(d.Type),
			Artifacts: artifacts, Gate: workflow.GateKind(d.Gate), Subtypes: d.Subtypes,
		})
	}
	return out
}

var (
	_ workflow.Repository = (*WorkflowRepo)(nil)
	_ workflow.Ancestry   = (*WorkflowRepo)(nil)
)
