package execution

import (
	"context"

	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/ports"
)

// Repository is the executor's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
//
// Every operation takes accountID explicitly and MUST filter by it in the WHERE:
// multi-tenant isolation is a required parameter of the port, not something the
// adapter could forget.
//
// Every write method stores state and event in the SAME transaction (ADR-0014) —
// and that is why there is no generic "SaveSandbox" here: a generic write does
// not know which event to emit, and the event would end up published outside the
// transaction by the caller.
type Repository interface {
	// ByID returns (nil, nil) when there is no row: whether absence is an error is
	// the domain's decision.
	ByID(ctx context.Context, accountID, id string) (*Sandbox, error)
	// ByIdempotencyKey resolves the REPEAT of a provisioning: same key, same
	// sandbox. Without it, a network retry would duplicate a microVM.
	ByIdempotencyKey(ctx context.Context, accountID, key string) (*Sandbox, error)
	// LiveByDemand returns the demand's NOT destroyed sandbox, if there is one.
	// An active demand has one sandbox (spec §1) — uniqueness is guaranteed by a
	// partial index in the database, and this query exists to answer before
	// trying to violate the constraint.
	LiveByDemand(ctx context.Context, accountID, demandID string) (*Sandbox, error)

	// Create writes the sandbox in provisioning and emits the start event.
	Create(ctx context.Context, s *Sandbox) (*Sandbox, error)
	// MarkProvisioned confirms what the executor DELIVERED: the real tier and
	// endpoints. The tier comes back because it is declared, never presumed — and
	// the row has to record what the client actually received.
	MarkProvisioned(ctx context.Context, accountID, id string, tier ports.IsolationTier, endpoints []Endpoint) (*Sandbox, error)
	// Transition applies a state change and emits the corresponding event. It
	// takes the whole Transition, not just the destination state, so the event
	// carries whether the work was preserved or lost — it is the question the
	// audit will ask later.
	Transition(ctx context.Context, accountID, id string, t Transition) (*Sandbox, error)
	// TouchActivity records usage and postpones idle suspension. It emits no
	// event: an activity heartbeat in an event log is noise that would drown the
	// demand's dossier.
	TouchActivity(ctx context.Context, accountID, id string) error

	// ListIdle feeds the saving sweeper: active sandboxes idle since before the
	// cutoff. It filters by account like everything else.
	ListIdle(ctx context.Context, accountID string, olderThanSeconds int) ([]Sandbox, error)

	// AccountsWithIdle is this domain's ONLY query that crosses accounts, and it
	// exists for a structural reason: the saving sweeper runs in the scheduler,
	// which is a SYSTEM actor and has no active account — while all the rest of
	// the domain requires one.
	//
	// Its output is no account's data: it is the list of accounts that HAVE
	// something to sweep. Every sweep still happens inside ONE account, with that
	// account in the context, so isolation is not loosened — what changes is only
	// who decides the visiting order.
	//
	// Without it, either the scheduler would gain unrestricted access, or an idle
	// sandbox would never suspend. The execution spec is explicit about the cost
	// of the second case: "an idle sandbox is what separates real parallelism
	// from a drowning machine".
	AccountsWithIdle(ctx context.Context, olderThanSeconds int) ([]string, error)
}

// Access is the NARROW port into the identity domain: the executor needs ONE
// thing about the caller — their role in the active account, because provisioning
// costs money and a viewer does not spend the account's money.
//
// The surface was chosen so that *identity.Service satisfies it as it stands:
// the composition root only wires, with no glue adapter.
type Access interface {
	Authorize(ctx context.Context, userID, accountID string) (*identity.Membership, error)
}

// Demands is the NARROW port into the demand domain.
//
// The executor needs to know TWO things before spending a microVM: the demand
// exists, and it belongs to the active account. Nothing more — no stage, no
// thread, no card. Declared here, and not imported from the demand package,
// because the dependency runs from executor to demand and not the other way:
// whoever executes knows what it executes, and inverting that would tie the two
// domains into a cycle.
type Demands interface {
	// DemandAccount returns the account that owns the demand.
	//
	// A nonexistent demand and a demand from ANOTHER account return the same
	// error (KindNotFound) on purpose: telling the two cases apart would leak the
	// existence of other accounts' ids to whoever kept trying.
	DemandAccount(ctx context.Context, demandID string) (string, error)
	// DemandProject returns the project the demand belongs to — the key of its
	// root repository (ADR-0021). Same rule as DemandAccount for absence.
	DemandProject(ctx context.Context, demandID string) (string, error)
}
