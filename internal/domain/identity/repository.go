package identity

import (
	"context"
	"time"
)

// Repository is the identity domain's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
type Repository interface {
	// Users
	UserBySubject(ctx context.Context, subject string) (*User, error)
	UserByID(ctx context.Context, id string) (*User, error)
	UpsertUser(ctx context.Context, u *User) (*User, error)
	// UserByVerifiedEmail finds the user who PROVED this address. The predicate
	// is in the query and not in the caller because the caller that forgets it
	// hands one person's account to another: an unverified e-mail is a claim,
	// and two subjects agreeing on a claim are not the same person.
	UserByVerifiedEmail(ctx context.Context, email string) (*User, error)

	// The onboarding journey's writes (spec 2026-09-20 §4). personalAccountID
	// travels with the two phone operations because the events they emit must
	// land in the person's own box, and the adapter has no way to find it.
	UpdateProfile(ctx context.Context, u *User, personalAccountID string) (*User, error)
	// SetPhoneVerified writes only when users.phone == phone; otherwise it is a
	// no-op with no error — a factor on another number is not the contact one.
	SetPhoneVerified(ctx context.Context, userID, phone string, at time.Time, personalAccountID string) error
	SetOnboardingStep(ctx context.Context, userID string, step Step, status StepStatus) (*User, error)
	SetOnboardedAt(ctx context.Context, userID string, at time.Time) (*User, error)
	// UpdateAccountProfile: an empty handle or display name is untouched.
	UpdateAccountProfile(ctx context.Context, accountID, handle, displayName string) (*Account, error)
	SetAccountPlan(ctx context.Context, accountID, planKey string) (*Account, error)

	// Accounts and memberships
	AccountByID(ctx context.Context, id string) (*Account, error)
	AccountByHandle(ctx context.Context, handle string) (*Account, error)
	CreateAccountWithOwner(ctx context.Context, a *Account, ownerUserID string) (*Account, error)
	AccountsOfUser(ctx context.Context, userID string) ([]Account, []Membership, error)

	// VerificationRequestsSince answers the rate limit's two questions at once:
	// how many messages went to this address inside the window, and when the
	// last one left. A zero time means none.
	VerificationRequestsSince(ctx context.Context, email string, since time.Time) (int, time.Time, error)
	// RecordVerificationRequest notes that one went out.
	RecordVerificationRequest(ctx context.Context, email, subject string) error
	// SetDefaultRevocationPolicy stores the value that will be COPIED onto a
	// grant's own policy at share time (flow sharing spec §3.2). It changes only
	// the default; grants already made are untouched.
	SetDefaultRevocationPolicy(ctx context.Context, accountID, policy string) error

	MembershipsOfAccount(ctx context.Context, accountID string) ([]Membership, error)
	MembershipOf(ctx context.Context, userID, accountID string) (*Membership, error)
	UpdateMembershipRole(ctx context.Context, membershipID string, role Role) (*Membership, error)
	// MembershipByID exists because removing takes the membership's id and has
	// to know WHOSE it is before deciding — the grants to sweep hang off the
	// user, not off the row.
	MembershipByID(ctx context.Context, membershipID string) (*Membership, error)
	RemoveMembership(ctx context.Context, membershipID string) error

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
