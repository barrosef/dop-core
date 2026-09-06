package grpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
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
	users    map[string]*identity.User
	accounts []identity.Account
	n        int
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

func (s *stubIdentityRepo) UserByVerifiedEmail(context.Context, string) (*identity.User, error) {
	return nil, errs.NotFound("user")
}

func (s *stubIdentityRepo) AccountsOfUser(context.Context, string) ([]identity.Account, []identity.Membership, error) {
	return s.accounts, nil, nil
}

func (s *stubIdentityRepo) CreateAccountWithOwner(_ context.Context, a *identity.Account, _ string) (*identity.Account, error) {
	s.n++
	a.ID = fmt.Sprintf("acct-%d", s.n)
	s.accounts = append(s.accounts, *a)
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
