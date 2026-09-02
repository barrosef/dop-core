package identity

import "context"

// Repository is the identity domain's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
type Repository interface {
	// Users
	UserBySubject(ctx context.Context, subject string) (*User, error)
	UserByID(ctx context.Context, id string) (*User, error)
	UpsertUser(ctx context.Context, u *User) (*User, error)

	// Accounts and memberships
	AccountByID(ctx context.Context, id string) (*Account, error)
	AccountByHandle(ctx context.Context, handle string) (*Account, error)
	CreateAccountWithOwner(ctx context.Context, a *Account, ownerUserID string) (*Account, error)
	AccountsOfUser(ctx context.Context, userID string) ([]Account, []Membership, error)

	MembershipsOfAccount(ctx context.Context, accountID string) ([]Membership, error)
	MembershipOf(ctx context.Context, userID, accountID string) (*Membership, error)
	UpdateMembershipRole(ctx context.Context, membershipID string, role Role) (*Membership, error)

	// Invites
	CreateInvite(ctx context.Context, inv *Invite) (*Invite, error)
	// InviteByID looks the row up by its id — which grants NOTHING on its own.
	//
	// It replaced the lookup by token hash. The token was a BEARER credential:
	// whoever held the value got in, and that is why it could not appear in an
	// event, a log or a projection. With acceptance requiring the session's
	// VERIFIED email to match the invite's, the id becomes sufficient as an
	// address and insufficient as a permission — which is the property that
	// lets it travel in the link, in the event and in the timeline without
	// risk.
	InviteByID(ctx context.Context, id string) (*Invite, error)
	AcceptInvite(ctx context.Context, inviteID, userID string) (*Membership, error)
	RevokeInvite(ctx context.Context, accountID, inviteID string) (*Invite, error)
	// InvitesOfAccount lists what was sent, so that "did I invite them?" is a
	// screen and not a support ticket. Revoked and accepted ones come too — the
	// list is a history, and a list that only shows the pending ones cannot
	// answer "what happened to the invite I sent yesterday?".
	InvitesOfAccount(ctx context.Context, accountID string) ([]Invite, error)
}
