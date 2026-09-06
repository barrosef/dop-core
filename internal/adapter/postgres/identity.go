package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// IdentityRepo implements identity.Repository. It is the ONLY place with
// identity SQL — the domain never sees a query.
type IdentityRepo struct{ pool *pgxpool.Pool }

func NewIdentityRepo(pool *pgxpool.Pool) *IdentityRepo { return &IdentityRepo{pool: pool} }

const userCols = `id, subject, email, email_verified, name, avatar_url, providers, created_at, updated_at`

func scanUser(row pgx.Row) (*identity.User, error) {
	var u identity.User
	var email, name, avatar *string
	err := row.Scan(&u.ID, &u.Subject, &email, &u.EmailVerified, &name, &avatar,
		&u.Providers, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.Email, u.Name, u.AvatarURL = deref(email), deref(name), deref(avatar)
	return &u, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (r *IdentityRepo) UserBySubject(ctx context.Context, subject string) (*identity.User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE subject = $1`, subject))
	if err != nil {
		return nil, Translate(err, "user")
	}
	return u, nil
}

func (r *IdentityRepo) UserByID(ctx context.Context, id string) (*identity.User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	if err != nil {
		return nil, Translate(err, "user")
	}
	return u, nil
}

// UpsertUser is idempotent by the natural key (subject) — EnsureUser runs on
// every login and must not duplicate.
func (r *IdentityRepo) UpsertUser(ctx context.Context, u *identity.User) (*identity.User, error) {
	var saved *identity.User
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO users (subject, email, email_verified, name, avatar_url, providers)
			VALUES ($1, NULLIF($2,''), $3, NULLIF($4,''), NULLIF($5,''), $6)
			ON CONFLICT (subject) DO UPDATE SET
				email          = COALESCE(NULLIF(EXCLUDED.email,''), users.email),
				email_verified = EXCLUDED.email_verified OR users.email_verified,
				name           = COALESCE(NULLIF(EXCLUDED.name,''), users.name),
				avatar_url     = COALESCE(NULLIF(EXCLUDED.avatar_url,''), users.avatar_url),
				providers      = EXCLUDED.providers,
				updated_at     = now()
			RETURNING `+userCols,
			u.Subject, u.Email, u.EmailVerified, u.Name, u.AvatarURL, u.Providers)
		var err error
		saved, err = scanUser(row)
		if err != nil {
			return Translate(err, "user")
		}
		return Emit(ctx, tx, ports.Event{
			Aggregate: "user", AggregateID: saved.ID, Type: "dop.identity.user.ensured",
			Payload: mustJSON(map[string]any{"subject": saved.Subject, "email": saved.Email}),
		})
	})
	return saved, err
}

// default_revocation_policy is read here, not just written by
// SetDefaultRevocationPolicy: Task 5 stamps a grant's own policy from this
// value at share time, and a read path that kept returning "" after a
// successful write would make an account that chose `terminate` hand out
// `prospective` grants — wrong, and silent (ADR-0021: an emulator/read path
// that lies about a write is exactly the shape that has bitten this codebase
// before).
const accountCols = `id, kind, handle, display_name,
	COALESCE(legal_id,''), COALESCE(legal_name,''), COALESCE(verified_domain,''),
	default_revocation_policy::text,
	created_at, updated_at`

func scanAccount(row pgx.Row) (*identity.Account, error) {
	var a identity.Account
	var kind string
	if err := row.Scan(&a.ID, &kind, &a.Handle, &a.DisplayName,
		&a.LegalID, &a.LegalName, &a.VerifiedDomain, &a.DefaultRevocationPolicy,
		&a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Kind = identity.AccountKind(kind)
	return &a, nil
}

func (r *IdentityRepo) AccountByID(ctx context.Context, id string) (*identity.Account, error) {
	a, err := scanAccount(r.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = $1`, id))
	if err != nil {
		return nil, Translate(err, "account")
	}
	return a, nil
}

func (r *IdentityRepo) AccountByHandle(ctx context.Context, h string) (*identity.Account, error) {
	a, err := scanAccount(r.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE handle = $1`, h))
	if err != nil {
		return nil, Translate(err, "account")
	}
	return a, nil
}

// CreateAccountWithOwner creates the account and the owner membership in the
// SAME transaction. The account never exists without an owner — the invariant
// starts holding at birth.
func (r *IdentityRepo) CreateAccountWithOwner(ctx context.Context, a *identity.Account, ownerUserID string) (*identity.Account, error) {
	var saved *identity.Account
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO accounts (kind, handle, display_name, legal_id)
			VALUES ($1, $2, $3, NULLIF($4,''))
			RETURNING `+accountCols,
			string(a.Kind), a.Handle, a.DisplayName, a.LegalID)
		var err error
		saved, err = scanAccount(row)
		if err != nil {
			return Translate(err, "account")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memberships (user_id, account_id, role) VALUES ($1, $2, 'owner')`,
			ownerUserID, saved.ID); err != nil {
			return Translate(err, "membership")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.ID, Aggregate: "account", AggregateID: saved.ID,
			Type:    "dop.identity.account.created",
			Payload: mustJSON(map[string]any{"kind": saved.Kind, "handle": saved.Handle, "owner": ownerUserID}),
		})
	})
	return saved, err
}

func (r *IdentityRepo) AccountsOfUser(ctx context.Context, userID string) ([]identity.Account, []identity.Membership, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT a.id, a.kind, a.handle, a.display_name,
		       COALESCE(a.legal_id,''), COALESCE(a.legal_name,''), COALESCE(a.verified_domain,''),
		       a.created_at, a.updated_at,
		       m.id, m.role, m.created_at, m.updated_at
		  FROM memberships m
		  JOIN accounts a ON a.id = m.account_id
		 WHERE m.user_id = $1
		 ORDER BY a.kind, a.handle`, userID)
	if err != nil {
		return nil, nil, Translate(err, "the user's accounts")
	}
	defer rows.Close()

	var accounts []identity.Account
	var members []identity.Membership
	for rows.Next() {
		var a identity.Account
		var m identity.Membership
		var kind, role string
		if err := rows.Scan(&a.ID, &kind, &a.Handle, &a.DisplayName,
			&a.LegalID, &a.LegalName, &a.VerifiedDomain, &a.CreatedAt, &a.UpdatedAt,
			&m.ID, &role, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, nil, Translate(err, "the user's accounts")
		}
		a.Kind = identity.AccountKind(kind)
		m.Role, m.UserID, m.AccountID = identity.Role(role), userID, a.ID
		accounts = append(accounts, a)
		members = append(members, m)
	}
	return accounts, members, rows.Err()
}

// SetDefaultRevocationPolicy stores the account's default; a grant's own
// policy is stamped from it only at share time (flow sharing spec §3.2) — this
// write never touches a grant already made.
func (r *IdentityRepo) SetDefaultRevocationPolicy(ctx context.Context, accountID, policy string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE accounts SET default_revocation_policy = $2, updated_at = now() WHERE id = $1`,
		accountID, policy)
	if err != nil {
		return Translate(err, "account")
	}
	return nil
}

func (r *IdentityRepo) MembershipsOfAccount(ctx context.Context, accountID string) ([]identity.Membership, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, user_id, account_id, role, created_at, updated_at
		   FROM memberships WHERE account_id = $1 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, Translate(err, "memberships")
	}
	defer rows.Close()
	var out []identity.Membership
	for rows.Next() {
		var m identity.Membership
		var role string
		if err := rows.Scan(&m.ID, &m.UserID, &m.AccountID, &role, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, Translate(err, "memberships")
		}
		m.Role = identity.Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *IdentityRepo) MembershipOf(ctx context.Context, userID, accountID string) (*identity.Membership, error) {
	var m identity.Membership
	var role string
	err := r.pool.QueryRow(ctx,
		`SELECT id, user_id, account_id, role, created_at, updated_at
		   FROM memberships WHERE user_id = $1 AND account_id = $2`, userID, accountID).
		Scan(&m.ID, &m.UserID, &m.AccountID, &role, &m.CreatedAt, &m.UpdatedAt)
	if NoRows(err) {
		return nil, nil // no membership is not an error; the domain decides
	}
	if err != nil {
		return nil, Translate(err, "membership")
	}
	m.Role = identity.Role(role)
	return &m, nil
}

// UpdateMembershipRole may fire the owner invariant's trigger — Translate turns
// the trigger's exception into a precondition error, with the database's message
// reaching the user.
// MembershipByID reads the row without filtering by account: the use case needs
// to compare the account and answer "not found" when it differs — an id from
// another account must not be told apart from one that does not exist.
func (r *IdentityRepo) MembershipByID(ctx context.Context, membershipID string) (*identity.Membership, error) {
	var m identity.Membership
	var role string
	err := r.pool.QueryRow(ctx, `
		SELECT id, user_id, account_id, role, created_at, updated_at
		  FROM memberships WHERE id = $1`, membershipID).
		Scan(&m.ID, &m.UserID, &m.AccountID, &role, &m.CreatedAt, &m.UpdatedAt)
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "membership")
	}
	m.Role = identity.Role(role)
	return &m, nil
}

// RemoveMembership deletes the row and emits the event in the SAME transaction
// (ADR-0019). The event carries the user_id because the row will not be there
// to be consulted afterwards — a projection reading this later has no way back.
func (r *IdentityRepo) RemoveMembership(ctx context.Context, membershipID string) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var userID, accountID, role string
		err := tx.QueryRow(ctx, `
			DELETE FROM memberships WHERE id = $1
			 RETURNING user_id, account_id, role`, membershipID).
			Scan(&userID, &accountID, &role)
		if err != nil {
			return Translate(err, "membership")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "membership", AggregateID: membershipID,
			Type:    "dop.identity.membership.removed",
			Payload: mustJSON(map[string]any{"user_id": userID, "role": role}),
		})
	})
}

func (r *IdentityRepo) UpdateMembershipRole(ctx context.Context, membershipID string, role identity.Role) (*identity.Membership, error) {
	var m identity.Membership
	var got string
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE memberships SET role = $2, updated_at = now()
			 WHERE id = $1
			 RETURNING id, user_id, account_id, role, created_at, updated_at`,
			membershipID, string(role)).
			Scan(&m.ID, &m.UserID, &m.AccountID, &got, &m.CreatedAt, &m.UpdatedAt)
		if err != nil {
			return Translate(err, "membership")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: m.AccountID, Aggregate: "membership", AggregateID: m.ID,
			Type:    "dop.identity.membership.role_changed",
			Payload: mustJSON(map[string]any{"user_id": m.UserID, "role": got}),
		})
	})
	if err != nil {
		return nil, err
	}
	m.Role = identity.Role(got)
	return &m, nil
}

func (r *IdentityRepo) CreateInvite(ctx context.Context, inv *identity.Invite) (*identity.Invite, error) {
	grants, _ := json.Marshal(inv.Grants)
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO invites (account_id, email, role, grants, invited_by, expires_at)
			VALUES ($1,$2,$3,$4,NULLIF($5,'')::uuid,$6)
			RETURNING id, created_at`,
			inv.AccountID, inv.Email, string(inv.Role), grants, inv.InvitedBy, inv.ExpiresAt).
			Scan(&inv.ID, &inv.CreatedAt); err != nil {
			return Translate(err, "invite")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: inv.AccountID, Aggregate: "invite", AggregateID: inv.ID,
			Type: "dop.identity.invite.created",
			// invite_id in clear text is safe NOW: on its own it grants nothing
			// — acceptance still requires the invited person's session.
			Payload: mustJSON(map[string]any{
				"invite_id": inv.ID, "email": inv.Email, "role": inv.Role,
				"expires_at": inv.ExpiresAt,
			}),
		})
	})
	return inv, err
}

func (r *IdentityRepo) InviteByID(ctx context.Context, id string) (*identity.Invite, error) {
	var inv identity.Invite
	var role, status string
	var grants []byte
	var invitedBy *string
	err := r.pool.QueryRow(ctx, `
		SELECT id, account_id, email, role, grants, status, invited_by, expires_at, created_at
		  FROM invites WHERE id = $1`, id).
		Scan(&inv.ID, &inv.AccountID, &inv.Email, &role, &grants, &status,
			&invitedBy, &inv.ExpiresAt, &inv.CreatedAt)
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "invite")
	}
	inv.Role, inv.Status, inv.InvitedBy = identity.Role(role), identity.InviteStatus(status), deref(invitedBy)
	_ = json.Unmarshal(grants, &inv.Grants)
	return &inv, nil
}

// InvitesOfAccount lists the account's invites — the history, not only what is
// pending: "what happened to the invite I sent yesterday?" is answered by a
// revoked or an accepted row, and a list that hid them would send the person to
// support.
func (r *IdentityRepo) InvitesOfAccount(ctx context.Context, accountID string) ([]identity.Invite, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, account_id, email, role, grants, status, invited_by, expires_at, created_at
		  FROM invites WHERE account_id = $1
		 ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, Translate(err, "invites")
	}
	defer rows.Close()

	out := []identity.Invite{}
	for rows.Next() {
		var inv identity.Invite
		var role, status string
		var grants []byte
		var invitedBy *string
		if err := rows.Scan(&inv.ID, &inv.AccountID, &inv.Email, &role, &grants, &status,
			&invitedBy, &inv.ExpiresAt, &inv.CreatedAt); err != nil {
			return nil, Translate(err, "invites")
		}
		inv.Role, inv.Status, inv.InvitedBy = identity.Role(role), identity.InviteStatus(status), deref(invitedBy)
		_ = json.Unmarshal(grants, &inv.Grants)
		out = append(out, inv)
	}
	return out, Translate(rows.Err(), "invites")
}

// AcceptInvite creates the membership, applies the grants composed in the invite
// and marks the invite — all in one transaction.
func (r *IdentityRepo) AcceptInvite(ctx context.Context, inviteID, userID string) (*identity.Membership, error) {
	var m identity.Membership
	var role string
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var accountID string
		var grants []byte
		if err := tx.QueryRow(ctx, `
			UPDATE invites SET status = 'accepted'
			 WHERE id = $1 AND status = 'pending'
			 RETURNING account_id, role, grants`, inviteID).
			Scan(&accountID, &role, &grants); err != nil {
			return Translate(err, "invite")
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO memberships (user_id, account_id, role)
			VALUES ($1,$2,$3)
			ON CONFLICT (user_id, account_id) DO UPDATE SET role = EXCLUDED.role, updated_at = now()
			RETURNING id, user_id, account_id, role, created_at, updated_at`,
			userID, accountID, role).
			Scan(&m.ID, &m.UserID, &m.AccountID, &role, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return Translate(err, "membership")
		}
		var specs []identity.GrantSpec
		_ = json.Unmarshal(grants, &specs)
		for _, g := range specs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO resource_grants (resource_id, user_id, level)
				VALUES ($1,$2,$3) ON CONFLICT (resource_id, user_id) DO UPDATE SET level = EXCLUDED.level`,
				g.ResourceID, userID, g.Level); err != nil {
				return Translate(err, "grant")
			}
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "membership", AggregateID: m.ID,
			Type:    "dop.identity.invite.accepted",
			Payload: mustJSON(map[string]any{"user_id": userID, "role": role, "grants": len(specs)}),
		})
	})
	if err != nil {
		return nil, err
	}
	m.Role = identity.Role(role)
	return &m, nil
}

func (r *IdentityRepo) RevokeInvite(ctx context.Context, accountID, inviteID string) (*identity.Invite, error) {
	var inv identity.Invite
	var role, status string
	err := r.pool.QueryRow(ctx, `
		UPDATE invites SET status = 'revoked'
		 WHERE id = $1 AND account_id = $2 AND status = 'pending'
		 RETURNING id, account_id, email, role, status, expires_at, created_at`,
		inviteID, accountID).
		Scan(&inv.ID, &inv.AccountID, &inv.Email, &role, &status, &inv.ExpiresAt, &inv.CreatedAt)
	if err != nil {
		return nil, Translate(err, "invite")
	}
	inv.Role, inv.Status = identity.Role(role), identity.InviteStatus(status)
	return &inv, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// accountFromCtx is the shortcut the repositories use: every query filters by
// the active account — isolation is a constraint, not a convention.
func accountFromCtx(ctx context.Context) (string, error) {
	id, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return "", errs.Wrap(errs.KindInvalid, err, "no active account")
	}
	return id, nil
}

var _ identity.Repository = (*IdentityRepo)(nil)
