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

// IdentityRepo implementa identity.Repository. É o ÚNICO lugar com SQL de
// identidade — o domínio nunca vê uma query.
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
		return nil, Translate(err, "usuário")
	}
	return u, nil
}

func (r *IdentityRepo) UserByID(ctx context.Context, id string) (*identity.User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	if err != nil {
		return nil, Translate(err, "usuário")
	}
	return u, nil
}

// UpsertUser é idempotente pela chave natural (subject) — EnsureUser roda em
// todo login e não pode duplicar.
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
			return Translate(err, "usuário")
		}
		return Emit(ctx, tx, ports.Event{
			Aggregate: "user", AggregateID: saved.ID, Type: "dop.identity.user.ensured",
			Payload: mustJSON(map[string]any{"subject": saved.Subject, "email": saved.Email}),
		})
	})
	return saved, err
}

const accountCols = `id, kind, handle, display_name,
	COALESCE(legal_id,''), COALESCE(legal_name,''), COALESCE(verified_domain,''),
	created_at, updated_at`

func scanAccount(row pgx.Row) (*identity.Account, error) {
	var a identity.Account
	var kind string
	if err := row.Scan(&a.ID, &kind, &a.Handle, &a.DisplayName,
		&a.LegalID, &a.LegalName, &a.VerifiedDomain, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Kind = identity.AccountKind(kind)
	return &a, nil
}

func (r *IdentityRepo) AccountByID(ctx context.Context, id string) (*identity.Account, error) {
	a, err := scanAccount(r.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = $1`, id))
	if err != nil {
		return nil, Translate(err, "conta")
	}
	return a, nil
}

func (r *IdentityRepo) AccountByHandle(ctx context.Context, h string) (*identity.Account, error) {
	a, err := scanAccount(r.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE handle = $1`, h))
	if err != nil {
		return nil, Translate(err, "conta")
	}
	return a, nil
}

// CreateAccountWithOwner cria conta e vínculo de owner na MESMA transação.
// A conta nunca existe sem dono — a invariante começa a valer no nascimento.
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
			return Translate(err, "conta")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memberships (user_id, account_id, role) VALUES ($1, $2, 'owner')`,
			ownerUserID, saved.ID); err != nil {
			return Translate(err, "vínculo")
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
		return nil, nil, Translate(err, "contas do usuário")
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
			return nil, nil, Translate(err, "contas do usuário")
		}
		a.Kind = identity.AccountKind(kind)
		m.Role, m.UserID, m.AccountID = identity.Role(role), userID, a.ID
		accounts = append(accounts, a)
		members = append(members, m)
	}
	return accounts, members, rows.Err()
}

func (r *IdentityRepo) MembershipsOfAccount(ctx context.Context, accountID string) ([]identity.Membership, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, user_id, account_id, role, created_at, updated_at
		   FROM memberships WHERE account_id = $1 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, Translate(err, "vínculos")
	}
	defer rows.Close()
	var out []identity.Membership
	for rows.Next() {
		var m identity.Membership
		var role string
		if err := rows.Scan(&m.ID, &m.UserID, &m.AccountID, &role, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, Translate(err, "vínculos")
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
		return nil, nil // sem vínculo não é erro; quem decide é o domínio
	}
	if err != nil {
		return nil, Translate(err, "vínculo")
	}
	m.Role = identity.Role(role)
	return &m, nil
}

// UpdateMembershipRole pode disparar a trigger de invariante do owner —
// Translate converte a exceção da trigger em erro de precondição, com a
// mensagem do banco chegando ao usuário.
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
			return Translate(err, "vínculo")
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

func (r *IdentityRepo) CreateInvite(ctx context.Context, inv *identity.Invite, tokenHash string) (*identity.Invite, error) {
	grants, _ := json.Marshal(inv.Grants)
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO invites (account_id, email, role, grants, token_hash, invited_by, expires_at)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,$7)
			RETURNING id, created_at`,
			inv.AccountID, inv.Email, string(inv.Role), grants, tokenHash, inv.InvitedBy, inv.ExpiresAt).
			Scan(&inv.ID, &inv.CreatedAt); err != nil {
			return Translate(err, "convite")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: inv.AccountID, Aggregate: "invite", AggregateID: inv.ID,
			Type:    "dop.identity.invite.created",
			Payload: mustJSON(map[string]any{"email": inv.Email, "role": inv.Role}),
		})
	})
	return inv, err
}

func (r *IdentityRepo) InviteByTokenHash(ctx context.Context, tokenHash string) (*identity.Invite, error) {
	var inv identity.Invite
	var role, status string
	var grants []byte
	var invitedBy *string
	err := r.pool.QueryRow(ctx, `
		SELECT id, account_id, email, role, grants, status, invited_by, expires_at, created_at
		  FROM invites WHERE token_hash = $1`, tokenHash).
		Scan(&inv.ID, &inv.AccountID, &inv.Email, &role, &grants, &status,
			&invitedBy, &inv.ExpiresAt, &inv.CreatedAt)
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "convite")
	}
	inv.Role, inv.Status, inv.InvitedBy = identity.Role(role), identity.InviteStatus(status), deref(invitedBy)
	_ = json.Unmarshal(grants, &inv.Grants)
	return &inv, nil
}

// AcceptInvite cria o vínculo, aplica as concessões compostas no convite e
// marca o convite — tudo numa transação.
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
			return Translate(err, "convite")
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO memberships (user_id, account_id, role)
			VALUES ($1,$2,$3)
			ON CONFLICT (user_id, account_id) DO UPDATE SET role = EXCLUDED.role, updated_at = now()
			RETURNING id, user_id, account_id, role, created_at, updated_at`,
			userID, accountID, role).
			Scan(&m.ID, &m.UserID, &m.AccountID, &role, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return Translate(err, "vínculo")
		}
		var specs []identity.GrantSpec
		_ = json.Unmarshal(grants, &specs)
		for _, g := range specs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO resource_grants (resource_id, user_id, level)
				VALUES ($1,$2,$3) ON CONFLICT (resource_id, user_id) DO UPDATE SET level = EXCLUDED.level`,
				g.ResourceID, userID, g.Level); err != nil {
				return Translate(err, "concessão")
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
		return nil, Translate(err, "convite")
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

// accountFromCtx é atalho usado pelos repositórios: toda consulta filtra por
// conta ativa — isolamento é constraint, não convenção.
func accountFromCtx(ctx context.Context) (string, error) {
	id, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return "", errs.Wrap(errs.KindInvalid, err, "conta ativa ausente")
	}
	return id, nil
}

var _ identity.Repository = (*IdentityRepo)(nil)
