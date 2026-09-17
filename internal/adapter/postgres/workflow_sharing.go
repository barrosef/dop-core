// Package postgres — the flow-sharing adapter: everything that crosses, or
// prepares to cross, an account boundary (migrations 0020/0021).
//
// It lives apart from WorkflowRepo the same way the port does: the invariants
// here are about publication, grant and revocation, not about a flow's own
// identity and versions.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/domain/workflow"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// WorkflowSharing implements workflow.SharingRepository.
type WorkflowSharing struct{ pool *pgxpool.Pool }

func NewWorkflowSharing(pool *pgxpool.Pool) *WorkflowSharing { return &WorkflowSharing{pool: pool} }

var _ workflow.SharingRepository = (*WorkflowSharing)(nil)

// ── publications ─────────────────────────────────────────────────────────────

// CreatePublication writes a publication. The unique key that absorbs a retry
// is the table's own natural key — (account_id, slug, version) — because
// flow_publications carries no idempotency_key column of its own: publishing
// the same version under the same name twice IS the repeat, by definition.
//
// `DO UPDATE SET slug = EXCLUDED.slug` is a no-op write that exists only so
// RETURNING fires on the conflict path; a plain DO NOTHING returns no row and
// the retry would look like a failure.
func (r *WorkflowSharing) CreatePublication(ctx context.Context, p *workflow.Publication, key string) (*workflow.Publication, error) {
	const q = `
INSERT INTO flow_publications (flow_id, account_id, slug, version, notes, published_by)
VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
ON CONFLICT (account_id, slug, version) DO UPDATE SET slug = EXCLUDED.slug
RETURNING id, flow_id, account_id, slug, version, COALESCE(notes, ''), published_at, withdrawn_at`
	var publishedBy any
	if p.PublishedBy != "" {
		publishedBy = p.PublishedBy
	}
	var out workflow.Publication
	var withdrawn *time.Time
	err := r.pool.QueryRow(ctx, q, p.FlowID, p.AccountID, p.Slug, p.Version, p.Notes, publishedBy).
		Scan(&out.ID, &out.FlowID, &out.AccountID, &out.Slug, &out.Version, &out.Notes, &out.PublishedAt, &withdrawn)
	if err != nil {
		return nil, Translate(err, "publishing the flow")
	}
	if withdrawn != nil {
		out.WithdrawnAt = *withdrawn
	}
	// key is accepted for interface parity with the rest of this adapter's
	// writes; the natural key above (account_id, slug, version) is what
	// actually absorbs the retry, since the table carries no idempotency_key
	// column of its own.
	return &out, nil
}

func (r *WorkflowSharing) PublicationByID(ctx context.Context, accountID, id string) (*workflow.Publication, error) {
	const q = `
SELECT id, flow_id, account_id, slug, version, COALESCE(notes, ''), published_at, withdrawn_at
  FROM flow_publications
 WHERE id = $1 AND account_id = $2`
	var out workflow.Publication
	var withdrawn *time.Time
	err := r.pool.QueryRow(ctx, q, id, accountID).
		Scan(&out.ID, &out.FlowID, &out.AccountID, &out.Slug, &out.Version, &out.Notes, &out.PublishedAt, &withdrawn)
	if err != nil {
		return nil, Translate(err, "publication")
	}
	if withdrawn != nil {
		out.WithdrawnAt = *withdrawn
	}
	return &out, nil
}

// Withdraw is written to succeed on a publication ALREADY withdrawn — the
// domain's own contract (Withdraw is a state, not an event) — while a
// publication that does not exist under this account is still NotFound.
// COALESCE keeps a second withdrawal from moving the timestamp: the first
// call is the one that matters, a retry must not rewrite it.
func (r *WorkflowSharing) Withdraw(ctx context.Context, accountID, id string, at time.Time) error {
	const q = `
UPDATE flow_publications SET withdrawn_at = COALESCE(withdrawn_at, $3)
 WHERE id = $1 AND account_id = $2
RETURNING id`
	var got string
	err := r.pool.QueryRow(ctx, q, id, accountID, at).Scan(&got)
	if err != nil {
		return Translate(err, "publication")
	}
	return nil
}

// ResolvePublication is the ONE deliberate crossing: the join on flow_shares
// is what authorises it. No grant, no row, no decision left for the code to
// get wrong — a caller with no share gets exactly what a caller asking about
// a flow that does not exist would get.
func (r *WorkflowSharing) ResolvePublication(ctx context.Context, callerAccountID string, ref workflow.PublicationRef) (*workflow.Publication, error) {
	const q = `
SELECT p.id, p.flow_id, p.account_id, p.slug, p.version, COALESCE(p.notes, ''),
       COALESCE(p.published_by::text, ''), p.published_at
  FROM flow_publications p
  JOIN accounts a    ON a.id = p.account_id
  JOIN flow_shares s ON s.publication_id = p.id
 WHERE a.handle = $1
   AND p.slug   = $2
   AND ($3::int IS NULL OR p.version = $3)
   AND p.withdrawn_at IS NULL
   AND s.to_account_id = $4
   AND s.revoked_at IS NULL
 ORDER BY p.version DESC
 LIMIT 1`
	var version *int32
	if ref.Pinned() {
		v := ref.Version
		version = &v
	}
	var out workflow.Publication
	err := r.pool.QueryRow(ctx, q, ref.Handle, ref.Slug, version, callerAccountID).
		Scan(&out.ID, &out.FlowID, &out.AccountID, &out.Slug, &out.Version, &out.Notes,
			&out.PublishedBy, &out.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Never Permission: whether the flow exists in another account is not
		// something an outsider gets to learn — the refusal says only that no
		// flow resolves to this reference, for THIS caller.
		return nil, errs.NotFound("no flow published as %s", ref)
	}
	if err != nil {
		return nil, Translate(err, "resolving the reference")
	}
	// The row this query returns is, by construction, never withdrawn — the
	// WHERE clause already filters that out.
	return &out, nil
}

// ── shares ───────────────────────────────────────────────────────────────────

// CreateShare follows CreatePublication's shape: the unique key that absorbs a
// retry is flow_shares' own natural key, (publication_id, to_account_id) —
// there is no idempotency_key column here either. A second grant to the same
// ACTIVE account/publication pair comes back as the row already there, exactly
// like a repeated publish comes back as the version already published.
//
// The ON CONFLICT target carries `WHERE revoked_at IS NULL` because the
// unique index it matches (migration 0022) is partial: a REVOKED row does not
// occupy the key, so granting again after a revoke does not collide at all —
// it inserts a fresh row, which is exactly the audit trail a re-grant should
// leave (the old, revoked row stays, a new active one appears alongside it).
func (r *WorkflowSharing) CreateShare(ctx context.Context, s *workflow.Share, key string) (*workflow.Share, error) {
	const q = `
INSERT INTO flow_shares (publication_id, to_account_id, revocation_policy, granted_by, granted_at)
VALUES ($1, $2, $3::revocation_policy, $4, $5)
ON CONFLICT (publication_id, to_account_id) WHERE revoked_at IS NULL
DO UPDATE SET to_account_id = EXCLUDED.to_account_id
RETURNING id, publication_id, to_account_id, revocation_policy::text,
          COALESCE(granted_by::text, ''), granted_at, revoked_at`
	var grantedBy any
	if s.GrantedBy != "" {
		grantedBy = s.GrantedBy
	}
	var out workflow.Share
	var policy string
	var revokedAt *time.Time
	err := r.pool.QueryRow(ctx, q, s.PublicationID, s.ToAccountID, string(s.RevocationPolicy), grantedBy, s.GrantedAt).
		Scan(&out.ID, &out.PublicationID, &out.ToAccountID, &policy, &out.GrantedBy, &out.GrantedAt, &revokedAt)
	if err != nil {
		return nil, Translate(err, "granting the flow")
	}
	out.RevocationPolicy = workflow.RevocationPolicy(policy)
	if revokedAt != nil {
		out.RevokedAt = *revokedAt
	}
	// key: see the comment on CreatePublication — the natural key (publication_id,
	// to_account_id) is what absorbs the retry here.
	return &out, nil
}

func (r *WorkflowSharing) ShareByID(ctx context.Context, accountID, id string) (*workflow.Share, error) {
	const q = `
SELECT s.id, s.publication_id, s.to_account_id, s.revocation_policy::text, COALESCE(s.granted_by::text, ''), s.granted_at, s.revoked_at
  FROM flow_shares s
  JOIN flow_publications p ON p.id = s.publication_id
 WHERE s.id = $1 AND p.account_id = $2`
	return scanShare(r.pool.QueryRow(ctx, q, id, accountID))
}

func (r *WorkflowSharing) SharesOfPublication(ctx context.Context, accountID, publicationID string) ([]workflow.Share, error) {
	// The publication has to be THIS account's — otherwise an id that belongs to
	// somebody else would leak whether it exists through an empty-vs-error
	// difference.
	if _, err := r.PublicationByID(ctx, accountID, publicationID); err != nil {
		return nil, err
	}
	const q = `
SELECT id, publication_id, to_account_id, revocation_policy::text, COALESCE(granted_by::text, ''), granted_at, revoked_at
  FROM flow_shares
 WHERE publication_id = $1
 ORDER BY granted_at`
	rows, err := r.pool.Query(ctx, q, publicationID)
	if err != nil {
		return nil, Translate(err, "the publication's grants")
	}
	defer rows.Close()

	var out []workflow.Share
	for rows.Next() {
		s, err := scanShare(rows)
		if err != nil {
			return nil, Translate(err, "the publication's grants")
		}
		out = append(out, *s)
	}
	return out, Translate(rows.Err(), "the publication's grants")
}

func scanShare(row pgx.Row) (*workflow.Share, error) {
	var out workflow.Share
	var policy string
	var revokedAt *time.Time
	if err := row.Scan(&out.ID, &out.PublicationID, &out.ToAccountID, &policy,
		&out.GrantedBy, &out.GrantedAt, &revokedAt); err != nil {
		return nil, Translate(err, "grant")
	}
	out.RevocationPolicy = workflow.RevocationPolicy(policy)
	if revokedAt != nil {
		out.RevokedAt = *revokedAt
	}
	return &out, nil
}

// ── revocation (R1) ──────────────────────────────────────────────────────────

// RevokeShare does the WHOLE revocation in one transaction: the share, the
// copies rev.Adoptions reaches, their adoption records, and both events
// (ADR-0014) — the same InTx/Emit pattern every other write in this package,
// and the workflow package beside it, already follows; see db.go and
// outbox.go for where the pattern itself is defined.
//
// Under `prospective`, rev.Adoptions is empty: the loop below simply does not
// run, and the two events still fire. That is not a special case in the code —
// it falls out of "the domain decided what the policy reaches, and here it
// reaches nothing beyond the grant".
func (r *WorkflowSharing) RevokeShare(ctx context.Context, accountID string, rev workflow.Revocation) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// The share itself. The join on flow_publications is the tenant filter:
		// a share id that belongs to another account's publication affects
		// nothing here, the same as everywhere else in this adapter.
		var shareID string
		err := tx.QueryRow(ctx, `
			UPDATE flow_shares s SET revoked_at = $2
			  FROM flow_publications p
			 WHERE s.id = $1 AND p.id = s.publication_id AND p.account_id = $3
			   AND s.revoked_at IS NULL
			RETURNING s.id`,
			rev.ShareID, rev.At, accountID).Scan(&shareID)
		if err != nil {
			return Translate(err, "the grant being revoked")
		}

		// The copies this policy reaches — empty under `prospective`. Both the
		// adoption record (the publisher's index) and the copy itself
		// (flows.revoked_at, in the OTHER account) get marked: never deleted,
		// because the adopter's own edits and the audit of a demand that
		// already ran under it have to survive.
		flowIDs := make([]string, 0, len(rev.Adoptions))
		for _, a := range rev.Adoptions {
			// ad.by_account_id = $4 (rev.ToAccountID) is the SECOND tenancy
			// proof, alongside p.account_id = $3: without it, a caller who
			// passes a CONSISTENT pair — same publication, same owner,
			// correctly paired adoption/flow — but belonging to a DIFFERENT
			// granted account would pass every other check here, and a third
			// company's copy would be marked revoked. Only rev.Adoptions being
			// built from the domain's own filter (Service.RevokeShare) kept
			// that from happening before; this is the SQL half of the same
			// guard, the way ad.flow_id = $5 below is the SQL half of the
			// mismatched-pair guard.
			ct, err := tx.Exec(ctx, `
				UPDATE flow_adoptions ad SET revoked_at = $2
				  FROM flow_publications p
				 WHERE ad.id = $1 AND ad.publication_id = p.id AND p.account_id = $3
				   AND ad.by_account_id = $4 AND ad.revoked_at IS NULL`,
				a.ID, rev.At, accountID, rev.ToAccountID)
			if err != nil {
				return Translate(err, "the adoption record")
			}
			if ct.RowsAffected() == 0 {
				// Not found under this account, or already revoked: either way
				// a revocation that silently reached zero rows is worse than one
				// that fails — the caller asked for something specific to
				// happen and nothing did.
				return errs.NotFound("the adoption record %s, for this account's publication", a.ID)
			}

			// No account filter on the FLOW itself on purpose: the copy lives
			// in the OTHER account by design (flow_adoptions.flow_id carries no
			// FK, exactly so one account's cascade cannot reach into another's
			// rows) — this is the one write in the adapter that crosses the
			// boundary. What authorises it is not the caller-supplied a.FlowID
			// taken on faith, but `ad.flow_id = $5`: the WHERE clause proves,
			// IN SQL, that flow $5 is the one adoption $1 actually points at —
			// a mismatched pair matches no row instead of silently revoking
			// whichever flow the caller named. `ad.by_account_id = $4` is the
			// same tenancy proof as the UPDATE above, for the same reason: the
			// adoption row alone does not prove it belongs to THIS revocation's
			// grantee.
			ct, err = tx.Exec(ctx, `
				UPDATE flows f SET revoked_at = $2
				  FROM flow_adoptions ad
				  JOIN flow_publications p ON p.id = ad.publication_id
				 WHERE f.id = $5 AND ad.id = $1 AND ad.flow_id = $5
				   AND p.account_id = $3 AND ad.by_account_id = $4 AND f.revoked_at IS NULL`,
				a.ID, rev.At, accountID, rev.ToAccountID, a.FlowID)
			if err != nil {
				return Translate(err, "the derived copy")
			}
			if ct.RowsAffected() == 0 {
				return errs.NotFound(
					"flow %s as the copy adoption %s points at, for this account's publication", a.FlowID, a.ID)
			}
			flowIDs = append(flowIDs, a.FlowID)
		}

		payload := mustJSON(map[string]any{
			"share_id":       rev.ShareID,
			"publication_id": rev.PublicationID,
			"to_account_id":  rev.ToAccountID,
			"policy":         string(rev.Policy),
			"flow_ids":       flowIDs,
		})
		// "flow_share" and not "flow": what is being revoked is the SHARE, not
		// the flow itself — the flow (and its copies) are reached AS A
		// CONSEQUENCE, not as the aggregate this event is about.
		//
		// `dop.workflow.share.revoked` / `dop.workflow.grant.revoked`, not
		// `dop.flow.*`: every event in the tree follows
		// `dop.<domain>.<aggregate>.<verb>` — including
		// `dop.workflow.flow.created` a few lines away in workflow.go — and
		// Subject() prepends `dop.` verbatim, so a `flow.*` type here would
		// have put these two on `dop.flow.*` subjects, invisible to anything
		// subscribed to `dop.workflow.>`.
		if err := Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "flow_share", AggregateID: rev.ShareID,
			Type: "dop.workflow.share.revoked", Payload: payload,
		}); err != nil {
			return err
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: rev.ToAccountID, Aggregate: "flow_share", AggregateID: rev.ShareID,
			Type: "dop.workflow.grant.revoked", Payload: payload,
		})
	})
}

// ── derivation (R17) ─────────────────────────────────────────────────────────

// insertDerivedFlowRow is insertFlowRow's counterpart for a COPY: it also
// writes origin_ref/origin_version/origin_adopted_at, which a flow written
// directly in its own account never has (workflow.go's insertFlowRow leaves
// them NULL, which is exactly right there).
func insertDerivedFlowRow(ctx context.Context, tx pgx.Tx, f *workflow.Flow, key string) (string, bool, error) {
	var ownerID any
	if f.OwnerID != "" {
		ownerID = f.OwnerID
	}
	var createdBy any
	if f.CreatedBy != "" {
		createdBy = f.CreatedBy
	}
	var originRef, originVersion, originAdoptedAt any
	if f.Origin != nil {
		originRef = f.Origin.Ref
		originVersion = f.Origin.Version
		originAdoptedAt = f.Origin.AdoptedAt
	}

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO flows (account_id, owner_scope, owner_id, current_version,
		                   idempotency_key, created_by, created_at, updated_at,
		                   origin_ref, origin_version, origin_adopted_at)
		VALUES ($1, $2::flow_scope, $3, 1, $4, $5, $6, $6, $7, $8, $9)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		f.AccountID, string(f.OwnerScope), ownerID, key, createdBy, f.CreatedAt,
		originRef, originVersion, originAdoptedAt).Scan(&id)
	if NoRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, Translate(err, "the derived flow")
	}
	return id, true, nil
}

// derivedFlowByKey mirrors workflow.go's flowByKey, including its account
// filter — flows.idempotency_key is globally unique, so RecordDerivation's
// retry path needs exactly the same tenant check flowByKey needs: without it,
// a colliding key from another account's flow could be handed back here too.
func derivedFlowByKey(ctx context.Context, tx pgx.Tx, accountID, key string) (*workflow.Flow, error) {
	f, err := scanFlow(tx.QueryRow(ctx, `SELECT `+flowCols+currentJoin+
		` WHERE f.idempotency_key = $1 AND f.account_id = $2`, key, accountID))
	if err != nil {
		return nil, Translate(err, "the derived flow")
	}
	return f, nil
}

func insertAdoption(ctx context.Context, tx pgx.Tx, a *workflow.Adoption) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO flow_adoptions (publication_id, version, by_account_id, flow_id, derived_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		a.PublicationID, a.Version, a.ByAccountID, a.FlowID, a.DerivedAt).Scan(&a.ID)
	if err != nil {
		return Translate(err, "the adoption record")
	}
	return nil
}

func adoptionOfFlow(ctx context.Context, tx pgx.Tx, flowID string) (*workflow.Adoption, error) {
	var a workflow.Adoption
	var revokedAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT id, publication_id, version, by_account_id, flow_id, derived_at, revoked_at
		  FROM flow_adoptions WHERE flow_id = $1`, flowID).
		Scan(&a.ID, &a.PublicationID, &a.Version, &a.ByAccountID, &a.FlowID, &a.DerivedAt, &revokedAt)
	if err != nil {
		return nil, Translate(err, "the adoption record")
	}
	if revokedAt != nil {
		a.RevokedAt = *revokedAt
	}
	return &a, nil
}

// RecordDerivation writes the copy (flow + its first version) and the
// publisher's adoption record in ONE transaction (R17). Two calls would let
// the copy exist while the publisher never learns of it — and the adoption
// record is exactly what RevokeShare uses to reach the copy, so an orphaned
// copy is one nobody could ever revoke.
//
// The retry path (idempotency key already used) does not re-derive: it loads
// the flow already written and the adoption record already made for it, and
// hands both back unchanged — a resend must not multiply either write.
func (r *WorkflowSharing) RecordDerivation(ctx context.Context, accountID string, flow *workflow.Flow, adoption *workflow.Adoption, idempotencyKey string) (*workflow.Flow, error) {
	var saved *workflow.Flow
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		id, created, err := insertDerivedFlowRow(ctx, tx, flow, idempotencyKey)
		if err != nil {
			return err
		}
		if !created {
			saved, err = derivedFlowByKey(ctx, tx, accountID, idempotencyKey)
			if err != nil {
				return err
			}
			got, err := adoptionOfFlow(ctx, tx, saved.ID)
			if err != nil {
				return err
			}
			*adoption = *got
			return nil
		}

		if _, err := insertVersion(ctx, tx, id, 1, flow, idempotencyKey+":1"); err != nil {
			return err
		}
		adoption.FlowID = id
		if err := insertAdoption(ctx, tx, adoption); err != nil {
			return err
		}
		// loadFlow is workflow.go's own loader: now that flowCols carries the
		// provenance and revocation columns too (see the comment on flowCols),
		// a derived flow needs no reader of its own.
		saved, err = loadFlow(ctx, tx, accountID, id)
		return err
	})
	return saved, err
}

func (r *WorkflowSharing) AdoptionsOfPublication(ctx context.Context, accountID, publicationID string) ([]workflow.Adoption, error) {
	if _, err := r.PublicationByID(ctx, accountID, publicationID); err != nil {
		return nil, err
	}
	const q = `
SELECT id, publication_id, version, by_account_id, flow_id, derived_at, revoked_at
  FROM flow_adoptions
 WHERE publication_id = $1
 ORDER BY derived_at`
	rows, err := r.pool.Query(ctx, q, publicationID)
	if err != nil {
		return nil, Translate(err, "the publication's adoptions")
	}
	defer rows.Close()

	var out []workflow.Adoption
	for rows.Next() {
		var a workflow.Adoption
		var revokedAt *time.Time
		if err := rows.Scan(&a.ID, &a.PublicationID, &a.Version, &a.ByAccountID, &a.FlowID, &a.DerivedAt, &revokedAt); err != nil {
			return nil, Translate(err, "the publication's adoptions")
		}
		if revokedAt != nil {
			a.RevokedAt = *revokedAt
		}
		out = append(out, a)
	}
	return out, Translate(rows.Err(), "the publication's adoptions")
}

// ── the pin ──────────────────────────────────────────────────────────────────

// Pin upserts the account's pin on a flow it inherits across an ownership
// boundary. Unlike CreatePublication/CreateShare this is a genuine update —
// BumpPin exists precisely to MOVE the pin, not to retry a write — so the
// conflict clause here writes the new values instead of returning the old
// ones.
func (r *WorkflowSharing) Pin(ctx context.Context, accountID, flowID string, version int32, by string, at time.Time) error {
	var pinnedBy any
	if by != "" {
		pinnedBy = by
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO account_flow_pins (account_id, flow_id, version, pinned_by, pinned_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (account_id, flow_id) DO UPDATE
		   SET version = EXCLUDED.version, pinned_by = EXCLUDED.pinned_by, pinned_at = EXCLUDED.pinned_at`,
		accountID, flowID, version, pinnedBy, at)
	if err != nil {
		return Translate(err, "the pin")
	}
	return nil
}

func (r *WorkflowSharing) PinOf(ctx context.Context, accountID, flowID string) (int32, bool, error) {
	var version int32
	err := r.pool.QueryRow(ctx,
		`SELECT version FROM account_flow_pins WHERE account_id = $1 AND flow_id = $2`,
		accountID, flowID).Scan(&version)
	if NoRows(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, Translate(err, "the pin")
	}
	return version, true, nil
}

// ── AccountFacts ─────────────────────────────────────────────────────────────

// AccountFactsRepo is the narrow port into identity that workflow needs: two
// unrelated fields of an account, not the account. It reads `accounts`
// directly, the same table's row `ResolvePublication` already joins against —
// a second read here is one more query, not a second source of truth.
type AccountFactsRepo struct{ pool *pgxpool.Pool }

func NewAccountFactsRepo(pool *pgxpool.Pool) *AccountFactsRepo {
	return &AccountFactsRepo{pool: pool}
}

func (r *AccountFactsRepo) DefaultRevocationPolicy(ctx context.Context, accountID string) (string, error) {
	var policy string
	err := r.pool.QueryRow(ctx,
		`SELECT default_revocation_policy::text FROM accounts WHERE id = $1`, accountID).Scan(&policy)
	if err != nil {
		return "", Translate(err, "account")
	}
	return policy, nil
}

// HandleOf is the same lookup ResolvePublication already runs the other way
// (handle → account, in the JOIN above): here it is account → handle, which is
// what the edge needs to render a publication's own reference server-side.
func (r *AccountFactsRepo) HandleOf(ctx context.Context, accountID string) (string, error) {
	var handle string
	err := r.pool.QueryRow(ctx,
		`SELECT handle FROM accounts WHERE id = $1`, accountID).Scan(&handle)
	if err != nil {
		return "", Translate(err, "account")
	}
	return handle, nil
}

var _ workflow.AccountFacts = (*AccountFactsRepo)(nil)
