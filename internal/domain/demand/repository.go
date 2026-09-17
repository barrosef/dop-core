package demand

import (
	"context"

	"github.com/barrosef/dop-core/internal/domain/ports"
)

// Emission is the EVENT the operation produces, the way the domain sees it: type
// and content. Aggregate and aggregate_id are not fields because they are always
// the same — `demand` and the demand's id — and that is not saved typing: it is
// ADR-0004's rule ("an append-only log PER DEMAND") written in a shape that
// cannot be violated by carelessness. A thread message, a finding and a gate
// decision all belong to the demand's log; whoever needs to slice by thread reads
// `thread_id` from the payload.
type Emission struct {
	Type    string
	Payload map[string]any
}

// The demand's event types. The outbox derives the NATS subject from them.
const (
	EventStarted          = "dop.demand.started"
	EventStageAdvanced    = "dop.demand.stage.advanced"
	EventGateDecided      = "dop.demand.gate.decided"
	EventThreadCreated    = "dop.demand.thread.created"
	EventThreadBlocked    = "dop.demand.thread.blocked"
	EventThreadResumed    = "dop.demand.thread.resumed"
	EventThreadConcluded  = "dop.demand.thread.concluded"
	EventMessagePosted    = "dop.demand.message.posted"
	EventFindingPublished = "dop.demand.finding.published"
)

// Aggregate is the aggregate's name in the log — one for everything the demand owns.
const Aggregate = "demand"

// Repository is the demand domain's persistence PORT.
//
// Note the shape of EVERY write: it takes the new state, the Emission and the
// idempotency key, and returns the result. That is deliberate — the signature
// forces the adapter to write state and event in the SAME transaction
// (ADR-0014). A port with `Save` on one side and `Emit` on the other would leave
// atomicity to the caller's discipline, which is exactly what the ADR exists so
// as not to depend on.
//
// Every operation takes accountID explicitly: multi-tenant isolation is a
// required parameter of the port, not something the adapter could forget.
type Repository interface {
	// ── the demand ──
	List(ctx context.Context, accountID, projectID string, limit int, after string) ([]Demand, error)
	ByID(ctx context.Context, accountID, id string) (*Demand, error)
	ByExternalKey(ctx context.Context, accountID, projectID, externalKey string) (*Demand, error)
	// Create writes the demand with the flow already frozen and its initial stages.
	Create(ctx context.Context, d *Demand, ev Emission, idemKey string) (*Demand, error)
	// SaveStage writes the stage and the demand's projected status. It returns the
	// stage as it was stored.
	SaveStage(ctx context.Context, accountID, demandID string, st Stage, status DopStatus, ev Emission, idemKey string) (*Stage, error)

	// ── threads ──
	ThreadsOf(ctx context.Context, accountID, demandID string) ([]Thread, error)
	ThreadByID(ctx context.Context, accountID, id string) (*Thread, error)
	CreateThread(ctx context.Context, t *Thread, ev Emission, idemKey string) (*Thread, error)
	SaveThreadState(ctx context.Context, accountID, threadID string, state ThreadState, ev Emission, idemKey string) (*Thread, error)

	// AppendMessage appends to the log. There is no `UpdateMessage` and no
	// `DeleteMessage`: the log is append-only (ADR-0004).
	AppendMessage(ctx context.Context, m *Message, ev Emission, idemKey string) (*Message, error)

	// ── findings ──
	CreateFinding(ctx context.Context, f *Finding, ev Emission, idemKey string) (*Finding, error)
	// ListFindings returns the findings ALREADY PUBLISHED on the demand.
	//
	// It exists for the context package (ADR-0006): without the findings, an agent
	// resuming the demand redoes an investigation another already finished — which
	// is exactly the waste the findings board exists to prevent.
	ListFindings(ctx context.Context, accountID, demandID string) ([]Finding, error)

	// HasFinding answers whether the thread has already published a finding — it
	// is what unlocks concluding it.
	HasFinding(ctx context.Context, accountID, threadID string) (bool, error)
}

// FlowResolver is the NARROW port into the flow domain.
//
// The demand needs ONE thing from it, once in its life: the effective flow at
// the instant it starts, resolved by the chain platform ◁ account ◁ workspace ◁
// project ◁ demand (ADR-0010). After that the live flow stops mattering — what
// drives the demand is the frozen snapshot. That is why the port has one method
// and no notion of editing, versioning or promoting a flow: those belong to the
// workflow domain, and this package does not know them.
type FlowResolver interface {
	// Resolve returns the requested scope's effective flow (scope: "project",
	// "demand", "workspace", "account"), with version and stages — enough to
	// freeze.
	Resolve(ctx context.Context, accountID, scope, scopeID string) (Flow, error)
}

// Watcher is the event SUBSCRIPTION port, for WatchDemand.
//
// Designed on the surface the `event` domain already offers (a single fan-out
// per process, replay by cursor, per-account isolation): subscribing per client
// on the bus would create a durable consumer per open cockpit tab, and
// reimplementing fan-out here would duplicate the slow-consumer policy in two
// places that drift apart over time.
//
// The filter is by aggregate and type because that is what the event service
// knows how to do; the slice by DEMAND is done in this package, comparing the
// aggregate_id — see Service.Watch.
type Watcher interface {
	Watch(ctx context.Context, sinceEventID string, aggregates, types []string, emit func(ports.Event) error) error
}
