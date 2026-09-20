package resource

import (
	"context"
	"strings"

	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Service concentrates the resource rules. It takes only PORTS.
//
// Note what does NOT exist here: no method returns a credential's value. The
// secret goes in through SetCredential and vanishes — whoever needs it is the
// executor, which resolves the SecretRef through the SecretStore itself. There
// is no read path by which a secret comes back out of an RPC (ADR-0001).
type Service struct {
	checker CredentialChecker
	repo    Repository
	access  Access
	secrets ports.SecretStore
	stepUp  StepUpGate
}

// StepUpGate is the second factor's gate, in the narrowest possible shape: one
// question, one answer (ADR-0020 §5).
//
// It is declared here, in the language of what is being asked, instead of this
// domain importing `secondfactor` — the same rule as the rest of the glue.
type StepUpGate interface {
	RequireStepUp(ctx context.Context) error
}

// WithStepUp wires the gate. A service assembled WITHOUT it has no second
// factor, and that is explicit: the domain tests build it without a gate on
// purpose, and the composition root always wires one (internal/app/register.go).
func (s *Service) WithStepUp(g StepUpGate) *Service {
	s.stepUp = g
	return s
}

// requireStepUp is the single call site of the gate in this domain. With no gate
// wired it lets through — the alternative would be a domain that cannot be
// tested without the second factor's whole machinery.
func (s *Service) requireStepUp(ctx context.Context) error {
	if s.stepUp == nil {
		return nil
	}
	return s.stepUp.RequireStepUp(ctx)
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

// CountIntegrations answers identity.Connections for the onboarding journey:
// how many git and how many task-manager integrations the account holds. It
// reads the account's rows with no visibility filter on purpose — the journey
// asks about the PERSONAL account, whose only member is its owner, and a count
// is not a listing.
func (s *Service) CountIntegrations(ctx context.Context, accountID string) (git, tasks int, err error) {
	all, err := s.repo.List(ctx, accountID, KindIntegration)
	if err != nil {
		return 0, 0, err
	}
	for i := range all {
		spec, err := ParseIntegration(all[i].Config)
		if err != nil {
			continue // a row that fails to parse was refused at write time; it is not a connection
		}
		switch spec.Category {
		case CategoryGit:
			git++
		case CategoryTaskManager:
			tasks++
		}
	}
	return git, tasks, nil
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
// account, it is composition over a membership that already exists (ADR-0009).
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

// GrantsOfMember lists the grants somebody holds in the active account.
//
// It is what the members screen needs in order to SHOW state before editing it:
// without a read, the screen can only write blind.
//
// Whoever asks about themselves always may. Asking about somebody else requires
// managing members — the same role that hands out and takes away access. It is
// not the resource's `manage`: this question is about a PERSON, and answering it
// resource by resource would leak which resources exist to whoever holds a grant
// over one of them.
func (s *Service) GrantsOfMember(ctx context.Context, userID string) ([]Grant, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, errs.Invalid("grant user not provided").WithCode(KeyGrantUserMissing, nil)
	}
	if userID != a.userID && !a.role.CanManageMembers() {
		return nil, errs.Permission("only an owner or admin sees another member's grants").
			WithCode(KeyOnlyManagersSeeGrants, nil)
	}
	return s.repo.GrantsOfUser(ctx, a.accountID, userID)
}

// RevokeAllOfMember removes every grant the person holds in the account.
//
// It DOES NOT AUTHORIZE, and that is deliberate: it is not reached from the
// edge — there is no RPC for it. Identity calls it when somebody leaves the
// account, and identity has already checked the role, the last-owner invariant
// and the second factor. Repeating the check here would mean checking it
// against the WRONG actor — the one doing the removing, not the one leaving.
//
// It takes accountID explicitly instead of reading the context so it cannot,
// by accident, sweep another account's grants.
//
// ONE statement, not a loop over RevokeGrant: the caller's guarantee is that a
// failed sweep removes nothing, and a loop makes that false halfway through.
func (s *Service) RevokeAllOfMember(ctx context.Context, accountID, userID string) error {
	return s.repo.RevokeGrantsOfUser(ctx, accountID, userID)
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
	// The second factor's gate: this is the operation that puts a third party's
	// key into the vault (ADR-0020 §5). It comes AFTER the cheap validation and
	// BEFORE the authorization, so that a caller with no session does not learn
	// which resources exist.
	if err := s.requireStepUp(ctx); err != nil {
		return "", err
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

// ── checking a credential (onboarding spec 2026-09-20 §5) ───────────────────

// CheckResult is what a credential check answers. Operated=false is the
// honest case: the platform has no adapter for this provider, the credential
// is stored, nothing was tried. OK is meaningful only when Operated is true.
type CheckResult struct {
	Operated bool
	OK       bool
	Identity string // who the provider says the token belongs to
	Message  string // a sentence for the person, in English; the edge translates the Kind
}

// CredentialChecker probes a provider with the RESOLVED secret. Declared here
// and satisfied in the composition root, which is the only place that knows
// GitHub, GitLab and the vault at once (the same reasoning as gitProviders).
type CredentialChecker interface {
	Check(ctx context.Context, spec IntegrationSpec, secret []byte) (CheckResult, error)
}

// WithChecker wires the prober. Without it Check answers "not operated" for
// everything — explicit, so a domain test needs no HTTP.
func (s *Service) WithChecker(c CredentialChecker) *Service {
	s.checker = c
	return s
}

// Check resolves the integration's credential from the vault and asks the
// checker whether it works. The secret is read HERE, in the core, and handed
// to the checker ready-made; it enters no result, no event and no error.
func (s *Service) Check(ctx context.Context, resourceID string) (CheckResult, error) {
	a, err := s.who(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	r, err := s.authorize(ctx, a, resourceID, LevelUse)
	if err != nil {
		return CheckResult{}, err
	}
	if !r.HasCredential() {
		return CheckResult{}, errs.Precondition("a resource of kind %q has no credential", r.Kind).
			WithCode(KeyKindHasNoCredential, map[string]any{"kind": string(r.Kind)})
	}
	spec, err := ParseIntegration(r.Config)
	if err != nil {
		return CheckResult{}, err
	}
	if r.CredentialRef == "" {
		return CheckResult{}, errs.Precondition("integration %q has no credential to check", r.Name).
			WithCode(KeyCredentialMissing, nil)
	}
	value, err := s.secrets.Get(ctx, SecretRefFor(a.accountID, r.ID))
	if err != nil {
		return CheckResult{}, errs.Wrap(errs.KindUnavailable, err,
			"failed to read the credential of resource %s", r.ID)
	}
	if len(value) == 0 {
		return CheckResult{}, errs.Precondition("integration %q has no credential to check", r.Name).
			WithCode(KeyCredentialMissing, nil)
	}
	if s.checker == nil {
		return CheckResult{Operated: false, Message: "no credential checker is wired"}, nil
	}
	return s.checker.Check(ctx, spec, []byte(value))
}
