package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Service concentrates the identity rules. It takes only PORTS.
type Service struct {
	repo   Repository
	clock  ports.Clock
	stepUp StepUpGate
	grants Grants
	mailer ports.Mailer
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
	KeyLastOwner            = "identity.membership.last_owner"
	KeyPersonalNotRemovable = "identity.membership.personal_not_removable"
	KeyInviteNotUsable      = "identity.invite.not_usable"
	KeyInviteNeedsSession   = "identity.invite.session_required"
	KeyInviteEmailUnverif   = "identity.invite.email_unverified"
	KeyInviteWrongRecipient = "identity.invite.wrong_recipient"
	KeyOnlyAdminsSetDefault = "identity.account.only_admins_default"
	KeyPolicyUnknown        = "identity.account.revocation_policy_unknown"
)

// MsgEmailBelongsToAnotherUser is the refusal an unknown subject gets when the
// address it arrives on is already somebody else's (spec US-7).
//
// It is a constant because TWO places produce it: EnsureUser's guard, which is
// the fast path, and the Postgres adapter, which catches the same collision off
// `users_email_uniq` when the guard could not see it — the index has no
// email_verified predicate, so an unverified row on either side slips past a
// guard that requires verification. Whoever lands on the floor instead of the
// fast path must not read a different sentence about the same situation, and
// the sentence names the CAUSE (a provider that is not linking accounts sharing
// an e-mail) because that is the only part anyone can act on.
const MsgEmailBelongsToAnotherUser = "this e-mail already belongs to another sign-in method; " +
	"the identity provider is not linking accounts that share an e-mail"

// NewService requires a clock. Accepting nil is what kept the port decorative:
// the service fell back to time.Now() internally, no expiry test was
// deterministic, and nobody noticed the abstraction was never proven. The panic
// here is deliberate — this is a wiring error, caught at boot, not in production
// at three in the morning.
// Option is how the OPTIONAL dependencies arrive. The repository and the clock
// are required and stay positional; a channel is not — most of what this service
// does sends nothing, and every one of its callers would otherwise have to name
// a mailer it has no use for.
type Option func(*Service)

// WithMailer wires the channel used by SendEmailVerification. Without it that
// one method refuses; everything else is unaffected.
func WithMailer(m ports.Mailer) Option {
	return func(s *Service) { s.mailer = m }
}

func NewService(repo Repository, clock ports.Clock, opts ...Option) *Service {
	if clock == nil {
		panic("identity.NewService: clock is required — use clock.NewSystem()")
	}
	s := &Service{repo: repo, clock: clock}
	for _, o := range opts {
		o(s)
	}
	return s
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

	if passwordOnly(p.Providers) && !p.EmailVerified {
		return nil, nil, errs.New(errs.KindPrecondition,
			"this e-mail has not been verified yet")
	}

	// Normalized ONCE, at the top, and used everywhere below. Looking up with the
	// raw address while writing the trimmed one meant a padded e-mail missed the
	// guard and then died on the unique index — the opaque failure the guard
	// exists to replace. Case is covered by citext and by the index's lower(),
	// but whitespace is not.
	email := strings.ToLower(strings.TrimSpace(p.Email))

	existing, err := s.repo.UserBySubject(ctx, p.Subject)
	if err != nil && errs.KindOf(err) != errs.KindNotFound {
		return nil, nil, err
	}

	// The e-mail is unique across the whole table (0001_foundation.sql), while
	// the lookup above is by subject. Those two only agree because the identity
	// provider links accounts that share an e-mail, which is configuration and
	// not code. When it stops agreeing, say why: without this the insert dies on
	// the unique index and the person reads "something went wrong".
	//
	// The verified predicate is load-bearing and stays: an UNVERIFIED claim must
	// never be enough to decide two subjects are the same person. That leaves
	// collisions this fast path cannot see — the index carries no
	// email_verified predicate — and those are caught by the adapter, which
	// returns this same refusal off the constraint itself.
	if existing == nil && email != "" && p.EmailVerified {
		if other, err := s.repo.UserByVerifiedEmail(ctx, email); err == nil && other != nil {
			return nil, nil, errs.New(errs.KindConflict, MsgEmailBelongsToAnotherUser)
		} else if err != nil && errs.KindOf(err) != errs.KindNotFound {
			return nil, nil, err
		}
	}

	u := &User{
		Subject:       p.Subject,
		Email:         email,
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

// ListInvites returns the active account's invites. It requires the role that
// can manage members: the list carries the addresses of people who were
// invited, and that is not public inside the account.
func (s *Service) ListInvites(ctx context.Context) ([]Invite, error) {
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
		return nil, errs.Permission("only an owner or admin may see the invites").
			WithCode(KeyOnlyAdminsInvite, nil)
	}
	return s.repo.InvitesOfAccount(ctx, accountID)
}

// InvitePreview is what whoever OPENS the link sees, before accepting.
//
// It does NOT carry the invitee's email. Whoever finds the link must not learn
// an address from it — that would turn it back into the oracle that taking the
// token out was meant to end (ADR-0026).
type InvitePreview struct {
	ID          string
	AccountName string
	Role        Role
	Status      InviteStatus
	ExpiresAt   time.Time
	Usable      bool
}

// GetInvite is the ONE identity operation that does not require an active
// account: whoever opens the link may not be a member of anything yet — that is
// the point of an invite.
//
// It requires a SESSION all the same. Without one, an id found in a log would
// tell a stranger that an account named X invited somebody as an admin.
func (s *Service) GetInvite(ctx context.Context, inviteID string) (*InvitePreview, error) {
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "reading an invite requires a session").
			WithCode(KeyInviteNeedsSession, nil)
	}
	inv, err := s.repo.InviteByID(ctx, inviteID)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		return nil, errs.NotFound("invite")
	}
	acct, err := s.repo.AccountByID(ctx, inv.AccountID)
	if err != nil {
		return nil, err
	}
	return &InvitePreview{
		ID:          inv.ID,
		AccountName: acct.DisplayName,
		Role:        inv.Role,
		Status:      inv.Status,
		ExpiresAt:   inv.ExpiresAt,
		Usable:      inv.IsUsable(s.now()),
	}, nil
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
// assertNotTheLastOwner refuses to leave an account without an owner.
//
// It is not a permission rule — whoever gets here is already allowed to change
// roles. It is about RECOVERY: only an owner may hand ownership over, so an
// account whose last owner demoted themselves cannot be fixed from inside; the
// way back is a hand at the database. One click on a select, and this screen
// would make that easy.
//
// It only reads the members when the new role is not owner, which is where the
// number of owners can drop.
func (s *Service) assertNotTheLastOwner(ctx context.Context, accountID, membershipID string, role Role) error {
	if role == RoleOwner {
		return nil
	}
	return s.assertLeavesAnOwner(ctx, accountID, membershipID)
}

// assertLeavesAnOwner is the check itself, shared by the demotion and the
// removal — the two ways the number of owners can drop.
func (s *Service) assertLeavesAnOwner(ctx context.Context, accountID, membershipID string) error {
	members, err := s.repo.MembershipsOfAccount(ctx, accountID)
	if err != nil {
		return err
	}
	owners, demotingAnOwner := 0, false
	for _, m := range members {
		if m.Role != RoleOwner {
			continue
		}
		owners++
		if m.ID == membershipID {
			demotingAnOwner = true
		}
	}
	if demotingAnOwner && owners == 1 {
		return errs.Precondition("the account needs at least one owner").
			WithCode(KeyLastOwner, nil)
	}
	return nil
}

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
	if err := s.assertNotTheLastOwner(ctx, accountID, membershipID, role); err != nil {
		return nil, err
	}
	if err := s.requireStepUp(ctx); err != nil {
		return nil, err
	}
	return s.repo.UpdateMembershipRole(ctx, membershipID, role)
}

// Grants is what identity needs from the RESOURCE domain when somebody leaves
// an account: the grants they held there stop existing with them.
//
// It is a port and not a direct call because grants are not identity's data.
// The composition root wires the resource service in, the same way it wires the
// second factor's gate.
type Grants interface {
	// RevokeAllOfMember removes every grant the person holds IN THIS ACCOUNT.
	// It does not authorize: whoever calls it has already decided.
	RevokeAllOfMember(ctx context.Context, accountID, userID string) error
}

// WithGrants wires the sweep. Without it, RemoveMembership refuses — a removal
// that leaves grants behind is exactly the hole it exists to close.
func (s *Service) WithGrants(g Grants) *Service {
	s.grants = g
	return s
}

// RemoveMembership takes somebody out of the account.
//
// What it does NOT touch is as important as what it does: the person's user,
// their personal account and everything in it stay untouched — they were never
// in this account (US-5.3). What the person CREATED here also stays: a resource
// belongs to the account, and `created_by` keeps the trail of who made it. What
// goes away is the membership and, with it, the grants — a grant outliving the
// membership would be access with no membership to justify it.
//
// The order is deliberate: the grants FIRST, the membership after. If the sweep
// fails, nothing was removed. Doing it the other way round, a failure between
// the two steps would leave exactly the orphan grant this exists to prevent.
func (s *Service) RemoveMembership(ctx context.Context, membershipID string) error {
	call, _ := ctxutil.From(ctx)
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return err
	}
	actor, err := s.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return err
	}
	if !actor.Role.CanManageMembers() {
		return errs.Permission("only an owner or admin may remove a member").
			WithCode(KeyOnlyAdminsSetRole, nil)
	}
	m, err := s.repo.MembershipByID(ctx, membershipID)
	if err != nil {
		return err
	}
	if m == nil || m.AccountID != accountID {
		// Another account's id answers the same as one that does not exist: the
		// difference between the two would tell whoever asked that the row is
		// real somewhere else.
		return errs.NotFound("membership")
	}
	acct, err := s.repo.AccountByID(ctx, accountID)
	if err != nil {
		return err
	}
	if acct != nil && acct.Kind == AccountPersonal {
		// A personal account has exactly one membership, and removing it would
		// be deleting the user — another operation, with another meaning and
		// another set of rules (P-3).
		return errs.Precondition("a personal account's membership is not removable").
			WithCode(KeyPersonalNotRemovable, nil)
	}
	if err := s.assertLeavesAnOwner(ctx, accountID, membershipID); err != nil {
		return err
	}
	if err := s.requireStepUp(ctx); err != nil {
		return err
	}
	if s.grants == nil {
		return errs.New(errs.KindInternal, "the grants sweep is not wired")
	}
	if err := s.grants.RevokeAllOfMember(ctx, accountID, m.UserID); err != nil {
		return err
	}
	return s.repo.RemoveMembership(ctx, membershipID)
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

// SetDefaultRevocationPolicy changes the value that will be STAMPED on future
// flow grants (flow sharing spec §3.2). It never touches grants already made:
// the terms somebody accepted are theirs, and reading the account at
// revocation time — instead of the grant — would let the publisher change
// those terms after the fact.
//
// The vocabulary is checked here, against plain strings, not against
// workflow.RevocationPolicy: this domain does not import workflow, the same
// way it does not import Postgres.
func (s *Service) SetDefaultRevocationPolicy(ctx context.Context, policy string) error {
	call, _ := ctxutil.From(ctx)
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return err
	}
	actor, err := s.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return err
	}
	if !actor.Role.CanManageMembers() {
		return errs.Permission("changing the account's default requires owner or admin").
			WithCode(KeyOnlyAdminsSetDefault, nil)
	}
	// These three literals ARE workflow.PolicyProspective, workflow.PolicyDrain
	// and workflow.PolicyTerminate — repeated, not imported, because identity
	// must not depend on the workflow domain's vocabulary (the same house rule
	// that keeps every domain package free of a sibling domain's types, mirrored
	// by the DefaultRevocationPolicy field on Account being a plain string).
	// Adding or renaming a policy means editing both this switch and
	// internal/domain/workflow/sharing.go; nothing but this comment ties them
	// together, so drifting apart here would validate the wrong set silently.
	switch policy {
	case "prospective", "drain", "terminate":
	default:
		return errs.Invalid("unknown revocation policy: %q — use prospective, drain or terminate", policy).
			WithCode(KeyPolicyUnknown, map[string]any{"policy": policy})
	}
	return s.repo.SetDefaultRevocationPolicy(ctx, accountID, policy)
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

// socialProviders is the set of providers this platform enables whose own
// authentication already vouches for the person (spec D-5). The strings are the
// ones the ADAPTERS emit, not invented labels: Firebase puts "google.com" and
// "github.com" in `sign_in_provider` and in the keys of `firebase.identities`,
// and normalizeProviders lowercases both adapters' vocabularies into the same
// shape.
//
// Adding a provider to the platform means adding it here. Until somebody does,
// that provider's users whose e-mail is unverified are REFUSED — see
// passwordOnly for why that is the direction we want the mistake to point in.
var socialProviders = map[string]bool{
	"google.com": true,
	"github.com": true,
}

// passwordOnly answers whether a password is the ONLY thing vouching for this
// person. It is the distinction the verification rule turns on: with a password
// the e-mail is the sole link between the credential and a human, and nobody
// checked it; with a social provider the provider already did the checking, and
// the e-mail is metadata. Refusing every unverified e-mail would lock out
// GitHub, which frequently delivers one (spec D-5).
//
// It asks whether a SOCIAL provider is present instead of asking whether EVERY
// entry is "password", and the direction is the entire point. Firebase keys
// `firebase.identities` by identifier TYPE, so a real e-mail/password token
// arrives as ["email","password"] — under "every entry must be password" the
// rule answered false for exactly the credential it exists to stop, and any
// unfamiliar value silently switched it OFF. That is fail-open, in the one place
// that must not be.
//
// This way round an UNKNOWN value counts as not-social and the rule still fires.
// The cost is stated so nobody meets it by surprise: enabling a new social
// provider without listing it in socialProviders refuses that provider's users
// whose e-mail is unverified until somebody adds it. A locked-out user tells us;
// a rule that quietly stopped running does not.
func passwordOnly(providers []string) bool {
	password := false
	for _, p := range providers {
		if socialProviders[p] {
			return false
		}
		if p == "password" {
			password = true
		}
	}
	return password
}

func randomSuffix(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// ── Proving the address ──────────────────────────────────────────────────────

// ResendInterval is the floor between two verification messages to the SAME
// address. Same value as the second factor's, and for the same reason: it is
// long enough that a person who did not receive the first one has actually
// looked, and short enough that they do not give up.
const ResendInterval = 60 * time.Second

// MaxVerificationsPerHour is the ceiling per address. Five is generous for
// somebody genuinely stuck and cheap enough that using this endpoint to post
// mail to a stranger costs more than it is worth.
const MaxVerificationsPerHour = 5

// VerificationWindow is the window the ceiling is counted in.
const VerificationWindow = time.Hour

// SendEmailVerification sends the message that proves a password credential's
// address (spec SP-0 US-2).
//
// The link is GENERATED ELSEWHERE and arrives ready: the BFF holds the Firebase
// Admin capability and this core does not. What is here is the part that must
// not be improvised — the rate limit, which needs state, and the channel, which
// is the platform's and not a provider's.
//
// The rate limit is keyed on the ADDRESS and not on a user, because at this
// moment THERE IS NO USER: EnsureUser refuses an unverified password credential
// before creating anything. Whoever asks for this message exists in Firebase and
// nowhere on this side.
//
// Read it also as what it would be if it were open: an endpoint that sends
// arbitrary text to an arbitrary address is a mail relay. It is not open —
// ADR-0029 means only a signed caller reaches it — and the ceiling is the second
// line, for the day the first one has a hole.
func (s *Service) SendEmailVerification(ctx context.Context, email, subject, link, name string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return errs.Invalid("e-mail not provided")
	}
	if link == "" {
		// Sending the message without the link would produce an e-mail that
		// looks right, arrives, and cannot be acted on — the worst of the three
		// possible failures, because nobody reports it as broken.
		return errs.Invalid("verification link not provided")
	}
	if s.mailer == nil {
		return errs.New(errs.KindUnavailable, "no channel wired for the verification message")
	}

	count, last, err := s.repo.VerificationRequestsSince(ctx, email, s.clock.Now().Add(-VerificationWindow))
	if err != nil {
		return err
	}
	if !last.IsZero() {
		if wait := ResendInterval - s.clock.Now().Sub(last); wait > 0 {
			return errs.Precondition("wait %d seconds before asking for another message",
				int(wait.Seconds()+0.999))
		}
	}
	if count >= MaxVerificationsPerHour {
		return errs.Precondition("too many verification messages for this address; try again later")
	}

	if _, err := s.mailer.Send(ctx, ports.Mail{
		Kind:   string(notification.KindEmailVerification),
		To:     email,
		ToName: name,
		Data:   map[string]any{"link": link, "name": name},
	}); err != nil {
		return err
	}
	// Recorded AFTER the send, on purpose: a provider that refused the message
	// consumed nobody's allowance. The opposite order would let a broken channel
	// lock a person out of retrying once it came back.
	return s.repo.RecordVerificationRequest(ctx, email, subject)
}
