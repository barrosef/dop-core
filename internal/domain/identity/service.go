package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service concentrates the identity rules. It takes only PORTS.
type Service struct {
	repo   Repository
	clock  ports.Clock
	stepUp StepUpGate
}

// StepUpGate is the second factor's gate, in the narrowest possible shape: one
// question, one answer (ADR-0027 §5).
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

// Translation keys for the refusals a person reads.
const (
	KeyRoleUnknown          = "identity.role.unknown"
	KeyEmailInvalid         = "identity.email.invalid"
	KeyGrantLevelInvalid    = "identity.grant.level_invalid"
	KeyLegalIDRequired      = "identity.account.legal_id_required"
	KeyHandleTaken          = "identity.handle.taken"
	KeyNoMembership         = "identity.membership.absent"
	KeyOnlyAdminsInvite     = "identity.invite.only_admins"
	KeyOnlyAdminsRevoke     = "identity.invite.only_admins_revoke"
	KeyOnlyAdminsSetRole    = "identity.membership.only_admins"
	KeyInviteNotUsable      = "identity.invite.not_usable"
	KeyInviteNeedsSession   = "identity.invite.session_required"
	KeyInviteEmailUnverif   = "identity.invite.email_unverified"
	KeyInviteWrongRecipient = "identity.invite.wrong_recipient"
)

// NewService requires a clock. Accepting nil is what kept the port decorative:
// the service fell back to time.Now() internally, no expiry test was
// deterministic, and nobody noticed the abstraction was never proven. The panic
// here is deliberate — this is a wiring error, caught at boot, not in production
// at three in the morning.
func NewService(repo Repository, clock ports.Clock) *Service {
	if clock == nil {
		panic("identity.NewService: clock is required — use clock.NewSystem()")
	}
	return &Service{repo: repo, clock: clock}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// EnsureUser is called on EVERY first login and has to be idempotent.
//
// This is where the account-linking decision lives: the same email arriving
// through a different provider resolves to the SAME user. Without it, someone
// who signed in with Google and later with GitHub becomes two users — and a
// duplicate in a multi-tenant system is not a cosmetic nuisance, it is access
// confusion (spec SP-0 §1).
//
// The personal account is born together with the user: the user does not need
// to know that happened, they just see "my account" in the selector.
func (s *Service) EnsureUser(ctx context.Context, p ports.Principal) (*User, *Account, error) {
	if p.Subject == "" {
		return nil, nil, errs.Invalid("principal with no subject")
	}

	existing, err := s.repo.UserBySubject(ctx, p.Subject)
	if err != nil && errs.KindOf(err) != errs.KindNotFound {
		return nil, nil, err
	}

	u := &User{
		Subject:       p.Subject,
		Email:         strings.ToLower(strings.TrimSpace(p.Email)),
		EmailVerified: p.EmailVerified,
		Name:          p.Name,
		AvatarURL:     p.AvatarURL,
		Providers:     p.Providers,
	}
	if existing != nil {
		u.ID = existing.ID
		// Preserve whatever the new provider did not bring.
		if u.Name == "" {
			u.Name = existing.Name
		}
		if u.AvatarURL == "" {
			u.AvatarURL = existing.AvatarURL
		}
		u.Providers = mergeProviders(existing.Providers, p.Providers)
	}

	saved, err := s.repo.UpsertUser(ctx, u)
	if err != nil {
		return nil, nil, err
	}

	// Is there already a personal account?
	accounts, _, err := s.repo.AccountsOfUser(ctx, saved.ID)
	if err != nil {
		return nil, nil, err
	}
	for i := range accounts {
		if accounts[i].Kind == AccountPersonal {
			return saved, &accounts[i], nil
		}
	}

	personal, err := s.createPersonalAccount(ctx, saved)
	if err != nil {
		return nil, nil, err
	}
	return saved, personal, nil
}

// createPersonalAccount derives the handle from the email and resolves
// collisions with a suffix — the user can change it later.
func (s *Service) createPersonalAccount(ctx context.Context, u *User) (*Account, error) {
	base := NormalizeHandle(u.Email)
	if base == "" {
		base = NormalizeHandle(u.Name)
	}
	if len(base) < handleMinLen {
		base = "dev-" + u.ID[:8]
	}

	handle := base
	for attempt := 0; attempt < 50; attempt++ {
		if attempt > 0 {
			handle = base + "-" + randomSuffix(4)
		}
		found, err := s.repo.AccountByHandle(ctx, handle)
		if err != nil && errs.KindOf(err) != errs.KindNotFound {
			return nil, err
		}
		if found != nil {
			continue
		}
		name := u.Name
		if name == "" {
			name = u.Email
		}
		return s.repo.CreateAccountWithOwner(ctx, &Account{
			Kind:        AccountPersonal,
			Handle:      handle,
			DisplayName: name,
		}, u.ID)
	}
	return nil, errs.Internal("could not derive a free handle")
}

// CreateOrganization creates the organization account; its creator becomes
// owner. No waiting and no paperwork: legitimacy comes from domain
// verification, done later (ADR-0004).
func (s *Service) CreateOrganization(ctx context.Context, handle, displayName, legalID string) (*Account, error) {
	call, ok := ctxutil.From(ctx)
	if !ok || call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "actor not identified")
	}
	handle = NormalizeHandle(handle)
	if err := ValidateHandle(handle); err != nil {
		return nil, err
	}
	if strings.TrimSpace(legalID) == "" {
		return nil, errs.Invalid("a company registration number is required for an organization account").
			WithCode(KeyLegalIDRequired, nil)
	}
	if found, err := s.repo.AccountByHandle(ctx, handle); err != nil {
		if errs.KindOf(err) != errs.KindNotFound {
			return nil, err
		}
	} else if found != nil {
		return nil, errs.New(errs.KindAlreadyExists, "the handle %q is already taken", handle).
			WithCode(KeyHandleTaken, map[string]any{"handle": handle})
	}
	return s.repo.CreateAccountWithOwner(ctx, &Account{
		Kind:        AccountOrganization,
		Handle:      handle,
		DisplayName: displayName,
		LegalID:     legalID,
	}, call.ActorID)
}

// ListAccounts feeds the active-account selector: the personal one plus every
// organization the user has a membership in.
func (s *Service) ListAccounts(ctx context.Context, userID string) ([]Account, []Membership, error) {
	if userID == "" {
		return nil, nil, errs.Invalid("user not provided")
	}
	return s.repo.AccountsOfUser(ctx, userID)
}

// PersonalAccountOf returns the id of the user's personal account.
//
// It exists for the second factor: the TOTP seed is a user's secret, and the
// vault's reference needs an isolation scope. The personal account IS the
// person inside the platform (ADR-0002) — it is born with the user, it has
// exactly one member and it is the only scope that means "this belongs to that
// person, not to a company they happen to be in".
func (s *Service) PersonalAccountOf(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", errs.Invalid("user not provided")
	}
	accounts, _, err := s.repo.AccountsOfUser(ctx, userID)
	if err != nil {
		return "", err
	}
	for _, a := range accounts {
		if a.Kind == AccountPersonal {
			return a.ID, nil
		}
	}
	// It does not happen through the normal path — EnsureUser creates it in the
	// same transaction as the user — and if it ever does, the honest answer is
	// a refusal, not an empty scope that would put the secret somewhere nobody
	// can find it again.
	return "", errs.Precondition("the user has no personal account")
}

// Authorize resolves the actor's role and grants in the active account.
// It is what the BFF queries to fill the decorators' AuthContext.
func (s *Service) Authorize(ctx context.Context, userID, accountID string) (*Membership, error) {
	if accountID == "" {
		return nil, ctxutil.ErrNoAccount
	}
	m, err := s.repo.MembershipOf(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errs.Permission("no membership in this account").WithCode(KeyNoMembership, nil)
	}
	return m, nil
}

// CreateInvite composes role and grants INTO THE INVITE — no defaults.
func (s *Service) CreateInvite(ctx context.Context, email string, role Role, grants []GrantSpec) (*Invite, error) {
	call, _ := ctxutil.From(ctx)
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidRole(role) {
		return nil, errs.Invalid("unknown role: %q", role).
			WithCode(KeyRoleUnknown, map[string]any{"role": string(role)})
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, errs.Invalid("invalid email").WithCode(KeyEmailInvalid, nil)
	}
	for _, g := range grants {
		if g.Level != "use" && g.Level != "manage" {
			return nil, errs.Invalid("invalid grant level: %q", g.Level).
				WithCode(KeyGrantLevelInvalid, map[string]any{"level": g.Level})
		}
	}

	// Whoever invites has to be able to manage members.
	actor, err := s.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !actor.Role.CanManageMembers() {
		return nil, errs.Permission("only an owner or admin may invite").
			WithCode(KeyOnlyAdminsInvite, nil)
	}
	// The second factor's gate (ADR-0027 §5): whoever invites is handing out a
	// key to the account. It comes AFTER the role check, so somebody with no
	// business inviting learns that first — the second factor is not a way to
	// hide what the permission already refuses.
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}

	inv := &Invite{
		AccountID: accountID,
		Email:     email,
		Role:      role,
		Grants:    grants,
		Status:    InvitePending,
		InvitedBy: call.ActorID,
		ExpiresAt: s.now().Add(InviteTTL),
	}
	saved, err := s.repo.CreateInvite(ctx, inv)
	if err != nil {
		return nil, err
	}
	// There is NO token any more. Acceptance checks the session's VERIFIED
	// email against the invite's, so the link only has to ADDRESS the invite —
	// and an id that grants nothing can travel in the email, in the event and
	// in the timeline without becoming a credential at rest.
	return saved, nil
}

// AcceptInvite validates expiry by TIME, not only by status: the sweep of
// expired invites may not have run yet.
func (s *Service) AcceptInvite(ctx context.Context, inviteID, userID string) (*Membership, error) {
	if userID == "" {
		return nil, errs.New(errs.KindUnauthorized, "acceptance requires an authenticated session").
			WithCode(KeyInviteNeedsSession, nil)
	}
	inv, err := s.repo.InviteByID(ctx, inviteID)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		return nil, errs.NotFound("invite")
	}
	if !inv.IsUsable(s.now()) {
		return nil, errs.Precondition("invite is %s", inv.Status).
			WithCode(KeyInviteNotUsable, map[string]any{"status": string(inv.Status)})
	}

	// The invite is for ONE person, and it now requires them to be that person.
	//
	// Acceptance used to check only the token: any authenticated user holding
	// the link entered the account, with the role granted to somebody else. It
	// was a BEARER credential, and that is why it could not appear in an event
	// or a projection — which in turn kept the email from carrying an
	// acceptance link.
	//
	// By requiring the session's VERIFIED email, the link stops granting
	// anything to whoever merely holds it: you have to BE the invitee. That is
	// what freed `invite_id` to travel in the clear.
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, errs.New(errs.KindUnauthorized, "session without a user")
	}
	// Unverified is a SEPARATE refusal from a mismatch: "confirm your email"
	// and "this invite is not yours" send the person to do different things,
	// and a single error would make both look like the same wall.
	if !u.EmailVerified {
		return nil, errs.Precondition(
			"acceptance requires a verified email: confirm %s before joining the account", u.Email).
			WithCode(KeyInviteEmailUnverif, map[string]any{"email": u.Email})
	}
	if !strings.EqualFold(strings.TrimSpace(u.Email), strings.TrimSpace(inv.Email)) {
		// The message does NOT say who the invite was for: that would turn the
		// link into an email oracle for whoever found it. The params are empty
		// for the same reason — a translated sentence must not be able to leak
		// what the English one refuses to.
		return nil, errs.New(errs.KindPermission, "this invite was issued to a different email").
			WithCode(KeyInviteWrongRecipient, nil)
	}

	return s.repo.AcceptInvite(ctx, inv.ID, userID)
}

func (s *Service) RevokeInvite(ctx context.Context, inviteID string) (*Invite, error) {
	call, _ := ctxutil.From(ctx)
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	actor, err := s.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !actor.Role.CanManageMembers() {
		return nil, errs.Permission("only an owner or admin may revoke invites").
			WithCode(KeyOnlyAdminsRevoke, nil)
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	return s.repo.RevokeInvite(ctx, accountID, inviteID)
}

// UpdateMembershipRole changes a member's role.
//
// The invariant "every account has at least one active owner" is enforced by a
// database TRIGGER — a rule no operation can violate, not even through a path
// nobody anticipated.
func (s *Service) UpdateMembershipRole(ctx context.Context, membershipID string, role Role) (*Membership, error) {
	call, _ := ctxutil.From(ctx)
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidRole(role) {
		return nil, errs.Invalid("unknown role: %q", role).
			WithCode(KeyRoleUnknown, map[string]any{"role": string(role)})
	}
	actor, err := s.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !actor.Role.CanManageMembers() {
		return nil, errs.Permission("only an owner or admin may change memberships").
			WithCode(KeyOnlyAdminsSetRole, nil)
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	return s.repo.UpdateMembershipRole(ctx, membershipID, role)
}

func (s *Service) ListMemberships(ctx context.Context) ([]Membership, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.MembershipsOfAccount(ctx, accountID)
}

func (s *Service) GetUser(ctx context.Context, id string) (*User, error) {
	return s.repo.UserByID(ctx, id)
}

func (s *Service) GetAccount(ctx context.Context, id string) (*Account, error) {
	return s.repo.AccountByID(ctx, id)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func mergeProviders(existing, incoming []string) []string {
	seen := make(map[string]bool, len(existing)+len(incoming))
	out := make([]string, 0, len(existing)+len(incoming))
	for _, list := range [][]string{existing, incoming} {
		for _, p := range list {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func randomSuffix(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
