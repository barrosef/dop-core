package identity_test

import (
	"context"
	"fmt"
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

// strings.ToLower, not strings.EqualFold, because it is what the SQL adapter
// actually does (`lower(email) = lower($1)`) — measured against a live
// database, Go's Unicode-aware EqualFold and glibc's locale-aware lower()
// disagree on at least one real address (Turkish dotted İ folds to "i̇stanbul"
// under Go, "istanbul" under glibc's en_US.utf8). Exact parity with the
// database's lower() is NOT achievable here without reimplementing a locale,
// so this double does not attempt it: it matches the ASCII-common case and
// stays silent about the rest. A domain test must never assert
// account-linking behaviour (this is Task 4's territory) on a non-ASCII
// address — that assertion belongs in test/integration, against the real
// adapter, or a passing domain suite would be hiding a broken product.
func (f *fakeRepo) UserByVerifiedEmail(_ context.Context, email string) (*identity.User, error) {
	for _, u := range f.users {
		if u.EmailVerified && strings.ToLower(u.Email) == strings.ToLower(email) {
			return u, nil
		}
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
func (f *fakeRepo) SetDefaultRevocationPolicy(_ context.Context, accountID, policy string) error {
	a, ok := f.accounts[accountID]
	if !ok {
		return errs.NotFound("account")
	}
	// accounts and byHandle share the same pointer (see CreateAccountWithOwner),
	// so mutating through either map is visible from both.
	a.DefaultRevocationPolicy = policy
	return nil
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
func (f *fakeRepo) MembershipByID(_ context.Context, id string) (*identity.Membership, error) {
	for i := range f.members {
		if f.members[i].ID == id {
			return &f.members[i], nil
		}
	}
	return nil, nil
}
func (f *fakeRepo) RemoveMembership(_ context.Context, id string) error {
	for i := range f.members {
		if f.members[i].ID == id {
			f.members = append(f.members[:i], f.members[i+1:]...)
			return nil
		}
	}
	return errs.NotFound("membership")
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
func (f *fakeRepo) InvitesOfAccount(_ context.Context, accountID string) ([]identity.Invite, error) {
	out := []identity.Invite{}
	for _, inv := range f.invites {
		if inv.AccountID == accountID {
			out = append(out, *inv)
		}
	}
	return out, nil
}

func (f *fakeRepo) RevokeInvite(context.Context, string, string) (*identity.Invite, error) {
	return nil, nil
}

// ── service tests ───────────────────────────────────────────────────────────

func TestEnsureUserCreatesThePersonalAccount(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})

	u, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "sub-1", Email: "dev@dop.local", Name: "Dev", EmailVerified: true, Providers: []string{"password"},
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
	p := ports.Principal{Subject: "sub-1", Email: "dev@dop.local", EmailVerified: true, Providers: []string{"password"}}

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

	svc.EnsureUser(ctx, ports.Principal{Subject: "sub-1", Email: "dev@dop.local", EmailVerified: true, Providers: []string{"password"}})
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

func TestASecondSubjectOnAVerifiedEmailIsRefusedByName(t *testing.T) {
	// Reaching here means the provider stopped linking accounts that share an
	// e-mail. The e-mail is unique in the schema, so the insert would fail on the
	// index and the person would read "something went wrong". The cause is a
	// configuration, and the error has to say so.
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()

	if _, _, err := svc.EnsureUser(ctx, ports.Principal{
		Subject: "sub-google", Email: "ana@example.com", EmailVerified: true,
		Providers: []string{"google"},
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err := svc.EnsureUser(ctx, ports.Principal{
		Subject: "sub-github", Email: "ana@example.com", EmailVerified: true,
		Providers: []string{"github"},
	})
	if errs.KindOf(err) != errs.KindConflict {
		t.Fatalf("expected a conflict naming the configuration, got %v", err)
	}
	if !strings.Contains(err.Error(), "linking") {
		t.Fatalf("the message does not name the cause: %v", err)
	}
}

func TestTheSameSubjectComingBackIsNotAConflict(t *testing.T) {
	// The guard must not fire on the ordinary case: the same person, same
	// subject, signing in again on an e-mail that is already theirs.
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	ctx := context.Background()
	p := ports.Principal{Subject: "sub-1", Email: "ana@example.com",
		EmailVerified: true, Providers: []string{"google"}}

	u1, _, _ := svc.EnsureUser(ctx, p)
	u2, _, err := svc.EnsureUser(ctx, p)
	if err != nil {
		t.Fatalf("signing in twice is not a conflict: %v", err)
	}
	if u1.ID != u2.ID {
		t.Fatal("the same subject produced two users")
	}
}

func TestAPasswordCredentialNeedsAVerifiedEmail(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	_, _, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "sub-1", Email: "ana@example.com", EmailVerified: false,
		Providers: []string{"password"},
	})
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("without verification anybody signs up with anybody's address: %v", err)
	}
}

func TestASocialCredentialEntersWithAnUnverifiedEmail(t *testing.T) {
	// GitHub frequently hands over an unverified e-mail. Refusing it here would
	// lock out the provider this platform's users are most likely to have — and
	// the e-mail is not the proof there, the provider's authentication is.
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	u, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "sub-2", Email: "bruno@example.com", EmailVerified: false,
		Providers: []string{"github"},
	})
	if err != nil {
		t.Fatalf("a social credential does not need the e-mail verified: %v", err)
	}
	if u == nil || acct == nil {
		t.Fatal("the user and the personal account should exist")
	}
}

func TestAPasswordLinkedToASocialProviderEnters(t *testing.T) {
	// Once a social provider is on the same credential, the password is no
	// longer the only thing vouching for the person.
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	_, _, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "sub-3", Email: "carla@example.com", EmailVerified: false,
		Providers: []string{"password", "google"},
	})
	if err != nil {
		t.Fatalf("password plus a social provider is not a password-only credential: %v", err)
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

// ── the second factor's gate (ADR-0027 §5) ──────────────────────────────────

type refusingGate struct{ calls int }

func (g *refusingGate) RequireStepUp(context.Context) error {
	g.calls++
	return errs.Permission("this operation requires the second factor")
}

func TestTheSensitiveOperationsAskTheStepUpGate(t *testing.T) {
	// Inviting, changing a role and revoking hand out — or take away — a key to
	// the account. They are the three the ADR lists on the identity side, and
	// the test exists so a fourth one added tomorrow is a deliberate decision,
	// not an omission.
	for _, c := range []struct {
		name string
		call func(t *testing.T, svc *identity.Service, ctx context.Context, acct *identity.Account) error
	}{
		{"invite", func(_ *testing.T, svc *identity.Service, ctx context.Context, _ *identity.Account) error {
			_, err := svc.CreateInvite(ctx, "new@dop.local", identity.RoleDeveloper, nil)
			return err
		}},
		{"revoke", func(_ *testing.T, svc *identity.Service, ctx context.Context, _ *identity.Account) error {
			_, err := svc.RevokeInvite(ctx, "inv-1")
			return err
		}},
		{"change role", func(_ *testing.T, svc *identity.Service, ctx context.Context, _ *identity.Account) error {
			_, err := svc.UpdateMembershipRole(ctx, "mem-1", identity.RoleAdmin)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo := newFakeRepo()
			gate := &refusingGate{}
			svc := identity.NewService(repo, fixedClock{now}).WithStepUp(gate)
			u, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
				Subject: "s1", Email: "owner@x.com", EmailVerified: true})
			if err != nil {
				t.Fatal(err)
			}
			ctx := ctxutil.Into(context.Background(), ctxutil.Call{
				ActorID: u.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID, SessionID: "sess-1"})

			err = c.call(t, svc, ctx, acct)
			if errs.KindOf(err) != errs.KindPermission {
				t.Fatalf("with the gate refusing it gave %v (%s)", err, errs.KindOf(err))
			}
			if gate.calls == 0 {
				t.Error("the operation did not ask the gate")
			}
		})
	}
}

func TestWithNoGateWiredTheDomainWorksOnItsOwn(t *testing.T) {
	// A service assembled WITHOUT a gate has no second factor, and that is
	// explicit: it is what lets this domain be tested without the second
	// factor's whole machinery. The composition root always wires one.
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	u, acct, _ := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s1", Email: "owner@x.com", EmailVerified: true})
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: u.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID})

	if _, err := svc.CreateInvite(ctx, "new@dop.local", identity.RoleDeveloper, nil); err != nil {
		t.Fatalf("with no gate it refused: %v", err)
	}
}

// ── the invite's path (P-32) ────────────────────────────────────────────────

func TestTheInvitePreviewDoesNotRevealTheInvitee(t *testing.T) {
	// Whoever finds the link must not learn an address from it — that would turn
	// it back into the oracle taking the token out was meant to end (ADR-0026).
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	owner, acct, _ := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s-owner", Email: "owner@acme.test", EmailVerified: true})
	ownerCtx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: owner.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID})
	inv, err := svc.CreateInvite(ownerCtx, "invitee@acme.test", identity.RoleDeveloper, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A stranger, with a session and no membership in that account.
	other, _, _ := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s-other", Email: "other@x.test", EmailVerified: true})
	otherCtx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: other.ID, ActorKind: ctxutil.ActorUser})

	p, err := svc.GetInvite(otherCtx, inv.ID)
	if err != nil {
		t.Fatalf("the preview requires no active account: %v", err)
	}
	if p.AccountName == "" || !p.Usable {
		t.Errorf("the preview says nothing useful: %+v", p)
	}
	// The struct has no field for it, and the test is what keeps it that way:
	// a field added tomorrow "just for convenience" fails here.
	if strings.Contains(fmt.Sprintf("%+v", *p), "invitee@acme.test") {
		t.Error("the preview leaked the invitee's e-mail")
	}
}

func TestReadingAnInviteRequiresASession(t *testing.T) {
	// Without it, an id found in a log would tell a stranger that an account
	// named X invited somebody as an admin.
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	owner, acct, _ := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s-owner", Email: "owner@acme.test", EmailVerified: true})
	ownerCtx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: owner.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID})
	inv, _ := svc.CreateInvite(ownerCtx, "invitee@acme.test", identity.RoleDeveloper, nil)

	_, err := svc.GetInvite(context.Background(), inv.ID)
	if errs.KindOf(err) != errs.KindUnauthorized {
		t.Fatalf("with no session it gave %v (%s)", err, errs.KindOf(err))
	}
}

func TestOnlyWhoManagesMembersSeesTheInvites(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	owner, acct, _ := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s-owner", Email: "owner@acme.test", EmailVerified: true})
	ownerCtx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: owner.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID})
	if _, err := svc.CreateInvite(ownerCtx, "invitee@acme.test", identity.RoleDeveloper, nil); err != nil {
		t.Fatal(err)
	}

	list, err := svc.ListInvites(ownerCtx)
	if err != nil || len(list) != 1 {
		t.Fatalf("the owner saw %d invites (err=%v)", len(list), err)
	}

	// A developer does not: the list carries the addresses of people who were
	// invited, and that is not public inside the account.
	dev, _, _ := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s-dev", Email: "dev@acme.test", EmailVerified: true})
	repo.members = append(repo.members, identity.Membership{
		ID: "mem-dev", UserID: dev.ID, AccountID: acct.ID, Role: identity.RoleDeveloper})
	devCtx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: dev.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID})

	if _, err := svc.ListInvites(devCtx); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("a developer saw the invites: %v", err)
	}
}

// ── the last owner ──────────────────────────────────────────────────────────

func TestTheLastOwnerCannotBeDemoted(t *testing.T) {
	// An account with no owner cannot be recovered from inside: nobody left can
	// promote anybody. With a select on the members screen it would be one
	// click away, so the refusal lives in the domain and not on the screen.
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	u, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s1", Email: "owner@x.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: u.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID, SessionID: "sess-1"})
	mems, _ := repo.MembershipsOfAccount(ctx, acct.ID)
	only := mems[0].ID

	_, err = svc.UpdateMembershipRole(ctx, only, identity.RoleAdmin)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("demoting the only owner gave %v (%s)", err, errs.KindOf(err))
	}
	if k, _ := errs.CodeOf(err); k != identity.KeyLastOwner {
		t.Errorf("key = %q", k)
	}

	// With a second owner it goes through — what is refused is emptying the
	// role, not editing it.
	repo.members = append(repo.members, identity.Membership{
		ID: "mem-second", UserID: "usr-2", AccountID: acct.ID, Role: identity.RoleOwner})
	if _, err := svc.UpdateMembershipRole(ctx, only, identity.RoleAdmin); err != nil {
		t.Fatalf("with two owners it still refused: %v", err)
	}
}

// ── taking somebody out of the account (US-5.3) ─────────────────────────────

type fakeGrants struct {
	swept []string // "accountID/userID", in call order
	fail  error
}

func (g *fakeGrants) RevokeAllOfMember(_ context.Context, accountID, userID string) error {
	if g.fail != nil {
		return g.fail
	}
	g.swept = append(g.swept, accountID+"/"+userID)
	return nil
}

// orgFixture builds an organization with an owner and a second member, which is
// the only shape where a removal is legal at all.
func orgFixture(t *testing.T) (*identity.Service, *fakeRepo, *fakeGrants, context.Context, string) {
	t.Helper()
	repo := newFakeRepo()
	grants := &fakeGrants{}
	svc := identity.NewService(repo, fixedClock{now}).WithGrants(grants)
	u, _, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s1", Email: "owner@x.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	org, err := svc.CreateOrganization(
		ctxutil.Into(context.Background(), ctxutil.Call{ActorID: u.ID, ActorKind: ctxutil.ActorUser}),
		"acme", "Acme", "00.000.000/0001-00")
	if err != nil {
		t.Fatal(err)
	}
	repo.members = append(repo.members, identity.Membership{
		ID: "mem-dev", UserID: "usr-dev", AccountID: org.ID, Role: identity.RoleDeveloper})
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: u.ID, ActorKind: ctxutil.ActorUser, AccountID: org.ID, SessionID: "sess-1"})
	return svc, repo, grants, ctx, org.ID
}

func TestRemovingAMemberSweepsTheirGrantsFirst(t *testing.T) {
	// A grant outliving the membership is access with no membership to justify
	// it. And the order matters: if the sweep fails, nothing was removed.
	svc, repo, grants, ctx, orgID := orgFixture(t)

	if err := svc.RemoveMembership(ctx, "mem-dev"); err != nil {
		t.Fatal(err)
	}
	if len(grants.swept) != 1 || grants.swept[0] != orgID+"/usr-dev" {
		t.Errorf("the grants were not swept: %v", grants.swept)
	}
	mems, _ := repo.MembershipsOfAccount(ctx, orgID)
	for _, m := range mems {
		if m.ID == "mem-dev" {
			t.Fatal("the membership survived the removal")
		}
	}
}

func TestAFailedSweepRemovesNothing(t *testing.T) {
	svc, repo, grants, ctx, orgID := orgFixture(t)
	grants.fail = errs.New(errs.KindInternal, "the vault is down")

	if err := svc.RemoveMembership(ctx, "mem-dev"); err == nil {
		t.Fatal("it removed the member with the sweep failing")
	}
	mems, _ := repo.MembershipsOfAccount(ctx, orgID)
	found := false
	for _, m := range mems {
		if m.ID == "mem-dev" {
			found = true
		}
	}
	if !found {
		t.Error("the membership went away even though the sweep failed")
	}
}

func TestTheLastOwnerCannotBeRemoved(t *testing.T) {
	svc, repo, _, ctx, orgID := orgFixture(t)
	var ownerMembership string
	mems, _ := repo.MembershipsOfAccount(ctx, orgID)
	for _, m := range mems {
		if m.Role == identity.RoleOwner {
			ownerMembership = m.ID
		}
	}

	err := svc.RemoveMembership(ctx, ownerMembership)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("removing the only owner gave %v (%s)", err, errs.KindOf(err))
	}
	if k, _ := errs.CodeOf(err); k != identity.KeyLastOwner {
		t.Errorf("key = %q", k)
	}
}

func TestThePersonalAccountsMembershipIsNotRemovable(t *testing.T) {
	// Removing it would be deleting the user — another operation, with another
	// meaning and another set of rules (P-3).
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now}).WithGrants(&fakeGrants{})
	u, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s1", Email: "dev@x.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: u.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID, SessionID: "sess-1"})
	mems, _ := repo.MembershipsOfAccount(ctx, acct.ID)

	err = svc.RemoveMembership(ctx, mems[0].ID)
	if k, _ := errs.CodeOf(err); k != identity.KeyPersonalNotRemovable {
		t.Fatalf("it gave %v (key %q)", err, k)
	}
}

func TestAMembershipOfAnotherAccountIsNotFound(t *testing.T) {
	// Telling apart "it is not yours" from "it does not exist" would say that
	// the row is real somewhere else.
	svc, repo, _, ctx, _ := orgFixture(t)
	repo.members = append(repo.members, identity.Membership{
		ID: "mem-elsewhere", UserID: "usr-x", AccountID: "acct-other", Role: identity.RoleDeveloper})

	err := svc.RemoveMembership(ctx, "mem-elsewhere")
	if errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("it gave %v (%s)", err, errs.KindOf(err))
	}
}

func TestADeveloperCannotRemoveAMember(t *testing.T) {
	svc, _, _, _, orgID := orgFixture(t)
	asDev := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: "usr-dev", ActorKind: ctxutil.ActorUser, AccountID: orgID, SessionID: "sess-2"})

	if err := svc.RemoveMembership(asDev, "mem-dev"); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("a developer removed a member: %v", err)
	}
}

func TestRemovingAMemberAsksTheStepUpGate(t *testing.T) {
	// It does not go into the table of the other three because the refusals
	// that come BEFORE the gate — not an admin, the last owner, a personal
	// account — are legitimately answered without a second factor. Only a
	// removal that would actually happen is worth challenging.
	repo := newFakeRepo()
	gate := &refusingGate{}
	grants := &fakeGrants{}
	svc := identity.NewService(repo, fixedClock{now}).WithStepUp(gate).WithGrants(grants)
	u, _, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s1", Email: "owner@x.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	org, err := svc.CreateOrganization(
		ctxutil.Into(context.Background(), ctxutil.Call{ActorID: u.ID, ActorKind: ctxutil.ActorUser}),
		"acme", "Acme", "00.000.000/0001-00")
	if err != nil {
		t.Fatal(err)
	}
	repo.members = append(repo.members, identity.Membership{
		ID: "mem-dev", UserID: "usr-dev", AccountID: org.ID, Role: identity.RoleDeveloper})
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: u.ID, ActorKind: ctxutil.ActorUser, AccountID: org.ID, SessionID: "sess-1"})

	if err := svc.RemoveMembership(ctx, "mem-dev"); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("with the gate refusing it gave %v", err)
	}
	if gate.calls == 0 {
		t.Error("the gate was not asked")
	}
	if len(grants.swept) != 0 {
		t.Error("it swept the grants before the gate refused")
	}
}

// ── the account's default revocation policy (flow sharing spec §3.2) ───────

func TestOnlyOwnerOrAdminChangesTheDefaultRevocationPolicy(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, fixedClock{now})
	owner, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "s-owner", Email: "owner@x.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	repo.members = append(repo.members, identity.Membership{
		ID: "mem-dev", UserID: "usr-dev", AccountID: acct.ID, Role: identity.RoleDeveloper})

	devCtx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: "usr-dev", ActorKind: ctxutil.ActorUser, AccountID: acct.ID})
	if err := svc.SetDefaultRevocationPolicy(devCtx, "terminate"); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("a developer must not change an account setting: %v", err)
	}

	ownerCtx := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: owner.ID, ActorKind: ctxutil.ActorUser, AccountID: acct.ID})
	if err := svc.SetDefaultRevocationPolicy(ownerCtx, "terminate"); err != nil {
		t.Fatalf("the owner has to be able to: %v", err)
	}
	got, err := repo.AccountByID(context.Background(), acct.ID)
	if err != nil || got.DefaultRevocationPolicy != "terminate" {
		t.Fatalf("the default was not stored: %+v (err=%v)", got, err)
	}

	if err := svc.SetDefaultRevocationPolicy(ownerCtx, "cascade"); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a value outside the vocabulary has to be refused: %v", err)
	}
}
