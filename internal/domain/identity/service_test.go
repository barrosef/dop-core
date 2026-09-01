package identity_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT a database: the repository is a port, and an
// in-memory double goes in here. It is the practical return on hexagonal
// architecture.
func TestNormalizeHandle(t *testing.T) {
	cases := map[string]string{
		"dev@dop.local":     "dev",
		"Ed Barros":         "ed-barros",
		"  UPPER@x.com  ":   "upper",
		"a..b__c":           "a-b-c",
		"---trim---":        "trim",
		"maria.silva@x.com": "maria-silva",
	}
	for input, want := range cases {
		if got := identity.NormalizeHandle(input); got != want {
			t.Errorf("NormalizeHandle(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateHandle(t *testing.T) {
	if err := identity.ValidateHandle("ed"); err != nil {
		t.Errorf("a valid minimum handle was refused: %v", err)
	}
	if err := identity.ValidateHandle("a"); err == nil {
		t.Error("a 1-character handle should be refused")
	}
	if err := identity.ValidateHandle("Ed_Barros"); err == nil {
		t.Error("uppercase and underscore should be refused")
	}
}

func TestRoles(t *testing.T) {
	if !identity.RoleOwner.CanManageMembers() || !identity.RoleAdmin.CanManageMembers() {
		t.Error("owner and admin must be able to manage members")
	}
	if identity.RoleDeveloper.CanManageMembers() || identity.RoleViewer.CanManageMembers() {
		t.Error("developer and viewer must NOT manage members")
	}
	// Without implicit manage, nobody can fix a broken integration.
	if !identity.RoleOwner.HasImplicitManage() || !identity.RoleAdmin.HasImplicitManage() {
		t.Error("owner and admin must have implicit manage")
	}
	if identity.RoleDeveloper.HasImplicitManage() {
		t.Error("developer does NOT have implicit manage")
	}
	if identity.ValidRole("superuser") {
		t.Error("a role outside the vocabulary should be refused")
	}
}

func TestInviteExpiresByTime(t *testing.T) {
	ref := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	inv := identity.Invite{Status: identity.InvitePending, ExpiresAt: ref.Add(time.Hour)}
	if !inv.IsUsable(ref) {
		t.Error("a pending invite within its deadline should be usable")
	}
	// Expiry is by TIME, not only by status: the sweep may not have run.
	if inv.IsUsable(ref.Add(2 * time.Hour)) {
		t.Error("an expired invite should be refused even while its status is pending")
	}
	revoked := identity.Invite{Status: identity.InviteRevoked, ExpiresAt: ref.Add(time.Hour)}
	if revoked.IsUsable(ref) {
		t.Error("a revoked invite is never usable")
	}
}

// ── in-memory double ────────────────────────────────────────────────────────

// fixedClock is the Clock double. It lives here, and not in
// internal/adapter/clock, because the architecture test rejects ANY adapter
// import under internal/domain — including from a _test.go file. The contract
// suite (test/contract/clock.go) is what guarantees this double and the real
// clock honour the same promises.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// The tests' base instant: fixed, so that invite expiry (14 days) is verifiable
// by equality rather than by tolerance.
var now = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

type fakeRepo struct {
	users    map[string]*identity.User // by subject
	byID     map[string]*identity.User
	accounts map[string]*identity.Account
	byHandle map[string]*identity.Account
	members  []identity.Membership
	invites  map[string]*identity.Invite
	accepted []string
	nextID   int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		users: map[string]*identity.User{}, byID: map[string]*identity.User{},
		accounts: map[string]*identity.Account{}, byHandle: map[string]*identity.Account{},
	}
}

func (f *fakeRepo) id(prefix string) string {
	f.nextID++
	return prefix + "-" + string(rune('a'+f.nextID))
}

func (f *fakeRepo) UserBySubject(_ context.Context, s string) (*identity.User, error) {
	if u, ok := f.users[s]; ok {
		return u, nil
	}
	return nil, errs.NotFound("user")
}
func (f *fakeRepo) UserByID(_ context.Context, id string) (*identity.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, errs.NotFound("user")
}
func (f *fakeRepo) UpsertUser(_ context.Context, u *identity.User) (*identity.User, error) {
	if u.ID == "" {
		u.ID = f.id("usr")
	}
	cp := *u
	f.users[u.Subject] = &cp
	f.byID[u.ID] = &cp
	return &cp, nil
}
func (f *fakeRepo) AccountByID(_ context.Context, id string) (*identity.Account, error) {
	if a, ok := f.accounts[id]; ok {
		return a, nil
	}
	return nil, errs.NotFound("account")
}
func (f *fakeRepo) AccountByHandle(_ context.Context, h string) (*identity.Account, error) {
	if a, ok := f.byHandle[h]; ok {
		return a, nil
	}
	return nil, errs.NotFound("account")
}
func (f *fakeRepo) CreateAccountWithOwner(_ context.Context, a *identity.Account, owner string) (*identity.Account, error) {
	a.ID = f.id("acct")
	cp := *a
	f.accounts[a.ID] = &cp
	f.byHandle[a.Handle] = &cp
	f.members = append(f.members, identity.Membership{
		ID: f.id("mem"), UserID: owner, AccountID: a.ID, Role: identity.RoleOwner,
	})
	return &cp, nil
}
func (f *fakeRepo) AccountsOfUser(_ context.Context, uid string) ([]identity.Account, []identity.Membership, error) {
	var accs []identity.Account
	var mems []identity.Membership
	for _, m := range f.members {
		if m.UserID == uid {
			if a, ok := f.accounts[m.AccountID]; ok {
				accs = append(accs, *a)
				mems = append(mems, m)
			}
		}
	}
	return accs, mems, nil
}
func (f *fakeRepo) MembershipsOfAccount(_ context.Context, aid string) ([]identity.Membership, error) {
	var out []identity.Membership
	for _, m := range f.members {
		if m.AccountID == aid {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fakeRepo) MembershipOf(_ context.Context, uid, aid string) (*identity.Membership, error) {
	for i := range f.members {
		if f.members[i].UserID == uid && f.members[i].AccountID == aid {
			return &f.members[i], nil
		}
	}
	return nil, nil
}
func (f *fakeRepo) UpdateMembershipRole(_ context.Context, id string, r identity.Role) (*identity.Membership, error) {
	for i := range f.members {
		if f.members[i].ID == id {
			f.members[i].Role = r
			return &f.members[i], nil
		}
	}
	return nil, errs.NotFound("membership")
}
func (f *fakeRepo) CreateInvite(_ context.Context, inv *identity.Invite) (*identity.Invite, error) {
	inv.ID = f.id("inv")
	if f.invites == nil {
		f.invites = map[string]*identity.Invite{}
	}
	f.invites[inv.ID] = inv
	return inv, nil
}
func (f *fakeRepo) InviteByID(_ context.Context, id string) (*identity.Invite, error) {
	return f.invites[id], nil
}
func (f *fakeRepo) AcceptInvite(_ context.Context, inviteID, userID string) (*identity.Membership, error) {
	f.accepted = append(f.accepted, inviteID+"/"+userID)
	return &identity.Membership{ID: f.id("mem"), UserID: userID}, nil
}
func (f *fakeRepo) RevokeInvite(context.Context, string, string) (*identity.Invite, error) {
	return nil, nil
}

// ── service tests ───────────────────────────────────────────────────────────

func TestEnsureUserCreatesThePersonalAccount(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})

	u, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "sub-1", Email: "dev@dop.local", Name: "Dev", Providers: []string{"password"},
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if acct == nil || acct.Kind != identity.AccountPersonal {
		t.Fatal("the personal account should be born together with the user")
	}
	if acct.Handle != "dev" {
		t.Errorf("the handle should derive from the email: %q", acct.Handle)
	}
	// The creator is owner — and a personal account has exactly one membership.
	mems, _ := repo.MembershipsOfAccount(context.Background(), acct.ID)
	if len(mems) != 1 || mems[0].Role != identity.RoleOwner || mems[0].UserID != u.ID {
		t.Errorf("owner membership missing or wrong: %+v", mems)
	}
}

func TestEnsureUserIsIdempotent(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()
	p := ports.Principal{Subject: "sub-1", Email: "dev@dop.local", Providers: []string{"password"}}

	u1, a1, _ := svc.EnsureUser(ctx, p)
	u2, a2, err := svc.EnsureUser(ctx, p)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if u1.ID != u2.ID {
		t.Error("EnsureUser created a duplicate user")
	}
	if a1.ID != a2.ID {
		t.Error("EnsureUser created a duplicate personal account")
	}
}

func TestAccountLinkingAccumulatesProviders(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()

	svc.EnsureUser(ctx, ports.Principal{Subject: "sub-1", Email: "dev@dop.local", Providers: []string{"password"}})
	u, _, err := svc.EnsureUser(ctx, ports.Principal{
		Subject: "sub-1", Email: "dev@dop.local", Providers: []string{"google.com"},
	})
	if err != nil {
		t.Fatalf("second provider: %v", err)
	}
	if len(u.Providers) != 2 {
		t.Errorf("both providers should add up, got %v", u.Providers)
	}
}

func TestCreateInviteRequiresPermission(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()

	u, acct, _ := svc.EnsureUser(ctx, ports.Principal{Subject: "s1", Email: "owner@x.com"})
	// a developer does not invite
	repo.members[0].Role = identity.RoleDeveloper
	ctx = ctxutil.Into(ctx, ctxutil.Call{AccountID: acct.ID, ActorID: u.ID, ActorKind: ctxutil.ActorUser})

	_, err := svc.CreateInvite(ctx, "newcomer@x.com", identity.RoleDeveloper, nil)
	if err == nil || errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("a developer should not be able to invite; error: %v", err)
	}

	repo.members[0].Role = identity.RoleAdmin
	inv, err := svc.CreateInvite(ctx, "newcomer@x.com", identity.RoleDeveloper,
		[]identity.GrantSpec{{ResourceID: "res-1", Level: "use"}})
	if err != nil {
		t.Fatalf("an admin should be able to invite: %v", err)
	}
	if inv.ID == "" {
		t.Error("the invite needs an id: it is what the link carries")
	}
	if len(inv.Grants) != 1 {
		t.Error("grants composed into the invite should be preserved")
	}
}

func TestCreateInviteRefusesAnInvalidLevel(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()
	u, acct, _ := svc.EnsureUser(ctx, ports.Principal{Subject: "s1", Email: "owner@x.com"})
	ctx = ctxutil.Into(ctx, ctxutil.Call{AccountID: acct.ID, ActorID: u.ID})

	if _, err := svc.CreateInvite(ctx, "n@x.com", identity.RoleDeveloper,
		[]identity.GrantSpec{{ResourceID: "r", Level: "admin"}}); err == nil {
		t.Error("a grant level outside use|manage should be refused")
	}
}

func TestOperationWithoutAnActiveAccountIsRefused(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	// No AccountID: the SP-0 rule — a request with no active account is invalid.
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "u1"})
	if _, err := svc.CreateInvite(ctx, "a@b.com", identity.RoleViewer, nil); err == nil {
		t.Error("an operation with no active account should be refused")
	}
}

// Invite expiry used to be checkable only by tolerance — the service read the
// wall clock internally. With the port injected, the exact instant can be
// asserted, and the 14-day boundary crossed without sleeping.
func TestInviteExpiresExactlyInFourteenDays(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()

	u, acct, _ := svc.EnsureUser(ctx, ports.Principal{Subject: "s1", Email: "owner@x.com"})
	ctx = ctxutil.Into(ctx, ctxutil.Call{AccountID: acct.ID, ActorID: u.ID, ActorKind: ctxutil.ActorUser})

	invite, err := svc.CreateInvite(ctx, "newcomer@dop.dev", identity.RoleDeveloper, nil)
	if err != nil {
		t.Fatalf("creating the invite: %v", err)
	}

	if want := now.Add(identity.InviteTTL); !invite.ExpiresAt.Equal(want) {
		t.Fatalf("expiry at %v, expected %v", invite.ExpiresAt, want)
	}

	// An instant BEFORE the deadline still works; at the deadline, it does not.
	// The boundary is closed at the top: `now.Before(ExpiresAt)`.
	if !invite.IsUsable(invite.ExpiresAt.Add(-time.Nanosecond)) {
		t.Error("the invite should hold at the last instant before expiring")
	}
	if invite.IsUsable(invite.ExpiresAt) {
		t.Error("the invite must not hold at the exact instant of expiry")
	}
}

// ── invite acceptance: the link is not a credential ─────────────────────────
//
// These tests are the entire security argument for the tokenless invite. If any
// of them starts accepting someone it should not, `invite_id` becomes a secret
// again — and it travels in email, in events and in projections.

// inviteFor creates the owner's account, promotes them to admin and returns the
// invite.
func inviteFor(t *testing.T, repo *fakeRepo, svc *identity.Service, email string) *identity.Invite {
	t.Helper()
	ctx := context.Background()
	u, acct, _ := svc.EnsureUser(ctx, ports.Principal{Subject: "owner", Email: "owner@x.com"})
	repo.members[0].Role = identity.RoleAdmin
	ctx = ctxutil.Into(ctx, ctxutil.Call{AccountID: acct.ID, ActorID: u.ID, ActorKind: ctxutil.ActorUser})
	inv, err := svc.CreateInvite(ctx, email, identity.RoleDeveloper, nil)
	if err != nil {
		t.Fatalf("creating the invite: %v", err)
	}
	return inv
}

func TestAcceptanceRequiresBeingTheInvitee(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()
	inv := inviteFor(t, repo, svc, "invitee@x.com")

	// A third party, authenticated, holding the link.
	intruder, _, _ := svc.EnsureUser(ctx, ports.Principal{
		Subject: "s-intruder", Email: "intruder@x.com", EmailVerified: true})

	_, err := svc.AcceptInvite(ctx, inv.ID, intruder.ID)
	if err == nil {
		t.Fatal("holding the invite id must NOT be enough to join the account")
	}
	if errs.KindOf(err) != errs.KindPermission {
		t.Errorf("the refusal should be a permission one, got %v", errs.KindOf(err))
	}
	// The error must not become an oracle: whoever holds the link does not get
	// to learn who was invited. The translation params are checked too — a
	// localized sentence must not leak what the English one refuses to.
	if strings.Contains(err.Error(), "invitee@x.com") {
		t.Errorf("the message reveals the invite's email: %q", err)
	}
	if _, params := errs.CodeOf(err); len(params) != 0 {
		t.Errorf("the refusal carries params that could leak the invitee: %v", params)
	}
	if len(repo.accepted) != 0 {
		t.Errorf("no membership should have been created, got %v", repo.accepted)
	}
}

func TestAcceptanceRequiresAVerifiedEmail(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()
	inv := inviteFor(t, repo, svc, "invitee@x.com")

	// Same email, unverified: an issuer that does not confirm addresses would
	// let anyone claim somebody else's.
	almostThem, _, _ := svc.EnsureUser(ctx, ports.Principal{
		Subject: "s-invitee", Email: "invitee@x.com", EmailVerified: false})

	_, err := svc.AcceptInvite(ctx, inv.ID, almostThem.ID)
	if err == nil {
		t.Fatal("an unverified email does not prove identity")
	}
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("the refusal should be a precondition one, got %v", errs.KindOf(err))
	}
}

func TestTheInviteesOwnAcceptanceWorks(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()
	inv := inviteFor(t, repo, svc, "invitee@x.com")

	them, _, _ := svc.EnsureUser(ctx, ports.Principal{
		Subject: "s-invitee", Email: "invitee@x.com", EmailVerified: true})

	m, err := svc.AcceptInvite(ctx, inv.ID, them.ID)
	if err != nil {
		t.Fatalf("the invitee themselves should get in: %v", err)
	}
	if m == nil || m.UserID != them.ID {
		t.Fatalf("the membership should belong to the invitee, got %+v", m)
	}
	if len(repo.accepted) != 1 || repo.accepted[0] != inv.ID+"/"+them.ID {
		t.Errorf("acceptance recorded wrongly: %v", repo.accepted)
	}
}

func TestAcceptanceWithoutASessionIsRefused(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	inv := inviteFor(t, repo, svc, "invitee@x.com")

	if _, err := svc.AcceptInvite(context.Background(), inv.ID, ""); err == nil {
		t.Fatal("the link alone, with no session, must not accept anything")
	}
}

func TestAcceptanceOfAnExpiredInviteIsRefused(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()
	inv := inviteFor(t, repo, svc, "invitee@x.com")
	them, _, _ := svc.EnsureUser(ctx, ports.Principal{
		Subject: "s-invitee", Email: "invitee@x.com", EmailVerified: true})

	// The clock moves to exactly the deadline — the boundary is closed at the top.
	expired := identity.NewService(repo, fixedClock{now.Add(identity.InviteTTL)})
	if _, err := expired.AcceptInvite(ctx, inv.ID, them.ID); err == nil {
		t.Fatal("an invite at the instant of expiry should not hold")
	}
}

// EnsureUser normalizes the email before writing, but the PORT does not promise
// that: whoever reads `users` reads whatever is in the column — an old row, an
// import, another write path. This test goes underneath EnsureUser on purpose,
// because it is the only way to reach the tolerant comparison. Without it,
// swapping EqualFold for `!=` would slip through, and the right invitee would be
// told "this invite is not yours" over a capital G.
func TestAcceptanceToleratesCaseAndSpaceInTheUserRow(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	inv := inviteFor(t, repo, svc, "invitee@x.com")

	unnormalized := &identity.User{
		ID: "usr-raw", Subject: "s-raw",
		Email: " Invitee@X.Com ", EmailVerified: true,
	}
	repo.byID[unnormalized.ID] = unnormalized

	if _, err := svc.AcceptInvite(context.Background(), inv.ID, unnormalized.ID); err != nil {
		t.Fatalf("the same email in a different case should be accepted: %v", err)
	}
}
