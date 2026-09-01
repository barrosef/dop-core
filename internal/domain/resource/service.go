package resource

import (
	"context"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service concentrates the resource rules. It takes only PORTS.
//
// Note what does NOT exist here: no method returns a credential's value. The
// secret goes in through SetCredential and vanishes — whoever needs it is the
// executor, which resolves the SecretRef through the SecretStore itself. There
// is no read path by which a secret comes back out of an RPC (ADR-0001).
type Service struct {
	repo    Repository
	access  Access
	secrets ports.SecretStore
}

func NewService(repo Repository, access Access, secrets ports.SecretStore) *Service {
	return &Service{repo: repo, access: access, secrets: secrets}
}

// actor is the caller resolved in the active account: role and account nature,
// which is exactly what EffectiveLevel consumes.
type actor struct {
	accountID   string
	userID      string
	role        identity.Role
	accountKind identity.AccountKind
}

// who resolves the caller, in the active account.
//
// A request with no active account is invalid by definition (SP-0) — and the
// role is resolved ONCE per operation, not once per resource.
func (s *Service) who(ctx context.Context) (actor, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return actor{}, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return actor{}, errs.New(errs.KindUnauthorized, "actor not identified")
	}
	m, err := s.access.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return actor{}, err
	}
	acct, err := s.access.GetAccount(ctx, accountID)
	if err != nil {
		return actor{}, err
	}
	return actor{
		accountID:   accountID,
		userID:      call.ActorID,
		role:        m.Role,
		accountKind: acct.Kind,
	}, nil
}

// authorize loads the resource and checks the REQUIRED level, in one operation.
// Separating "load" from "authorize" would invite forgetting the second part.
func (s *Service) authorize(ctx context.Context, a actor, resourceID string, need Level) (*Resource, error) {
	if strings.TrimSpace(resourceID) == "" {
		return nil, errs.Invalid("resource not provided").WithCode(KeyResourceMissing, nil)
	}
	r, err := s.repo.ByID(ctx, a.accountID, resourceID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errs.NotFound("resource")
	}
	g, err := s.repo.GrantOf(ctx, a.accountID, r.ID, a.userID)
	if err != nil {
		return nil, err
	}
	if lvl := EffectiveLevel(*r, a.role, a.accountKind, g); !lvl.AtLeast(need) {
		return nil, errs.Permission("no %s grant over resource %q", need, r.Name).
			WithCode(KeyNoGrant, map[string]any{"level": string(need), "resource": r.Name})
	}
	return r, nil
}

// List returns what the actor MAY see, not everything the account holds.
//
// Filtering on read is what makes the by-nature default real: a closed
// integration does not appear in the list of somebody with no grant. The actor's
// grants are fetched in one go — there is no query per row.
func (s *Service) List(ctx context.Context, kind Kind) ([]Resource, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	if kind != "" && !ValidKind(kind) {
		return nil, errs.Invalid("unknown resource kind: %q", kind).
			WithCode(KeyKindUnknown, map[string]any{"kind": string(kind)})
	}

	all, err := s.repo.List(ctx, a.accountID, kind)
	if err != nil {
		return nil, err
	}
	grants, err := s.repo.GrantsOfUser(ctx, a.accountID, a.userID)
	if err != nil {
		return nil, err
	}
	byResource := make(map[string]*Grant, len(grants))
	for i := range grants {
		byResource[grants[i].ResourceID] = &grants[i]
	}

	out := make([]Resource, 0, len(all))
	for i := range all {
		if EffectiveLevel(all[i], a.role, a.accountKind, byResource[all[i].ID]).AtLeast(LevelUse) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Resource, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	return s.authorize(ctx, a, id, LevelUse)
}

// Create registers a resource in the active account.
//
// Two decisions live here:
//
//   - an integration has its config VALIDATED on write (category and provider):
//     finding out at deploy time that the integration does not know who it is
//     comes too late;
//   - an integration is born CLOSED, so whoever created it receives explicit
//     manage. Without that, a developer connects GitHub and loses access to
//     their own integration the instant after — the by-nature default would
//     become a trap instead of a protection.
func (s *Service) Create(ctx context.Context, kind Kind, name string, config map[string]any) (*Resource, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidKind(kind) {
		return nil, errs.Invalid("unknown resource kind: %q", kind).
			WithCode(KeyKindUnknown, map[string]any{"kind": string(kind)})
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	// Viewer is a read-only role: it does not create resources in the account.
	if a.role == identity.RoleViewer {
		return nil, errs.Permission("a viewer does not create resources").WithCode(KeyViewerCannotCreate, nil)
	}
	if config == nil {
		config = map[string]any{}
	}
	if kind.HasCredential() {
		if _, err := ParseIntegration(config); err != nil {
			return nil, err
		}
	}

	saved, err := s.repo.Create(ctx, &Resource{
		AccountID: a.accountID,
		Kind:      kind,
		Name:      strings.TrimSpace(name),
		Version:   1,
		Config:    config,
		Status:    "active",
		CreatedBy: a.userID,
	})
	if err != nil {
		return nil, err
	}

	if saved.HasCredential() && !a.role.HasImplicitManage() {
		if _, err := s.repo.Grant(ctx, a.accountID, &Grant{
			ResourceID: saved.ID,
			UserID:     a.userID,
			Level:      LevelManage,
			GrantedBy:  a.userID,
		}); err != nil {
			return nil, err
		}
	}
	return saved, nil
}

// Update changes the configuration. Content is VERSIONED — the new version does
// not erase the previous one, because somebody will need to know which skill
// that execution ran with. An integration is overwritten: yesterday's base_url
// is not history, it is litter.
func (s *Service) Update(ctx context.Context, id string, config map[string]any) (*Resource, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	r, err := s.authorize(ctx, a, id, LevelManage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		config = map[string]any{}
	}
	if r.HasCredential() {
		if _, err := ParseIntegration(config); err != nil {
			return nil, err
		}
	}
	return s.repo.Update(ctx, a.accountID, r.ID, config, r.IsVersioned())
}

// Delete removes the resource and, before it, the credential.
//
// The order is deliberate: the vault first, the row second. Delete on the
// SecretStore is idempotent by contract, so a failure removing the row leaves a
// clean retry. The reverse order would leave an orphan secret in the vault —
// with no row pointing at it, nobody would ever find it to delete.
func (s *Service) Delete(ctx context.Context, id string) error {
	a, err := s.who(ctx)
	if err != nil {
		return err
	}
	r, err := s.authorize(ctx, a, id, LevelManage)
	if err != nil {
		return err
	}
	if r.CredentialRef != "" {
		if err := s.secrets.Delete(ctx, SecretRefFor(a.accountID, r.ID)); err != nil {
			return errs.Wrap(errs.KindUnavailable, err,
				"failed to remove the credential of resource %s", r.ID)
		}
	}
	return s.repo.Delete(ctx, a.accountID, r.ID)
}

// Grant gives access to a member of the account.
//
// The granter has to hold manage over the RESOURCE — being a member is not
// enough, and holding use is not enough. And a grant only goes to someone who is
// already a member of the account: resource access is not a way into the
// account, it is composition over a membership that already exists (ADR-0013).
//
// A consequence worth recording: a personal account has no second member, so no
// grant is possible — a personal account's resource is never shareable, and that
// falls out of the cardinality, with no special rule.
func (s *Service) Grant(ctx context.Context, resourceID, userID string, level Level) (*Grant, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidLevel(level) {
		return nil, errs.Invalid("invalid grant level: %q (use use or manage)", level).
			WithCode(KeyLevelInvalid, map[string]any{"level": string(level)})
	}
	if strings.TrimSpace(userID) == "" {
		return nil, errs.Invalid("grant user not provided").WithCode(KeyGrantUserMissing, nil)
	}
	r, err := s.authorize(ctx, a, resourceID, LevelManage)
	if err != nil {
		return nil, err
	}
	if _, err := s.access.Authorize(ctx, userID, a.accountID); err != nil {
		switch errs.KindOf(err) {
		case errs.KindPermission, errs.KindNotFound:
			return nil, errs.Invalid("cannot grant access to someone who is not a member of this account").
				WithCode(KeyGranteeNotMember, nil)
		}
		return nil, err
	}
	return s.repo.Grant(ctx, a.accountID, &Grant{
		ResourceID: r.ID,
		UserID:     userID,
		Level:      level,
		GrantedBy:  a.userID,
	})
}

// RevokeGrant removes a grant. Authorization is over the grant's RESOURCE —
// whoever manages the resource decides who reaches it.
//
// Revoking does not close the door on owner and admin: their manage is implicit
// and does not go through this table. That is deliberate.
func (s *Service) RevokeGrant(ctx context.Context, grantID string) error {
	a, err := s.who(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(grantID) == "" {
		return errs.Invalid("grant not provided").WithCode(KeyGrantMissing, nil)
	}
	g, err := s.repo.GrantByID(ctx, a.accountID, grantID)
	if err != nil {
		return err
	}
	if g == nil {
		return errs.NotFound("grant")
	}
	if _, err := s.authorize(ctx, a, g.ResourceID, LevelManage); err != nil {
		return err
	}
	return s.repo.RevokeGrant(ctx, a.accountID, grantID)
}

// SetCredential stores the secret in the SecretStore and persists ONLY the
// opaque reference on the resource's row.
//
// The value is not written to the database, does not come back from any read,
// does not enter an event and does not enter an error message — not even as a
// "cause". The order is vault first, row second: if the database fails, an
// orphan secret with no pointer is left behind (inert, and overwritten on the
// next attempt); the reverse order would leave the row claiming a credential
// that does not exist, and execution would fail far from here, with no
// explanation.
func (s *Service) SetCredential(ctx context.Context, resourceID string, secret []byte) (string, error) {
	a, err := s.who(ctx)
	if err != nil {
		return "", err
	}
	if len(secret) == 0 {
		return "", errs.Invalid("empty credential").WithCode(KeyCredentialEmpty, nil)
	}
	r, err := s.authorize(ctx, a, resourceID, LevelManage)
	if err != nil {
		return "", err
	}
	if !r.HasCredential() {
		return "", errs.Precondition("a resource of kind %q has no credential", r.Kind).
			WithCode(KeyKindHasNoCredential, map[string]any{"kind": string(r.Kind)})
	}

	if err := s.secrets.Put(ctx, SecretRefFor(a.accountID, r.ID), ports.SecretValue(secret)); err != nil {
		return "", errs.Wrap(errs.KindUnavailable, err,
			"failed to store the credential of resource %s", r.ID)
	}
	saved, err := s.repo.SetCredentialRef(ctx, a.accountID, r.ID, CredentialRef(a.accountID, r.ID))
	if err != nil {
		return "", err
	}
	return saved.CredentialRef, nil
}
