package resource

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
)

// Repository is the resource domain's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
//
// Every operation takes accountID explicitly: multi-tenant isolation is a
// required parameter of the port, not something the adapter could forget.
type Repository interface {
	// Resources
	List(ctx context.Context, accountID string, kind Kind) ([]Resource, error)
	ByID(ctx context.Context, accountID, id string) (*Resource, error)
	Create(ctx context.Context, r *Resource) (*Resource, error)
	// Update writes the new configuration. bumpVersion=true increments the
	// version instead of overwriting — that is what separates content from a
	// credential.
	Update(ctx context.Context, accountID, id string, config map[string]any, bumpVersion bool) (*Resource, error)
	Delete(ctx context.Context, accountID, id string) error
	// SetCredentialRef stores ONLY the opaque pointer. The secret's value never
	// passes through this port at any point.
	SetCredentialRef(ctx context.Context, accountID, id, ref string) (*Resource, error)

	// Grants
	GrantsOfUser(ctx context.Context, accountID, userID string) ([]Grant, error)
	GrantOf(ctx context.Context, accountID, resourceID, userID string) (*Grant, error)
	GrantByID(ctx context.Context, accountID, grantID string) (*Grant, error)
	// Grant is an upsert: granting again at another level ADJUSTS the grant, it
	// does not duplicate the row (the table has UNIQUE (resource_id, user_id)).
	Grant(ctx context.Context, accountID string, g *Grant) (*Grant, error)
	RevokeGrant(ctx context.Context, accountID, grantID string) error
}

// Access is the NARROW port into the identity domain: a resource needs to know
// two things about the caller — their role in the active account (for owner and
// admin's implicit manage) and the account's nature (for the by-nature access
// default). Nothing beyond that.
//
// The surface was chosen so that *identity.Service satisfies it as it stands:
// the composition root only wires, with no glue adapter and without duplicating
// the role rule in two places.
type Access interface {
	Authorize(ctx context.Context, userID, accountID string) (*identity.Membership, error)
	GetAccount(ctx context.Context, id string) (*identity.Account, error)
}
