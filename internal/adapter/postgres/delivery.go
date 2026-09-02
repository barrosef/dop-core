package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// DeliveryRepo implements delivery.Repository. It is the ONLY place with
// delivery SQL — the domain never sees a query.
//
// Three things hold for this whole file:
//
//   - account_id goes into EVERY WHERE clause. Where the filter does not fit in
//     the table (the repository lives in project_repos, which has no account),
//     it comes through a join with projects. Multi-tenant isolation is a
//     constraint, not trust in the caller;
//   - every state change writes the event in the SAME transaction, through InTx
//   - Emit: a commit ⇒ state and event, or neither (ADR-0019);
//   - the idempotency key is looked up BEFORE writing and stored on the row,
//     with a partial unique index per account. Repeating the call returns the
//     same row instead of duplicating the effect (ADR-0017).
type DeliveryRepo struct{ pool *pgxpool.Pool }

func NewDeliveryRepo(pool *pgxpool.Pool) *DeliveryRepo { return &DeliveryRepo{pool: pool} }

// ─────────────────────────── evidence ────────────────────────────

const verificationCols = `id, account_id, demand_id::text, repo_id::text, commit_sha,
	kind::text, suite, outcome::text, total, passed, failed,
	sandbox_id, log_ref, detail, attempts, started_at, ended_at`

func scanVerification(row pgx.Row) (*delivery.VerificationRun, error) {
	var r delivery.VerificationRun
	var kind, outcome string
	var detail []byte
	var started *time.Time
	if err := row.Scan(&r.ID, &r.AccountID, &r.DemandID, &r.RepoID, &r.Commit,
		&kind, &r.Suite, &outcome, &r.Total, &r.Passed, &r.Failed,
		&r.SandboxID, &r.LogRef, &detail, &r.Attempts, &started, &r.EndedAt); err != nil {
		return nil, err
	}
	r.Kind = delivery.CheckKind(kind)
	r.Outcome = delivery.Outcome(outcome)
	if started != nil {
		r.StartedAt = *started
	}
	r.Detail = map[string]any{}
	_ = json.Unmarshal(detail, &r.Detail)
	return &r, nil
}

// RecordVerification records the run. Running the SAME suite again on the same
// commit updates the row and increments attempts — how many times it was tried
// is part of the evidence, not noise to hide.
func (d *DeliveryRepo) RecordVerification(ctx context.Context, run *delivery.VerificationRun, idemKey string) (*delivery.VerificationRun, error) {
	// started_at is optional: Go's zero instant is not SQL's NULL, and writing
	// "year 1" as a run's start would be false data.
	var startedAt any
	if !run.StartedAt.IsZero() {
		startedAt = run.StartedAt
	}

	var saved *delivery.VerificationRun
	err := InTx(ctx, d.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO verification_runs (account_id, demand_id, repo_id, commit_sha,
			       kind, suite, outcome, total, passed, failed, sandbox_id, log_ref,
			       detail, attempts, started_at, ended_at, idempotency_key)
			VALUES ($1, $2::uuid, $3::uuid, $4, $5::verification_kind, $6,
			        $7::verification_outcome, $8, $9, $10, $11, $12, $13, 1,
			        $14, $15, NULLIF($16,''))
			ON CONFLICT (account_id, demand_id, repo_id, commit_sha, kind, suite)
			DO UPDATE SET outcome = EXCLUDED.outcome, total = EXCLUDED.total,
			              passed = EXCLUDED.passed, failed = EXCLUDED.failed,
			              sandbox_id = EXCLUDED.sandbox_id, log_ref = EXCLUDED.log_ref,
			              detail = EXCLUDED.detail, ended_at = EXCLUDED.ended_at,
			              attempts = verification_runs.attempts + 1
			RETURNING `+verificationCols,
			run.AccountID, run.DemandID, run.RepoID, run.Commit,
			string(run.Kind), run.Suite, string(run.Outcome),
			run.Total, run.Passed, run.Failed, run.SandboxID, run.LogRef,
			mustJSON(run.Detail), startedAt, run.EndedAt, idemKey)
		var err error
		saved, err = scanVerification(row)
		if err != nil {
			return Translate(err, "a verification run")
		}
		// Each run's result is an event (ADR-0007 §1): it is what the timeline
		// and the spec's quality metrics feed on.
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "delivery", AggregateID: saved.DemandID,
			Type: "dop.delivery.verification.recorded",
			Payload: mustJSON(map[string]any{
				"run_id": saved.ID, "repo_id": saved.RepoID, "commit": saved.Commit,
				"kind": saved.Kind, "suite": saved.Suite, "outcome": saved.Outcome,
				"attempts": saved.Attempts, "log_ref": saved.LogRef,
			}),
		})
	})
	return saved, err
}

// EvidenceFor returns the runs of that exact commit. Absence is not an error:
// empty evidence is a legitimate answer — and it is the one that makes the queue
// refuse.
func (d *DeliveryRepo) EvidenceFor(ctx context.Context, accountID, demandID, repoID, commit string) (delivery.Evidence, error) {
	ev := delivery.Evidence{DemandID: demandID, RepoID: repoID, Commit: commit}
	rows, err := d.pool.Query(ctx, `
		SELECT `+verificationCols+`
		  FROM verification_runs
		 WHERE account_id = $1 AND demand_id = $2::uuid
		   AND repo_id = $3::uuid AND commit_sha = $4
		 ORDER BY kind, suite`, accountID, demandID, repoID, commit)
	if err != nil {
		return ev, Translate(err, "verification evidence")
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanVerification(rows)
		if err != nil {
			return ev, Translate(err, "verification evidence")
		}
		ev.Runs = append(ev.Runs, *r)
	}
	return ev, rows.Err()
}

// ─────────────────────────── pull requests ───────────────────────────

const prCols = `p.id, p.account_id, p.demand_id::text, p.repo_id::text, p.repo_name,
	p.source_branch, p.target_branch, p.head_commit, p.url, p.external_id,
	p.merged, p.has_conflict, p.reviewers, p.created_at, p.updated_at`

type reviewerRow struct {
	Name     string `json:"name"`
	Initials string `json:"initials"`
	Status   string `json:"status"`
}

func scanPR(row pgx.Row) (*delivery.PullRequest, error) {
	var pr delivery.PullRequest
	var reviewers []byte
	if err := row.Scan(&pr.ID, &pr.AccountID, &pr.DemandID, &pr.RepoID, &pr.Repo,
		&pr.SourceBranch, &pr.TargetBranch, &pr.HeadCommit, &pr.URL, &pr.ExternalID,
		&pr.Merged, &pr.HasConflict, &reviewers, &pr.CreatedAt, &pr.UpdatedAt); err != nil {
		return nil, err
	}
	var rows []reviewerRow
	_ = json.Unmarshal(reviewers, &rows)
	for _, r := range rows {
		pr.Reviewers = append(pr.Reviewers, delivery.Reviewer{
			Name: r.Name, Initials: r.Initials, Status: r.Status})
	}
	return &pr, nil
}

// ListPullRequests always filters by account; the project comes in through a
// join, because the PR→project link goes through the repository.
func (d *DeliveryRepo) ListPullRequests(ctx context.Context, accountID string, f delivery.PRFilter) ([]delivery.PullRequest, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+prCols+`
		  FROM pull_requests p
		  JOIN project_repos r ON r.id = p.repo_id
		 WHERE p.account_id = $1
		   -- NULLIF before the cast: an empty filter has to become NULL, not
		   -- ''::uuid. Postgres does not guarantee short-circuiting in an OR, and a cast
		   -- casting an empty string to uuid would bring the unfiltered query down.
		   AND ($2::text = '' OR p.demand_id  = NULLIF($2,'')::uuid)
		   AND ($3::text = '' OR r.project_id = NULLIF($3,'')::uuid)
		   AND ($4::text = '' OR p.repo_id    = NULLIF($4,'')::uuid)
		   AND (NOT $5::bool OR p.merged = false)
		 ORDER BY p.created_at DESC, p.id`,
		accountID, f.DemandID, f.ProjectID, f.RepoID, f.OnlyOpen)
	if err != nil {
		return nil, Translate(err, "pull requests")
	}
	defer rows.Close()

	var out []delivery.PullRequest
	for rows.Next() {
		pr, err := scanPR(rows)
		if err != nil {
			return nil, Translate(err, "pull requests")
		}
		out = append(out, *pr)
	}
	return out, rows.Err()
}

func (d *DeliveryRepo) PullRequestByID(ctx context.Context, accountID, id string) (*delivery.PullRequest, error) {
	pr, err := scanPR(d.pool.QueryRow(ctx,
		`SELECT `+prCols+` FROM pull_requests p WHERE p.id = $1 AND p.account_id = $2`, id, accountID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "pull request")
	}
	return pr, nil
}

// PullRequestOf returns (nil, nil) when there is no PR: whether the absence is
// an error is the domain's decision — and there it becomes "no green, no PR".
func (d *DeliveryRepo) PullRequestOf(ctx context.Context, accountID, demandID, repoID string) (*delivery.PullRequest, error) {
	pr, err := scanPR(d.pool.QueryRow(ctx, `
		SELECT `+prCols+`
		  FROM pull_requests p
		 WHERE p.account_id = $1 AND p.demand_id = $2::uuid AND p.repo_id = $3::uuid`,
		accountID, demandID, repoID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "pull request")
	}
	return pr, nil
}

// OpenPullRequest relies on the `assert_pr_tem_verde` trigger as the last
// backstop: the service has already refused earlier, with the message that says
// what is missing, but ADR-0007's rule must not depend on any specific code
// path. The trigger's exception comes back as failed_precondition through
// Translate.
func (d *DeliveryRepo) OpenPullRequest(ctx context.Context, pr *delivery.PullRequest, idemKey string) (*delivery.PullRequest, error) {
	if existing, err := d.prByIdemKey(ctx, pr.AccountID, idemKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	var saved *delivery.PullRequest
	err := InTx(ctx, d.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO pull_requests (account_id, demand_id, repo_id, repo_name,
			       source_branch, target_branch, head_commit, url, external_id,
			       reviewers, idempotency_key)
			VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, NULLIF($11,''))
			RETURNING `+prColsBare,
			pr.AccountID, pr.DemandID, pr.RepoID, pr.Repo,
			pr.SourceBranch, pr.TargetBranch, pr.HeadCommit, pr.URL, pr.ExternalID,
			mustJSON(reviewersJSON(pr.Reviewers)), idemKey)
		var err error
		saved, err = scanPR(row)
		if err != nil {
			return Translate(err, "pull request")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "delivery", AggregateID: saved.ID,
			Type: "dop.delivery.pull_request.opened",
			Payload: mustJSON(map[string]any{
				"demand_id": saved.DemandID, "repo_id": saved.RepoID,
				"url": saved.URL, "head_commit": saved.HeadCommit,
				"source_branch": saved.SourceBranch, "target_branch": saved.TargetBranch,
			}),
		})
	})
	return saved, err
}

// prColsBare is prCols's same set without the alias — an INSERT's RETURNING does
// not know the SELECT's `p` alias.
const prColsBare = `id, account_id, demand_id::text, repo_id::text, repo_name,
	source_branch, target_branch, head_commit, url, external_id,
	merged, has_conflict, reviewers, created_at, updated_at`

func reviewersJSON(rs []delivery.Reviewer) []reviewerRow {
	out := make([]reviewerRow, 0, len(rs))
	for _, r := range rs {
		out = append(out, reviewerRow{Name: r.Name, Initials: r.Initials, Status: r.Status})
	}
	return out
}

// ─────────────────────────── merge queue ────────────────────────────

const queueCols = `id, account_id, repo_id::text, demand_id::text, pull_request_id::text,
	seq, priority, state::text, overlapping_files, conflict, enqueued_at, updated_at`

type conflictRow struct {
	Files      []string  `json:"files"`
	BaseCommit string    `json:"base_commit"`
	Attempts   int       `json:"attempts"`
	Detail     string    `json:"detail"`
	ReportedAt time.Time `json:"reported_at"`
}

func scanQueueEntry(row pgx.Row) (*delivery.MergeQueueEntry, error) {
	var e delivery.MergeQueueEntry
	var state string
	var conflict []byte
	if err := row.Scan(&e.ID, &e.AccountID, &e.RepoID, &e.DemandID, &e.PullRequestID,
		&e.Seq, &e.Priority, &state, &e.OverlappingFiles, &conflict,
		&e.EnqueuedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.State = delivery.QueueState(state)
	if len(conflict) > 0 {
		var c conflictRow
		if json.Unmarshal(conflict, &c) == nil {
			e.Conflict = &delivery.ConflictReport{
				Files: c.Files, BaseCommit: c.BaseCommit, Attempts: c.Attempts,
				Detail: c.Detail, ReportedAt: c.ReportedAt,
			}
		}
	}
	return &e, nil
}

// QueueOfRepo returns the queue already in (priority, seq) — the same order the
// domain reapplies. The ORDER BY here is the index's convenience; the RULE of who
// goes first lives in the domain, where it is testable without a database.
func (d *DeliveryRepo) QueueOfRepo(ctx context.Context, accountID, repoID string, includeMerged bool) ([]delivery.MergeQueueEntry, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+queueCols+`
		  FROM merge_queue_entries
		 WHERE account_id = $1 AND repo_id = $2::uuid
		   AND ($3::bool OR state <> 'merged')
		 ORDER BY priority, seq`, accountID, repoID, includeMerged)
	if err != nil {
		return nil, Translate(err, "merge queue")
	}
	defer rows.Close()

	var out []delivery.MergeQueueEntry
	for rows.Next() {
		e, err := scanQueueEntry(rows)
		if err != nil {
			return nil, Translate(err, "merge queue")
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (d *DeliveryRepo) QueueEntryByID(ctx context.Context, accountID, id string) (*delivery.MergeQueueEntry, error) {
	e, err := scanQueueEntry(d.pool.QueryRow(ctx,
		`SELECT `+queueCols+` FROM merge_queue_entries WHERE id = $1 AND account_id = $2`,
		id, accountID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "a merge queue entry")
	}
	return e, nil
}

// Enqueue assigns the repository's sequence UNDER A LOCK on the repository's
// row.
//
// It is the lock that makes the order deterministic under concurrency: without
// it, two simultaneous enqueues would read the same MAX(seq) and one of the two
// would break on the UNIQUE (repo_id, seq) — the constraint would save the
// integrity, but at the price of an error the client would have to reprocess.
// The same SELECT confirms the repository belongs to THE ACCOUNT: project_repos
// has no account_id, the link comes through projects.
func (d *DeliveryRepo) Enqueue(ctx context.Context, e *delivery.MergeQueueEntry, idemKey string) (*delivery.MergeQueueEntry, error) {
	if existing, err := d.queueByIdemKey(ctx, e.AccountID, idemKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	var saved *delivery.MergeQueueEntry
	err := InTx(ctx, d.pool, func(tx pgx.Tx) error {
		var lockedRepo string
		err := tx.QueryRow(ctx, `
			SELECT r.id FROM project_repos r
			  JOIN projects p ON p.id = r.project_id
			 WHERE r.id = $1::uuid AND p.account_id = $2
			 FOR UPDATE OF r`, e.RepoID, e.AccountID).Scan(&lockedRepo)
		if NoRows(err) {
			return errs.NotFound("project repository")
		}
		if err != nil {
			return Translate(err, "project repository")
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO merge_queue_entries (account_id, repo_id, demand_id,
			       pull_request_id, seq, priority, state, overlapping_files,
			       enqueued_at, idempotency_key)
			VALUES ($1, $2::uuid, $3::uuid, $4::uuid,
			        (SELECT COALESCE(max(seq), 0) + 1 FROM merge_queue_entries WHERE repo_id = $2::uuid),
			        $5, $6::merge_queue_state, $7, $8, NULLIF($9,''))
			RETURNING `+queueCols,
			e.AccountID, e.RepoID, e.DemandID, e.PullRequestID,
			e.Priority, string(e.State), e.OverlappingFiles, e.EnqueuedAt, idemKey)
		saved, err = scanQueueEntry(row)
		if err != nil {
			// UNIQUE (repo_id, pull_request_id) becomes a 409: the same PR
			// going in twice is the client's retry, not new state.
			return Translate(err, "a merge queue entry")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "merge_queue", AggregateID: saved.ID,
			Type: "dop.delivery.merge.enqueued",
			Payload: mustJSON(map[string]any{
				"repo_id": saved.RepoID, "demand_id": saved.DemandID,
				"pull_request_id": saved.PullRequestID,
				"seq":             saved.Seq, "priority": saved.Priority,
			}),
		})
	})
	return saved, err
}

// SetQueueState moves the entry and emits the corresponding event.
//
// A conflict has an event type of its OWN — it is what feeds the attention box
// (ADR-0008 §2). A generic `state_changed` would force every consumer to inspect
// the payload to discover that there was somebody to be called.
func (d *DeliveryRepo) SetQueueState(ctx context.Context, accountID, entryID string, to delivery.QueueState, c *delivery.ConflictReport, idemKey string) (*delivery.MergeQueueEntry, error) {
	var saved *delivery.MergeQueueEntry
	err := InTx(ctx, d.pool, func(tx pgx.Tx) error {
		var conflict any
		if c != nil {
			conflict = mustJSON(conflictRow{
				Files: c.Files, BaseCommit: c.BaseCommit, Attempts: c.Attempts,
				Detail: c.Detail, ReportedAt: c.ReportedAt,
			})
		}
		row := tx.QueryRow(ctx, `
			UPDATE merge_queue_entries
			   SET state = $3::merge_queue_state,
			       conflict = COALESCE($4::jsonb, conflict),
			       idempotency_key = COALESCE(NULLIF($5,''), idempotency_key),
			       updated_at = now()
			 WHERE id = $1 AND account_id = $2
			 RETURNING `+queueCols,
			entryID, accountID, string(to), conflict, idemKey)
		var err error
		saved, err = scanQueueEntry(row)
		if err != nil {
			return Translate(err, "a merge queue entry")
		}

		tipo := "dop.delivery.merge.state_changed"
		payload := map[string]any{
			"repo_id": saved.RepoID, "demand_id": saved.DemandID,
			"pull_request_id": saved.PullRequestID, "state": saved.State,
			"seq": saved.Seq, "priority": saved.Priority,
		}
		if saved.State.NeedsHuman() && saved.Conflict != nil {
			tipo = "dop.delivery.merge.conflict_escalated"
			payload["files"] = saved.Conflict.Files
			payload["base_commit"] = saved.Conflict.BaseCommit
			payload["attempts"] = saved.Conflict.Attempts
			payload["detail"] = saved.Conflict.Detail
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "merge_queue", AggregateID: saved.ID,
			Type: tipo, Payload: mustJSON(payload),
		})
	})
	return saved, err
}

// ─────────────────────────── diretrizes ───────────────────────────

const directiveCols = `id, account_id, project_id::text, kind::text, summary, payload,
	affected_demands::text[], options, recommended, status::text,
	COALESCE(decided_option,''), COALESCE(rationale,''), COALESCE(decided_by::text,''),
	decided_at, created_at, updated_at`

// The JSON keys below are the SAME ones the `assert_diretriz_coordena_sem_pausar`
// trigger inspects. Diverging here would make the trigger let through an
// instruction it ought to bar — which is why they are declared in a single
// place.
type instructionRow struct {
	DemandID string         `json:"demand_id"`
	Action   string         `json:"action"`
	When     string         `json:"when,omitempty"`
	Payload  map[string]any `json:"payload,omitempty"`
}

type optionRow struct {
	Key          string           `json:"key"`
	Summary      string           `json:"summary"`
	Instructions []instructionRow `json:"instructions"`
}

func scanDirective(row pgx.Row) (*delivery.Directive, error) {
	var d delivery.Directive
	var kind, status string
	var payload, options []byte
	var option, rationale, decidedBy string
	var decidedAt *time.Time
	if err := row.Scan(&d.ID, &d.AccountID, &d.ProjectID, &kind, &d.Summary, &payload,
		&d.AffectedDemands, &options, &d.Recommended, &status,
		&option, &rationale, &decidedBy, &decidedAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	d.Kind = delivery.DirectiveKind(kind)
	d.Status = delivery.DirectiveStatus(status)
	d.Payload = map[string]any{}
	_ = json.Unmarshal(payload, &d.Payload)

	var opts []optionRow
	_ = json.Unmarshal(options, &opts)
	for _, o := range opts {
		opt := delivery.DirectiveOption{Key: o.Key, Summary: o.Summary}
		for _, i := range o.Instructions {
			opt.Instructions = append(opt.Instructions, delivery.Instruction{
				DemandID: i.DemandID, Action: delivery.DirectiveKind(i.Action),
				When: i.When, Payload: i.Payload,
			})
		}
		d.Options = append(d.Options, opt)
	}
	if decidedAt != nil {
		d.Decision = &delivery.Decision{
			Option: option, Rationale: rationale, DecidedBy: decidedBy, DecidedAt: *decidedAt,
		}
	}
	return &d, nil
}

func (d *DeliveryRepo) ListDirectives(ctx context.Context, accountID, projectID string) ([]delivery.Directive, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+directiveCols+`
		  FROM directives
		 WHERE account_id = $1 AND project_id = $2::uuid
		 ORDER BY status, created_at DESC`, accountID, projectID)
	if err != nil {
		return nil, Translate(err, "diretrizes")
	}
	defer rows.Close()

	var out []delivery.Directive
	for rows.Next() {
		dir, err := scanDirective(rows)
		if err != nil {
			return nil, Translate(err, "diretrizes")
		}
		out = append(out, *dir)
	}
	return out, rows.Err()
}

func (d *DeliveryRepo) DirectiveByID(ctx context.Context, accountID, id string) (*delivery.Directive, error) {
	dir, err := scanDirective(d.pool.QueryRow(ctx,
		`SELECT `+directiveCols+` FROM directives WHERE id = $1 AND account_id = $2`, id, accountID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "diretriz")
	}
	return dir, nil
}

// CreateDirective is the tech lead triggering the attention box with a
// ready-made decision item (ADR-0015 §3) — hence the event of its own.
func (d *DeliveryRepo) CreateDirective(ctx context.Context, dir *delivery.Directive, idemKey string) (*delivery.Directive, error) {
	if existing, err := d.directiveByIdemKey(ctx, dir.AccountID, idemKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	var saved *delivery.Directive
	err := InTx(ctx, d.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO directives (account_id, project_id, kind, summary, payload,
			       affected_demands, options, recommended, status, idempotency_key)
			VALUES ($1, $2::uuid, $3::directive_kind, $4, $5, $6::uuid[], $7, $8,
			        'proposed', NULLIF($9,''))
			RETURNING `+directiveCols,
			dir.AccountID, dir.ProjectID, string(dir.Kind), dir.Summary,
			mustJSON(dir.Payload), dir.AffectedDemands,
			mustJSON(optionsJSON(dir.Options)), dir.Recommended, idemKey)
		var err error
		saved, err = scanDirective(row)
		if err != nil {
			return Translate(err, "diretriz")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "directive", AggregateID: saved.ID,
			Type: "dop.delivery.directive.proposed",
			Payload: mustJSON(map[string]any{
				"project_id": saved.ProjectID, "kind": saved.Kind,
				"summary": saved.Summary, "recommended": saved.Recommended,
				"options": chavesDasOpcoes(saved.Options), "demands": saved.AffectedDemands,
			}),
		})
	})
	return saved, err
}

// DecideDirective records the decision AND applies, in the SAME transaction, the
// only coordination delivery knows how to carry out on its own: the preferred
// order in the queue (ADR-0015 §6).
//
// Note what the queue's UPDATE does and what it does NOT: it touches `priority`.
// There is no demand column to touch, no demand state to change — the
// coordination reorders the merge and the demand that lost its turn keeps
// running. The other instructions travel in the event, to whoever owns them.
func (d *DeliveryRepo) DecideDirective(ctx context.Context, accountID, id string, dec delivery.Decision, ins []delivery.Instruction, idemKey string) (*delivery.Directive, error) {
	var saved *delivery.Directive
	err := InTx(ctx, d.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE directives
			   SET status = 'decided', decided_option = $3, rationale = $4,
			       decided_by = NULLIF($5,'')::uuid, decided_at = $6,
			       idempotency_key = COALESCE(NULLIF($7,''), idempotency_key),
			       updated_at = now()
			 WHERE id = $1 AND account_id = $2 AND status = 'proposed'
			 RETURNING `+directiveCols,
			id, accountID, dec.Option, dec.Rationale, dec.DecidedBy, dec.DecidedAt, idemKey)
		var err error
		saved, err = scanDirective(row)
		if err != nil {
			return Translate(err, "diretriz")
		}

		for _, i := range ins {
			if i.Action != delivery.DirectiveMergeOrder {
				continue
			}
			priority, ok := numberOf(i.Payload["priority"])
			if !ok {
				continue
			}
			if _, err := tx.Exec(ctx, `
				UPDATE merge_queue_entries
				   SET priority = $3, updated_at = now()
				 WHERE account_id = $1 AND demand_id = $2::uuid AND state <> 'merged'`,
				accountID, i.DemandID, priority); err != nil {
				return Translate(err, "merge queue order")
			}
		}

		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "directive", AggregateID: saved.ID,
			Type: "dop.delivery.directive.decided",
			Payload: mustJSON(map[string]any{
				"project_id": saved.ProjectID, "kind": saved.Kind,
				"option": dec.Option, "rationale": dec.Rationale,
				"decided_by": dec.DecidedBy, "actor_kind": dec.ActorKind,
				// The instructions travel in the event: it is how the
				// coordination reaches the demands without delivery writing
				// into their state.
				"instructions": instructionsJSON(ins),
			}),
		})
	})
	return saved, err
}

// ─────────────────────────── auxiliares ───────────────────────────

func optionsJSON(opts []delivery.DirectiveOption) []optionRow {
	out := make([]optionRow, 0, len(opts))
	for _, o := range opts {
		out = append(out, optionRow{
			Key: o.Key, Summary: o.Summary, Instructions: instructionsJSON(o.Instructions),
		})
	}
	return out
}

func instructionsJSON(ins []delivery.Instruction) []instructionRow {
	out := make([]instructionRow, 0, len(ins))
	for _, i := range ins {
		out = append(out, instructionRow{
			DemandID: i.DemandID, Action: string(i.Action), When: i.When, Payload: i.Payload,
		})
	}
	return out
}

func chavesDasOpcoes(opts []delivery.DirectiveOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Key)
	}
	return out
}

// numberOf accepts whatever comes from the contract's Struct: JSON has no int,
// and the instruction's payload goes through jsonb on the way back.
func numberOf(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// ── re-reading by idempotency key ──
//
// The repetition is read BEFORE writing. The partial unique index per account is
// what guarantees two simultaneous attempts do not create two rows; this read is
// what makes the second attempt return the SAME response instead of a conflict
// the client would have to interpret.

func (d *DeliveryRepo) prByIdemKey(ctx context.Context, accountID, idemKey string) (*delivery.PullRequest, error) {
	if idemKey == "" {
		return nil, nil
	}
	pr, err := scanPR(d.pool.QueryRow(ctx,
		`SELECT `+prCols+` FROM pull_requests p
		  WHERE p.account_id = $1 AND p.idempotency_key = $2`, accountID, idemKey))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "pull request")
	}
	return pr, nil
}

func (d *DeliveryRepo) queueByIdemKey(ctx context.Context, accountID, idemKey string) (*delivery.MergeQueueEntry, error) {
	if idemKey == "" {
		return nil, nil
	}
	e, err := scanQueueEntry(d.pool.QueryRow(ctx,
		`SELECT `+queueCols+` FROM merge_queue_entries
		  WHERE account_id = $1 AND idempotency_key = $2`, accountID, idemKey))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "a merge queue entry")
	}
	return e, nil
}

func (d *DeliveryRepo) directiveByIdemKey(ctx context.Context, accountID, idemKey string) (*delivery.Directive, error) {
	if idemKey == "" {
		return nil, nil
	}
	dir, err := scanDirective(d.pool.QueryRow(ctx,
		`SELECT `+directiveCols+` FROM directives
		  WHERE account_id = $1 AND idempotency_key = $2`, accountID, idemKey))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "diretriz")
	}
	return dir, nil
}

var _ delivery.Repository = (*DeliveryRepo)(nil)
