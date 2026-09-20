package grpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

func TestEnsureUserWithoutAVerifiedTokenIsRefused(t *testing.T) {
	// The service is nil on purpose: the refusal has to happen BEFORE anything
	// is created, so reaching the service at all would panic and fail this test.
	// That is a stronger assertion than any double could make.
	srv := NewIdentityServer(nil)

	_, err := srv.EnsureUser(context.Background(), &dopv1.EnsureUserRequest{
		Subject: "sub-1", Email: "ana@example.com", EmailVerified: true,
	})
	if errs.KindOf(err) != errs.KindUnauthorized {
		t.Fatalf("without a token nothing may be created: %v", err)
	}
}

func TestListAccountsSaysWhichRoleInWhichAccount(t *testing.T) {
	// The same person is an owner in one account and a viewer in another. That
	// is the whole point of the role living on the membership: without it the
	// cockpit cannot tell the two apart until after switching into each one.
	repo := &stubIdentityRepo{
		users: map[string]*identity.User{},
		accounts: []identity.Account{
			{ID: "acct-own", Kind: identity.AccountPersonal, Handle: "ana"},
			{ID: "acct-view", Kind: identity.AccountOrganization, Handle: "acme"},
		},
		memberships: []identity.Membership{
			// DELIBERATELY in the opposite order to the accounts. The two slices
			// happen to be aligned in the Postgres adapter, and the port never
			// promised it — a positional join would pass every day until it
			// handed somebody an owner's screen.
			{UserID: "u-1", AccountID: "acct-view", Role: identity.RoleViewer},
			{UserID: "u-1", AccountID: "acct-own", Role: identity.RoleOwner},
		},
	}
	srv := NewIdentityServer(identity.NewService(repo, stubClock{}))

	got, err := srv.ListAccounts(context.Background(), &dopv1.ListAccountsRequest{
		User: &dopv1.UserRef{Id: "u-1"},
	})
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}

	want := map[string]dopv1.Role{
		"acct-own":  dopv1.Role_ROLE_OWNER,
		"acct-view": dopv1.Role_ROLE_VIEWER,
	}
	if len(got.GetItems()) != len(want) {
		t.Fatalf("expected %d accounts with a role, got %d", len(want), len(got.GetItems()))
	}
	for _, it := range got.GetItems() {
		id := it.GetAccount().GetId()
		if it.GetRole() != want[id] {
			t.Errorf("account %s came back as %s, expected %s", id, it.GetRole(), want[id])
		}
	}

	// The deprecated field keeps answering: a caller still reading it must not
	// silently start seeing an empty list in the middle of the transition.
	if len(got.GetAccounts()) != len(want) {
		t.Errorf("the deprecated `accounts` stopped being filled: %d", len(got.GetAccounts()))
	}
}

func TestEnsureUserIgnoresWhatTheBodyClaims(t *testing.T) {
	// The body says the e-mail is verified and the subject is somebody else's.
	// Both are text. Only the token's answer may decide.
	repo := &stubIdentityRepo{users: map[string]*identity.User{}}
	srv := NewIdentityServer(identity.NewService(repo, stubClock{}))
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		Verified: &ctxutil.VerifiedIdentity{
			Subject: "sub-real", Email: "ana@example.com", EmailVerified: true,
			Providers: []string{"google"},
		},
	})

	got, err := srv.EnsureUser(ctx, &dopv1.EnsureUserRequest{
		Subject: "sub-someone-else", Email: "victim@example.com",
		EmailVerified: true, Provider: "password",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if got.GetEmail() != "ana@example.com" {
		t.Fatalf("the body chose the identity: %q", got.GetEmail())
	}
}

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }

// stubIdentityRepo implements identity.Repository for the five methods
// EnsureUser touches. Every other method panics rather than returning a zero
// value: a double that answers questions nobody taught it makes the test that
// leans on it pass for the wrong reason.
type stubIdentityRepo struct {
	users       map[string]*identity.User
	accounts    []identity.Account
	memberships []identity.Membership
	n           int
}

func (s *stubIdentityRepo) UserBySubject(_ context.Context, sub string) (*identity.User, error) {
	if u, ok := s.users[sub]; ok {
		return u, nil
	}
	return nil, errs.NotFound("user")
}

func (s *stubIdentityRepo) UpsertUser(_ context.Context, u *identity.User) (*identity.User, error) {
	if u.ID == "" {
		s.n++
		u.ID = fmt.Sprintf("usr-%d", s.n)
	}
	cp := *u
	s.users[u.Subject] = &cp
	return &cp, nil
}

// It consults s.users like its siblings, instead of the hardcoded NotFound it
// used to answer. A hardcoded miss is the very thing this type's doc comment
// warns about: EnsureUser's collision guard asks this question, and a double
// that always says "nobody" makes the case it guards untestable while every
// test that passes through it goes green. The empty address matches nobody
// here for the same reason it cannot in the adapter — `lower(NULL) =
// lower(”)` is NULL, not true.
func (s *stubIdentityRepo) UserByVerifiedEmail(_ context.Context, email string) (*identity.User, error) {
	if email == "" {
		return nil, errs.NotFound("user")
	}
	for _, u := range s.users {
		if u.EmailVerified && strings.ToLower(u.Email) == strings.ToLower(email) {
			return u, nil
		}
	}
	return nil, errs.NotFound("user")
}

func (s *stubIdentityRepo) AccountsOfUser(context.Context, string) ([]identity.Account, []identity.Membership, error) {
	return s.accounts, s.memberships, nil
}

func (s *stubIdentityRepo) VerificationRequestsSince(context.Context, string, time.Time) (int, time.Time, error) {
	panic("VerificationRequestsSince: not taught to this double")
}

func (s *stubIdentityRepo) RecordVerificationRequest(context.Context, string, string) error {
	panic("RecordVerificationRequest: not taught to this double")
}

func (s *stubIdentityRepo) CreateAccountWithOwner(_ context.Context, a *identity.Account, ownerID string) (*identity.Account, error) {
	s.n++
	a.ID = fmt.Sprintf("acct-%d", s.n)
	s.accounts = append(s.accounts, *a)
	s.memberships = append(s.memberships, identity.Membership{
		UserID: ownerID, AccountID: a.ID, Role: identity.RoleOwner,
	})
	return a, nil
}

func (s *stubIdentityRepo) AccountByHandle(context.Context, string) (*identity.Account, error) {
	return nil, errs.NotFound("account")
}

func (s *stubIdentityRepo) UserByID(context.Context, string) (*identity.User, error) {
	panic("stubIdentityRepo: UserByID is not used by these tests")
}

func (s *stubIdentityRepo) AccountByID(context.Context, string) (*identity.Account, error) {
	panic("stubIdentityRepo: AccountByID is not used by these tests")
}

func (s *stubIdentityRepo) SetDefaultRevocationPolicy(context.Context, string, string) error {
	panic("stubIdentityRepo: SetDefaultRevocationPolicy is not used by these tests")
}

func (s *stubIdentityRepo) MembershipsOfAccount(context.Context, string) ([]identity.Membership, error) {
	panic("stubIdentityRepo: MembershipsOfAccount is not used by these tests")
}

func (s *stubIdentityRepo) MembershipOf(context.Context, string, string) (*identity.Membership, error) {
	panic("stubIdentityRepo: MembershipOf is not used by these tests")
}

func (s *stubIdentityRepo) UpdateMembershipRole(context.Context, string, identity.Role) (*identity.Membership, error) {
	panic("stubIdentityRepo: UpdateMembershipRole is not used by these tests")
}

func (s *stubIdentityRepo) MembershipByID(context.Context, string) (*identity.Membership, error) {
	panic("stubIdentityRepo: MembershipByID is not used by these tests")
}

func (s *stubIdentityRepo) RemoveMembership(context.Context, string) error {
	panic("stubIdentityRepo: RemoveMembership is not used by these tests")
}

func (s *stubIdentityRepo) CreateInvite(context.Context, *identity.Invite) (*identity.Invite, error) {
	panic("stubIdentityRepo: CreateInvite is not used by these tests")
}

func (s *stubIdentityRepo) InviteByID(context.Context, string) (*identity.Invite, error) {
	panic("stubIdentityRepo: InviteByID is not used by these tests")
}

func (s *stubIdentityRepo) AcceptInvite(context.Context, string, string) (*identity.Membership, error) {
	panic("stubIdentityRepo: AcceptInvite is not used by these tests")
}

func (s *stubIdentityRepo) RevokeInvite(context.Context, string, string) (*identity.Invite, error) {
	panic("stubIdentityRepo: RevokeInvite is not used by these tests")
}

func (s *stubIdentityRepo) InvitesOfAccount(context.Context, string) ([]identity.Invite, error) {
	panic("stubIdentityRepo: InvitesOfAccount is not used by these tests")
}

// The onboarding journey's writes: this stub serves the RPC translation tests
// above, none of which reach them, so they answer "unsupported" loudly rather
// than pretending.
func (s *stubIdentityRepo) UpdateProfile(context.Context, *identity.User, string) (*identity.User, error) {
	return nil, errs.Internal("not in this stub")
}
func (s *stubIdentityRepo) SetPhoneVerified(context.Context, string, string, time.Time, string) error {
	return errs.Internal("not in this stub")
}
func (s *stubIdentityRepo) SetOnboardingStep(context.Context, string, identity.Step, identity.StepStatus) (*identity.User, error) {
	return nil, errs.Internal("not in this stub")
}
func (s *stubIdentityRepo) SetOnboardedAt(context.Context, string, time.Time) (*identity.User, error) {
	return nil, errs.Internal("not in this stub")
}
func (s *stubIdentityRepo) UpdateAccountProfile(context.Context, string, string, string) (*identity.Account, error) {
	return nil, errs.Internal("not in this stub")
}
func (s *stubIdentityRepo) SetAccountPlan(context.Context, string, string) (*identity.Account, error) {
	return nil, errs.Internal("not in this stub")
}
