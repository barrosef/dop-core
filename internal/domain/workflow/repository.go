package workflow

import "context"

// Repository is the flow domain's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
//
// Every operation takes accountID explicitly: multi-tenant isolation is a
// required parameter of the port, not something the adapter could forget. The
// only exception is the PLATFORM CATALOGUE, which has no owner — reads always
// see it, writes never reach it.
//
// Note what does NOT exist here: no operation that alters a stored version. It
// is not an oversight — it is the domain's invariant expressed in the shape of
// the port. The way to change a flow is AppendVersion, and only that.
type Repository interface {
	// List returns the account's flows. An empty scope lists every level; an
	// empty ownerID lists every owner at that level.
	List(ctx context.Context, accountID string, scope Scope, ownerID string) ([]Flow, error)

	// ByID returns the flow's CURRENT version.
	ByID(ctx context.Context, accountID, id string) (*Flow, error)

	// VersionOf returns a specific version — frozen, exactly as it was written.
	// This is how a demand in progress reads the flow it froze on start
	// (ADR-0010 §4).
	VersionOf(ctx context.Context, accountID, id string, version int32) (*Flow, error)

	// ByOwners returns the current version of EACH requested level's flow, in a
	// single query.
	//
	// It exists as an operation of its own — and not as a loop over ByID —
	// because resolution happens every time a demand is opened and the chain has
	// five levels: that would be five round trips on the product's hottest
	// screen. Levels with no declared flow simply do not come back.
	ByOwners(ctx context.Context, accountID string, refs []ScopeRef) ([]Flow, error)

	// Create writes the flow and version 1. idempotencyKey is required: repeating
	// the call returns what was already created instead of creating a second
	// flow.
	Create(ctx context.Context, f *Flow, idempotencyKey string) (*Flow, error)

	// AppendVersion writes the NEXT version without touching the previous one.
	// baseVersion is the version the author worked on: if the flow has already
	// moved on, the write is refused as a conflict instead of overwriting
	// somebody else's work.
	AppendVersion(ctx context.Context, accountID string, f *Flow, baseVersion int32, idempotencyKey string) (*Flow, error)

	// Promote publishes src's content at the target level: it creates the
	// target's flow if it has none, or appends a version to the one that exists.
	//
	// It publishes, it does not move: the source flow stays where it is. Moving
	// would pull the flow out from under the demands that already adopted it.
	Promote(ctx context.Context, accountID string, src *Flow, target ScopeRef, idempotencyKey string) (*Flow, error)
}

// Ancestry is the NARROW port into the account's tree: given a concrete level,
// what is the chain of levels above it.
//
// This domain needs ONE thing from hierarchy and from demand — the lineage — and
// not project, workspace or demand as entities. Declaring the port with that
// surface is what keeps flow from becoming a client of two other domains over a
// single query.
//
// The chain comes back from the MOST GENERIC to the MOST SPECIFIC, including the
// target, and always starting at the platform:
//
//	demand d ⇒ [platform, account a, workspace w, project p, demand d]
//
// A target that does not exist, or that belongs to another account, is NotFound:
// somebody outside the account should not even discover that the id exists.
type Ancestry interface {
	ChainOf(ctx context.Context, accountID string, target ScopeRef) ([]ScopeRef, error)
}

// Access is the NARROW port into the identity domain: promoting a flow to the
// level above requires `manage`, and knowing that only takes the actor's ROLE in
// the active account. Nothing beyond that.
//
// The role travels as a plain string on purpose. The vocabulary belongs to the
// identity domain, and importing its type here would couple the two packages
// over a single comparison — the composition root wires it with three lines of
// glue and each domain keeps understanding only what it needs.
type Access interface {
	RoleOf(ctx context.Context, userID, accountID string) (string, error)
}

// Roles with implicit `manage` over the account's content (ADR-0009): owner and
// admin. Without it, nobody can fix a flow published by somebody who has already
// left the company.
const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
)

func canManage(role string) bool { return role == RoleOwner || role == RoleAdmin }
