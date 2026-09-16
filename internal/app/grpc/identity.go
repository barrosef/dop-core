// Package grpc translates between the contract (.proto) and the domain.
//
// This layer is THIN on purpose: it converts types, calls the service, converts
// back. No business rule lives here — if an `if` of business rule shows up in
// this package, it is in the wrong place.
package grpc

import (
	"context"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type IdentityServer struct {
	dopv1.UnimplementedIdentityServiceServer
	svc *identity.Service
}

func NewIdentityServer(svc *identity.Service) *IdentityServer {
	return &IdentityServer{svc: svc}
}

// EnsureUser is the bootstrap: it runs BEFORE the person has a user, so no actor
// can authorize it. What authorizes it is the token, and the token is also the
// only acceptable source for who the person is — the request's fields describe
// an identity the caller merely asserts (ADR-0029).
//
// The fields are IGNORED, not validated: an older client that still sends them
// is served the token's values and never told its body went nowhere. That is on
// purpose — there is nothing for it to do differently, and refusing the call
// would break a client that is asking for the right thing in an outdated way.
// They stay in the proto because removing a field is a wire-compatibility break
// for every client at once; they come out when the last one has stopped sending
// them.
func (s *IdentityServer) EnsureUser(ctx context.Context, _ *dopv1.EnsureUserRequest) (*dopv1.User, error) {
	call, _ := ctxutil.From(ctx)
	if call.Verified == nil {
		return nil, errs.New(errs.KindUnauthorized,
			"EnsureUser needs the person's token: a request body cannot say who they are")
	}
	u, _, err := s.svc.EnsureUser(ctx, ports.Principal{
		Subject:       call.Verified.Subject,
		Email:         call.Verified.Email,
		EmailVerified: call.Verified.EmailVerified,
		Name:          call.Verified.Name,
		AvatarURL:     call.Verified.AvatarURL,
		Providers:     call.Verified.Providers,
	})
	if err != nil {
		return nil, err
	}
	return userToProto(u), nil
}

func (s *IdentityServer) GetUser(ctx context.Context, req *dopv1.GetUserRequest) (*dopv1.User, error) {
	u, err := s.svc.GetUser(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return userToProto(u), nil
}

func (s *IdentityServer) ListAccounts(ctx context.Context, req *dopv1.ListAccountsRequest) (*dopv1.ListAccountsResponse, error) {
	userID := req.GetUser().GetId()
	if userID == "" {
		if call, ok := ctxutil.From(ctx); ok {
			userID = call.ActorID
		}
	}
	accounts, memberships, err := s.svc.ListAccounts(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Joined by account id rather than by position. The adapter happens to build
	// both slices from the same rows, but the PORT does not promise that, and a
	// silent misalignment would show one account carrying another's role — an
	// authorization answer, wrong, with nothing to indicate it.
	roleOf := make(map[string]identity.Role, len(memberships))
	for i := range memberships {
		roleOf[memberships[i].AccountID] = memberships[i].Role
	}

	out := make([]*dopv1.Account, 0, len(accounts))
	items := make([]*dopv1.AccountMembership, 0, len(accounts))
	for i := range accounts {
		a := accountToProto(&accounts[i])
		out = append(out, a)
		items = append(items, &dopv1.AccountMembership{
			Account: a,
			// A membership with no role is not a role of "none": it is this
			// account having no membership for this user, which cannot happen —
			// the query joins ON the membership. ROLE_UNSPECIFIED says "the core
			// did not answer" and nothing else, which is what a caller must be
			// able to distinguish from "viewer".
			Role: roleToProto(roleOf[accounts[i].ID]),
		})
	}
	// `accounts` stays filled through the transition: it is deprecated, not gone,
	// and a reader still on it must not silently start seeing an empty list.
	return &dopv1.ListAccountsResponse{Accounts: out, Items: items}, nil
}

func (s *IdentityServer) GetAccount(ctx context.Context, req *dopv1.GetAccountRequest) (*dopv1.Account, error) {
	a, err := s.svc.GetAccount(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return accountToProto(a), nil
}

func (s *IdentityServer) CreateAccount(ctx context.Context, req *dopv1.CreateAccountRequest) (*dopv1.Account, error) {
	a, err := s.svc.CreateOrganization(ctx, req.GetHandle(), req.GetDisplayName(), req.GetLegalId())
	if err != nil {
		return nil, err
	}
	return accountToProto(a), nil
}

// SendEmailVerification is the one RPC here that takes an address from the
// request body and acts on it. Everywhere else in this file the identity comes
// from the verified token (D-10) — here it cannot, because the person HAS NO
// USER yet: EnsureUser refuses their unverified credential before creating
// anything, which is the very thing this message exists to undo.
//
// What stands in for that: only a signed caller reaches the core at all
// (ADR-0029), and the domain caps sends per address.
func (s *IdentityServer) SendEmailVerification(ctx context.Context, req *dopv1.SendEmailVerificationRequest) (*dopv1.SendEmailVerificationResponse, error) {
	if err := s.svc.SendEmailVerification(ctx, req.GetEmail(), req.GetSubject(),
		req.GetLink(), req.GetDisplayName()); err != nil {
		return nil, err
	}
	return &dopv1.SendEmailVerificationResponse{}, nil
}

func (s *IdentityServer) ListMemberships(ctx context.Context, _ *dopv1.ListMembershipsRequest) (*dopv1.ListMembershipsResponse, error) {
	members, err := s.svc.ListMemberships(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Membership, 0, len(members))
	for i := range members {
		out = append(out, membershipToProto(&members[i]))
	}
	return &dopv1.ListMembershipsResponse{Memberships: out}, nil
}

func (s *IdentityServer) CreateInvite(ctx context.Context, req *dopv1.CreateInviteRequest) (*dopv1.Invite, error) {
	grants := make([]identity.GrantSpec, 0, len(req.GetGrants()))
	for _, g := range req.GetGrants() {
		grants = append(grants, identity.GrantSpec{
			ResourceID: g.GetResource().GetId(),
			Level:      g.GetLevel(),
		})
	}
	inv, err := s.svc.CreateInvite(ctx, req.GetEmail(), roleFromProto(req.GetRole()), grants)
	if err != nil {
		return nil, err
	}
	// There is no token to return: the invite has no secret. The email (P-11)
	// carries only the id, and acceptance checks who is logged in.
	return inviteToProto(inv), nil
}

func (s *IdentityServer) AcceptInvite(ctx context.Context, req *dopv1.AcceptInviteRequest) (*dopv1.Membership, error) {
	call, _ := ctxutil.From(ctx)
	m, err := s.svc.AcceptInvite(ctx, req.GetInviteId(), call.ActorID)
	if err != nil {
		return nil, err
	}
	return membershipToProto(m), nil
}

func (s *IdentityServer) RevokeInvite(ctx context.Context, req *dopv1.RevokeInviteRequest) (*dopv1.Invite, error) {
	inv, err := s.svc.RevokeInvite(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return inviteToProto(inv), nil
}

func (s *IdentityServer) ListInvites(ctx context.Context, _ *dopv1.ListInvitesRequest) (*dopv1.ListInvitesResponse, error) {
	invites, err := s.svc.ListInvites(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Invite, 0, len(invites))
	for i := range invites {
		out = append(out, inviteToProto(&invites[i]))
	}
	return &dopv1.ListInvitesResponse{Invites: out}, nil
}

func (s *IdentityServer) GetInvite(ctx context.Context, req *dopv1.GetInviteRequest) (*dopv1.InvitePreview, error) {
	p, err := s.svc.GetInvite(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	status := map[identity.InviteStatus]dopv1.Invite_Status{
		identity.InvitePending:  dopv1.Invite_STATUS_PENDING,
		identity.InviteAccepted: dopv1.Invite_STATUS_ACCEPTED,
		identity.InviteExpired:  dopv1.Invite_STATUS_EXPIRED,
		identity.InviteRevoked:  dopv1.Invite_STATUS_REVOKED,
	}[p.Status]
	return &dopv1.InvitePreview{
		Id:          p.ID,
		AccountName: p.AccountName,
		Role:        roleToProto(p.Role),
		Status:      status,
		ExpiresAt:   timestamppb.New(p.ExpiresAt),
		Usable:      p.Usable,
	}, nil
}

func (s *IdentityServer) UpdateMembership(ctx context.Context, req *dopv1.UpdateMembershipRequest) (*dopv1.Membership, error) {
	m, err := s.svc.UpdateMembershipRole(ctx, req.GetMembershipId(), roleFromProto(req.GetRole()))
	if err != nil {
		return nil, err
	}
	return membershipToProto(m), nil
}

func (s *IdentityServer) RemoveMembership(ctx context.Context, req *dopv1.RemoveMembershipRequest) (*dopv1.RemoveMembershipResponse, error) {
	if err := s.svc.RemoveMembership(ctx, req.GetMembershipId()); err != nil {
		return nil, err
	}
	return &dopv1.RemoveMembershipResponse{Removed: true}, nil
}

// ── conversions ──────────────────────────────────────────────────────────────

func userToProto(u *identity.User) *dopv1.User {
	if u == nil {
		return nil
	}
	return &dopv1.User{
		Id: u.ID, Email: u.Email, Name: u.Name, AvatarUrl: u.AvatarURL,
		Providers: u.Providers,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(u.CreatedAt),
			UpdatedAt: timestamppb.New(u.UpdatedAt),
		},
	}
}

func accountToProto(a *identity.Account) *dopv1.Account {
	if a == nil {
		return nil
	}
	kind := dopv1.Account_KIND_PERSONAL
	if a.Kind == identity.AccountOrganization {
		kind = dopv1.Account_KIND_ORGANIZATION
	}
	return &dopv1.Account{
		Id: a.ID, Kind: kind, Handle: a.Handle, DisplayName: a.DisplayName,
		LegalId: a.LegalID, LegalName: a.LegalName, VerifiedDomain: a.VerifiedDomain,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(a.CreatedAt),
			UpdatedAt: timestamppb.New(a.UpdatedAt),
		},
	}
}

func membershipToProto(m *identity.Membership) *dopv1.Membership {
	if m == nil {
		return nil
	}
	return &dopv1.Membership{
		Id:      m.ID,
		User:    &dopv1.UserRef{Id: m.UserID},
		Account: &dopv1.AccountRef{Id: m.AccountID},
		Role:    roleToProto(m.Role),
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(m.CreatedAt),
			UpdatedAt: timestamppb.New(m.UpdatedAt),
		},
	}
}

func inviteToProto(i *identity.Invite) *dopv1.Invite {
	if i == nil {
		return nil
	}
	grants := make([]*dopv1.ResourceGrantSpec, 0, len(i.Grants))
	for _, g := range i.Grants {
		grants = append(grants, &dopv1.ResourceGrantSpec{
			Resource: &dopv1.ResourceRef{Id: g.ResourceID}, Level: g.Level,
		})
	}
	status := map[identity.InviteStatus]dopv1.Invite_Status{
		identity.InvitePending:  dopv1.Invite_STATUS_PENDING,
		identity.InviteAccepted: dopv1.Invite_STATUS_ACCEPTED,
		identity.InviteExpired:  dopv1.Invite_STATUS_EXPIRED,
		identity.InviteRevoked:  dopv1.Invite_STATUS_REVOKED,
	}[i.Status]
	return &dopv1.Invite{
		Id: i.ID, Account: &dopv1.AccountRef{Id: i.AccountID}, Email: i.Email,
		Role: roleToProto(i.Role), Grants: grants, Status: status,
		ExpiresAt: timestamppb.New(i.ExpiresAt),
		Audit:     &dopv1.AuditStamp{CreatedAt: timestamppb.New(i.CreatedAt)},
	}
}

func roleToProto(r identity.Role) dopv1.Role {
	switch r {
	case identity.RoleOwner:
		return dopv1.Role_ROLE_OWNER
	case identity.RoleAdmin:
		return dopv1.Role_ROLE_ADMIN
	case identity.RoleDeveloper:
		return dopv1.Role_ROLE_DEVELOPER
	case identity.RoleViewer:
		return dopv1.Role_ROLE_VIEWER
	}
	return dopv1.Role_ROLE_UNSPECIFIED
}

func roleFromProto(r dopv1.Role) identity.Role {
	switch r {
	case dopv1.Role_ROLE_OWNER:
		return identity.RoleOwner
	case dopv1.Role_ROLE_ADMIN:
		return identity.RoleAdmin
	case dopv1.Role_ROLE_DEVELOPER:
		return identity.RoleDeveloper
	case dopv1.Role_ROLE_VIEWER:
		return identity.RoleViewer
	}
	return ""
}
