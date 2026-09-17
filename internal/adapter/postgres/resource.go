package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/domain/resource"
)

// ResourceRepo implements resource.Repository. It is the ONLY place with
// resource SQL — the domain never sees a query.
//
// Two things hold for this whole file:
//
//   - account_id goes into EVERY WHERE clause, including the grants' (through a
//     join with resources). Multi-tenant isolation is a constraint, not trust in
//     the caller;
//   - every state change writes the event in the SAME transaction, through InTx
//   - Emit. It is the transactional outbox: a commit ⇒ state and event, or
//     neither (ADR-0014).
type ResourceRepo struct{ pool *pgxpool.Pool }

func NewResourceRepo(pool *pgxpool.Pool) *ResourceRepo { return &ResourceRepo{pool: pool} }

const resourceCols = `id, account_id, kind, name, version, config,
	COALESCE(credential_ref,''), status, COALESCE(created_by::text,''),
	created_at, updated_at`

func scanResource(row pgx.Row) (*resource.Resource, error) {
	var r resource.Resource
	var kind string
	var config []byte
	if err := row.Scan(&r.ID, &r.AccountID, &kind, &r.Name, &r.Version, &config,
		&r.CredentialRef, &r.Status, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Kind = resource.Kind(kind)
	r.Config = map[string]any{}
	_ = json.Unmarshal(config, &r.Config)
	return &r, nil
}

// List filters by account and, optionally, by kind. An empty kind = all.
func (r *ResourceRepo) List(ctx context.Context, accountID string, kind resource.Kind) ([]resource.Resource, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+resourceCols+`
		  FROM resources
		 WHERE account_id = $1
		   AND ($2::text = '' OR kind::text = $2::text)
		 ORDER BY kind, name`, accountID, string(kind))
	if err != nil {
		return nil, Translate(err, "resources")
	}
	defer rows.Close()

	var out []resource.Resource
	for rows.Next() {
		res, err := scanResource(rows)
		if err != nil {
			return nil, Translate(err, "resources")
		}
		out = append(out, *res)
	}
	return out, rows.Err()
}

// ByID returns (nil, nil) when there is no row: "not found" is the domain's
// decision, not the adapter's — it is there that one knows whether the absence
// is an error.
func (r *ResourceRepo) ByID(ctx context.Context, accountID, id string) (*resource.Resource, error) {
	res, err := scanResource(r.pool.QueryRow(ctx,
		`SELECT `+resourceCols+` FROM resources WHERE id = $1 AND account_id = $2`, id, accountID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "resource")
	}
	return res, nil
}

func (r *ResourceRepo) Create(ctx context.Context, res *resource.Resource) (*resource.Resource, error) {
	var saved *resource.Resource
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO resources (account_id, kind, name, version, config, status, created_by)
			VALUES ($1, $2::resource_kind, $3, $4, $5, $6, NULLIF($7,'')::uuid)
			RETURNING `+resourceCols,
			res.AccountID, string(res.Kind), res.Name, res.Version,
			mustJSON(res.Config), res.Status, res.CreatedBy)
		var err error
		saved, err = scanResource(row)
		if err != nil {
			// UNIQUE (account_id, kind, name) becomes a 409, not a 500 —
			// repeating the same name is the client's error, with a useful
			// answer.
			return Translate(err, "resource")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "resource", AggregateID: saved.ID,
			Type: "dop.resource.created",
			Payload: mustJSON(map[string]any{
				"kind": saved.Kind, "name": saved.Name, "version": saved.Version,
			}),
		})
	})
	return saved, err
}

// Update writes the new configuration. bumpVersion increments the version in
// the SAME UPDATE — two commands would open a window for a read to see a version
// and a config that do not match.
func (r *ResourceRepo) Update(ctx context.Context, accountID, id string, config map[string]any, bumpVersion bool) (*resource.Resource, error) {
	var saved *resource.Resource
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE resources
			   SET config     = $3,
			       version    = CASE WHEN $4::bool THEN version + 1 ELSE version END,
			       updated_at = now()
			 WHERE id = $1 AND account_id = $2
			 RETURNING `+resourceCols,
			id, accountID, mustJSON(config), bumpVersion)
		var err error
		saved, err = scanResource(row)
		if err != nil {
			return Translate(err, "resource")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "resource", AggregateID: saved.ID,
			Type: "dop.resource.updated",
			Payload: mustJSON(map[string]any{
				"kind": saved.Kind, "name": saved.Name, "version": saved.Version,
				"versioned": bumpVersion,
			}),
		})
	})
	return saved, err
}

func (r *ResourceRepo) Delete(ctx context.Context, accountID, id string) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var kind, name string
		if err := tx.QueryRow(ctx, `
			DELETE FROM resources WHERE id = $1 AND account_id = $2
			RETURNING kind::text, name`, id, accountID).Scan(&kind, &name); err != nil {
			return Translate(err, "resource")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "resource", AggregateID: id,
			Type:    "dop.resource.deleted",
			Payload: mustJSON(map[string]any{"kind": kind, "name": name}),
		})
	})
}

// SetCredentialRef writes the POINTER, never the secret. The event too carries
// only the opaque reference: the event log is read by a lot of people.
func (r *ResourceRepo) SetCredentialRef(ctx context.Context, accountID, id, ref string) (*resource.Resource, error) {
	var saved *resource.Resource
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE resources SET credential_ref = $3, updated_at = now()
			 WHERE id = $1 AND account_id = $2
			 RETURNING `+resourceCols, id, accountID, ref)
		var err error
		saved, err = scanResource(row)
		if err != nil {
			return Translate(err, "resource")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "resource", AggregateID: saved.ID,
			Type: "dop.resource.credential_set",
			Payload: mustJSON(map[string]any{
				"kind": saved.Kind, "credential_ref": saved.CredentialRef,
			}),
		})
	})
	return saved, err
}

// ── grants ───────────────────────────────────────────────────────────────────

const grantCols = `g.id, g.resource_id, g.user_id, g.level,
	COALESCE(g.granted_by::text,''), g.created_at`

func scanGrant(row pgx.Row) (*resource.Grant, error) {
	var g resource.Grant
	var level string
	if err := row.Scan(&g.ID, &g.ResourceID, &g.UserID, &level, &g.GrantedBy, &g.CreatedAt); err != nil {
		return nil, err
	}
	g.Level = resource.Level(level)
	return &g, nil
}

// GrantsOfUser brings all the actor's grants in the account at once — it is what
// allows the domain to filter a whole list without one query per row.
func (r *ResourceRepo) GrantsOfUser(ctx context.Context, accountID, userID string) ([]resource.Grant, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+grantCols+`
		  FROM resource_grants g
		  JOIN resources r ON r.id = g.resource_id
		 WHERE r.account_id = $1 AND g.user_id = $2`, accountID, userID)
	if err != nil {
		return nil, Translate(err, "grants")
	}
	defer rows.Close()

	var out []resource.Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, Translate(err, "grants")
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

func (r *ResourceRepo) GrantOf(ctx context.Context, accountID, resourceID, userID string) (*resource.Grant, error) {
	g, err := scanGrant(r.pool.QueryRow(ctx, `
		SELECT `+grantCols+`
		  FROM resource_grants g
		  JOIN resources r ON r.id = g.resource_id
		 WHERE r.account_id = $1 AND g.resource_id = $2 AND g.user_id = $3`,
		accountID, resourceID, userID))
	if NoRows(err) {
		return nil, nil // no grant is not an error; the domain decides
	}
	if err != nil {
		return nil, Translate(err, "grant")
	}
	return g, nil
}

func (r *ResourceRepo) GrantByID(ctx context.Context, accountID, grantID string) (*resource.Grant, error) {
	g, err := scanGrant(r.pool.QueryRow(ctx, `
		SELECT `+grantCols+`
		  FROM resource_grants g
		  JOIN resources r ON r.id = g.resource_id
		 WHERE r.account_id = $1 AND g.id = $2`, accountID, grantID))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "grant")
	}
	return g, nil
}

// Grant is an upsert over UNIQUE (resource_id, user_id): granting again with
// another level ADJUSTS the grant. The INSERT ... SELECT ties the write to the
// active account — another account's resource simply produces no row, and the
// zero-rows becomes "not found".
func (r *ResourceRepo) Grant(ctx context.Context, accountID string, g *resource.Grant) (*resource.Grant, error) {
	var saved *resource.Grant
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO resource_grants (resource_id, user_id, level, granted_by)
			SELECT r.id, $2::uuid, $3::text, NULLIF($4,'')::uuid
			  FROM resources r
			 WHERE r.id = $1 AND r.account_id = $5
			ON CONFLICT (resource_id, user_id)
			DO UPDATE SET level = EXCLUDED.level, granted_by = EXCLUDED.granted_by
			RETURNING id, resource_id, user_id, level, COALESCE(granted_by::text,''), created_at`,
			g.ResourceID, g.UserID, string(g.Level), g.GrantedBy, accountID)
		var err error
		saved, err = scanGrant(row)
		if err != nil {
			return Translate(err, "grant")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "resource", AggregateID: saved.ResourceID,
			Type: "dop.resource.granted",
			Payload: mustJSON(map[string]any{
				"grant_id": saved.ID, "user_id": saved.UserID, "level": saved.Level,
			}),
		})
	})
	return saved, err
}

// RevokeGrantsOfUser removes every grant of one person in the account, and
// emits one event per grant — all inside a SINGLE transaction.
//
// The atomicity is the whole point: identity calls this when somebody leaves,
// having promised that a failed sweep removes nothing. The loop it replaced
// broke that promise halfway, and the test that covered it proved a fake.
func (r *ResourceRepo) RevokeGrantsOfUser(ctx context.Context, accountID, userID string) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			DELETE FROM resource_grants g
			 USING resources r
			 WHERE g.user_id = $2 AND r.id = g.resource_id AND r.account_id = $1
			 RETURNING g.resource_id, g.level`, accountID, userID)
		if err != nil {
			return Translate(err, "grants")
		}
		type gone struct{ resourceID, level string }
		var removed []gone
		for rows.Next() {
			var g gone
			if err := rows.Scan(&g.resourceID, &g.level); err != nil {
				rows.Close()
				return Translate(err, "grants")
			}
			removed = append(removed, g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Translate(err, "grants")
		}
		// The events come AFTER the rows are drained: emitting inside the
		// iteration would run a second query on a connection already streaming
		// one, which pgx refuses.
		for _, g := range removed {
			if err := Emit(ctx, tx, ports.Event{
				AccountID: accountID, Aggregate: "resource", AggregateID: g.resourceID,
				Type:    "dop.resource.grant_revoked",
				Payload: mustJSON(map[string]any{"user_id": userID, "level": g.level}),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *ResourceRepo) RevokeGrant(ctx context.Context, accountID, grantID string) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var resourceID, userID, level string
		if err := tx.QueryRow(ctx, `
			DELETE FROM resource_grants g
			 USING resources r
			 WHERE g.id = $2 AND r.id = g.resource_id AND r.account_id = $1
			 RETURNING g.resource_id, g.user_id, g.level`, accountID, grantID).
			Scan(&resourceID, &userID, &level); err != nil {
			return Translate(err, "grant")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "resource", AggregateID: resourceID,
			Type:    "dop.resource.grant_revoked",
			Payload: mustJSON(map[string]any{"grant_id": grantID, "user_id": userID, "level": level}),
		})
	})
}

var _ resource.Repository = (*ResourceRepo)(nil)
