package app

import (
	"context"
	"time"

	"google.golang.org/grpc"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/adapter/clock"
	"github.com/Digital-Business-One/dop-core/internal/adapter/notifier"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres/projection"
	appgrpc "github.com/Digital-Business-One/dop-core/internal/app/grpc"
	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
	"github.com/Digital-Business-One/dop-core/internal/domain/cost"
	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/execution"
	"github.com/Digital-Business-One/dop-core/internal/domain/hierarchy"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/resource"
	"github.com/Digital-Business-One/dop-core/internal/domain/secondfactor"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// RegisterServices wires the domain services to the gRPC server.
//
// This is the composition root in action: the domain receives PORTS (repository,
// SecretStore), and the concrete adapters are chosen here. It takes a ctx
// because the event service opens ONE subscription per process, and that
// subscription lives as long as the process does — it is the server's context
// that ends it, not a client's.
func RegisterServices(ctx context.Context, srv *grpc.Server, deps *Deps) error {
	// identity is the root: hierarchy and resources authorize AGAINST it. That
	// is why it is born first and passed on as a port (resource.Access), not as
	// a concrete dependency.
	// The clock is a port, and it is now mandatory: the service refuses nil,
	// because accepting nil was what kept the abstraction decorative.
	relogio := clock.NewSystem()

	identitySvc := identity.NewService(postgres.NewIdentityRepo(deps.Pool), relogio)
	dopv1.RegisterIdentityServiceServer(srv, appgrpc.NewIdentityServer(identitySvc))

	// The second factor is born right after identity, and BEFORE the domains
	// that consume it: it is the gate the sensitive operations call, and a gate
	// wired after its callers is a gate that is nil on the first call.
	secondFactorSvc := secondfactor.NewService(
		postgres.NewSecondFactorRepo(deps.Pool),
		secondFactorUsers{identitySvc},
		deps.Secrets,
		deps.Mailer,
		deps.SMS,
		relogio,
	)
	dopv1.RegisterSecondFactorServiceServer(srv, appgrpc.NewSecondFactorServer(secondFactorSvc))
	identitySvc.WithStepUp(secondFactorSvc)

	hierarchySvc := hierarchy.NewService(postgres.NewHierarchyRepo(deps.Pool))
	dopv1.RegisterHierarchyServiceServer(srv, appgrpc.NewHierarchyServer(hierarchySvc))

	// The SecretStore arrives here already chosen by configuration (wire.go):
	// the resource domain stores a credential without knowing whether the vault
	// is k8s's or GCP's.
	resourceSvc := resource.NewService(postgres.NewResourceRepo(deps.Pool), identitySvc, deps.Secrets)
	resourceSvc.WithStepUp(secondFactorSvc)
	// Identity gets its grants sweep only now, because it needs the resource
	// service, which needs identity. It is the same cycle the step-up gate has,
	// resolved the same way: a port on one side, the wiring here.
	identitySvc.WithGrants(resourceSvc)
	dopv1.RegisterResourceServiceServer(srv, appgrpc.NewResourceServer(resourceSvc))

	// Live events: ONE subscription on the bus per process, with an in-memory
	// fan-out to the subscribers. A subscription PER CLIENT would create a
	// durable consumer on the broker for every open cockpit tab — and the port
	// has no way to remove them.
	eventSvc := event.NewService(postgres.NewEventRepo(deps.Pool), deps.Bus, relogio)
	if err := eventSvc.Start(ctx, "", []string{"dop.>"}); err != nil {
		return err
	}
	dopv1.RegisterEventServiceServer(srv, appgrpc.NewEventServer(eventSvc))

	// The flow repository satisfies TWO ports — the content and the chain of
	// ancestors. They are different questions the same table answers; separating
	// the ports keeps the domain saying what it needs, not who it needs it
	// from.
	wfRepo := postgres.NewWorkflowRepo(deps.Pool)
	workflowSvc := workflow.NewService(wfRepo, wfRepo, workflowAccess{identitySvc}, relogio)
	dopv1.RegisterWorkflowServiceServer(srv, appgrpc.NewWorkflowServer(workflowSvc))

	// The router is this package's POLICY, not a port: nil chooses the default
	// written in ADR-0011, which is a draft to be calibrated with telemetry
	// (P-7).
	costSvc := cost.NewService(postgres.NewCostRepo(deps.Pool), relogio, cost.NewRouter(nil))
	dopv1.RegisterCostServiceServer(srv, appgrpc.NewCostServer(costSvc))

	// The demand freezes the resolved flow and watches its own event log. It
	// knows how to do neither on its own — and it needs to know where neither
	// comes from (see internal/app/glue.go).
	demandSvc := demand.NewService(
		postgres.NewDemandRepo(deps.Pool),
		demandFlows{workflowSvc},
		demandWatcher{eventSvc},
		relogio,
	)
	dopv1.RegisterDemandServiceServer(srv, appgrpc.NewDemandServer(demandSvc))

	// A nil Embedder is DECLARED, not forgotten: with no embedding service
	// wired, the search falls back to the lexical path (trigram). The day there
	// is one, this is where it goes in — and only here.
	knowledgeSvc := knowledge.NewService(
		postgres.NewKnowledgeRepo(deps.Pool),
		deps.Objects,
		knowledgeDemands{demandSvc, hierarchySvc},
		nil,
		relogio,
		knowledge.Budget{},
	)
	dopv1.RegisterKnowledgeServiceServer(srv, appgrpc.NewKnowledgeServer(knowledgeSvc))

	// The git provider is resolved PER REPOSITORY (ADR-0013), not chosen at
	// boot — see internal/app/gitproviders.go.
	deliverySvc := delivery.NewService(
		postgres.NewDeliveryRepo(deps.Pool),
		deliveryDemands{demandSvc},
		relogio,
		gitProviders{deps.Pool, resourceSvc, deps.Secrets, deps.Cfg},
	)
	dopv1.RegisterDeliveryServiceServer(srv, appgrpc.NewDeliveryServer(deliverySvc))

	// The launcher arrives already chosen by configuration (wire.go): the domain
	// provisions a sandbox without knowing whether the substrate is Docker or
	// Kubernetes.
	executionSvc := buildExecution(deps, identitySvc, demandSvc, relogio)
	dopv1.RegisterExecutionServiceServer(srv, appgrpc.NewExecutionServer(executionSvc))

	// The attention box is a PROJECTION, and its service is read + streaming
	// only: an item is born from an event and dies from one, never from an
	// RPC.
	attentionSvc := attention.NewService(
		postgres.NewAttentionRepo(deps.Pool),
		attentionWatcher{eventSvc},
		relogio,
	)
	dopv1.RegisterAttentionServiceServer(srv, appgrpc.NewAttentionServer(attentionSvc))

	// The agent runtime lives HERE, and not in the BFF (ADR-0023): the
	// provider's credential comes out of the vault and is used in the same
	// process, crossing no network at all. The BFF is the layer exposed to the
	// internet — and compromising it must not hand over every account's agent
	// credentials.
	agentSvc := agent.NewService(
		agentProviders{resourceSvc, deps.Secrets},
		agentKnowledge{knowledgeSvc},
		agentRouting{costSvc},
		agentConversation{demandSvc},
		// With the sandbox wired, the agent stops merely conversing and starts
		// ACTING: the tool loop runs a command inside the demand's isolated
		// environment. It is the piece that separates modelling work from
		// executing work.
		agent.WithSandbox(agentSandbox{executionSvc}),
	)
	dopv1.RegisterAgentServiceServer(srv, appgrpc.NewAgentServer(agentSvc))

	return nil
}

// buildNotification assembles the communication trigger.
//
// Like buildExecution, it exists because TWO processes need it: the worker,
// which reacts to an event, and sched, which sweeps the attention box's delayed
// digest. Assembling it separately in both places is how the assemblies
// diverge.
func buildNotification(deps *Deps) *notification.Service {
	return notification.NewService(
		postgres.NewNotificationRepo(deps.Pool),
		deps.Mailer,
		clock.NewSystem(),
		notification.Config{
			BaseURL:     deps.Cfg.CockpitBaseURL,
			DigestDelay: deps.Cfg.DigestDelay,
		},
	)
}

// buildExecution assembles the execution service.
//
// It exists as a function because TWO processes need it: `serve`, which serves
// the RPCs, and `sched`, which sweeps idle sandboxes. Building it separately in
// both places is how the two assemblies diverge — one gains a new dependency and
// the other does not, and the behaviour starts depending on the mode.
func buildExecution(deps *Deps, id *identity.Service, dm *demand.Service, relogio ports.Clock) *execution.Service {
	return execution.NewService(
		postgres.NewExecutionRepo(deps.Pool),
		deps.Launcher,
		id,
		executionDemands{dm},
		relogio,
		execution.Config{
			DevboxImage:   deps.Cfg.DevboxImage,
			IngressDomain: deps.Cfg.IngressDomain,
		},
	)
}

// RegisterProjections subscribes the consumers that build the projections: the
// dossier, the timeline, the attention box, the metrics and the cost
// (ADR-0006).
func RegisterProjections(ctx context.Context, deps *Deps) error {
	log := logging.From(ctx)

	// Timeline: the timeline per aggregate. An idempotent consumer —
	// JetStream's delivery is at-least-once (ADR-0019).
	timeline := projection.NewTimeline(deps.Pool)
	if err := deps.Bus.Subscribe(ctx, "", "timeline", []string{"dop.>"}, timeline.Handle); err != nil {
		return err
	}

	// The attention box: it subscribes ONLY to the subjects the rule knows how
	// to translate. Subscribing to `dop.>` and discarding most would waste
	// deliveries; subscribing to too few would make the item never arrive, in
	// silence — there is a test in the domain guaranteeing the subjects cover
	// every handled event.
	attentionProj := projection.NewAttention(deps.Pool)
	if err := deps.Bus.Subscribe(ctx, "", "attention", attention.Subjects(), attentionProj.Handle); err != nil {
		return err
	}

	// Communication: the TRIGGER (ADR-0025). It subscribes only to the subjects
	// the rule knows how to translate, and the decider is a table — when the
	// reaction becomes data (P-29), you swap the loader, not the caller.
	//
	// No use case calls the Mailer directly: if one did, it would become the
	// trigger, diffused across as many use cases as sent email.
	notificacao := buildNotification(deps)
	if err := deps.Bus.Subscribe(ctx, "", "notification",
		notification.Subjects(), notifier.NewConsumer(notificacao).Handle); err != nil {
		return err
	}

	log.Info("projections registered", "total", 3,
		"consumidores", []string{"timeline", "attention", "notification"})
	return nil
}

// RegisterLauncher prepares the execution substrate in the launcher process.
//
// Today the sandbox's life cycle is driven by CALLS (ProvisionSandbox and
// company) and by SWEEPS (the scheduler suspends the idle ones). There is no
// event subscription: provisioning automatically when a demand starts is a
// policy that has not been decided, and creating a sandbox — which costs money —
// per event without that decision would be inventing spend governance.
//
// When the policy exists, this is where the subscription goes.
func RegisterLauncher(ctx context.Context, deps *Deps) error {
	log := logging.From(ctx)
	tiers, err := deps.Launcher.SupportedTiers(ctx)
	if err != nil {
		// Not knowing which isolation the substrate offers is a reason NOT to
		// come up: `isolationTier` is declared and checked, and a launcher that
		// cannot answer would accept anything later on.
		return err
	}
	log.Info("launcher pronto", "substrato", deps.Cfg.SandboxBackend, "isolamentos", tiers)
	return nil
}

// RunScheduledTasks runs the periodic cycle: PR polling, suspension of idle
// sandboxes, creation of future event partitions, invite expiry and cleanup of
// the idempotency table.
func RunScheduledTasks(ctx context.Context, deps *Deps) {
	log := logging.From(ctx)

	// Future partitions FIRST, before any other task: without them, at the turn
	// of the month every event write fails — and it fails on the outbox's path,
	// bringing down any operation that changes state. The migrations create
	// fixed partitions and stop; what carries on from here is this.
	created, err := postgres.EnsureMonthlyPartitions(ctx, deps.Pool, time.Now().UTC(), partitionsAhead)
	if err != nil {
		// It does not bring the cycle down: the next one tries again, and there
		// are months of slack before the absence becomes a problem. But it goes
		// up as an ERROR, because it is the warning that separates "months of
		// slack" from "tomorrow everything stops".
		log.Error("failed to ensure the future partitions", logging.FieldError, err.Error())
	}
	if len(created) > 0 {
		log.Info("partitions created", "partitions", created)
	}

	// The cost-saving sweep: a sandbox stopped beyond the limit is suspended.
	// The pod dies, the workspace survives on the volume. The substrate's spec
	// is blunt about the cost of not doing this — "an idle sandbox is what
	// separates real parallelism from a drowned machine".
	{
		sweeper := execution.NewSweeper(
			postgres.NewExecutionRepo(deps.Pool), deps.Launcher, clock.NewSystem())
		accounts, suspended, err := sweeper.SweepAllAccounts(ctx)
		if err != nil {
			log.Error("the idle sandbox sweep failed", logging.FieldError, err.Error())
		} else if suspended > 0 {
			log.Info("idle sandboxes suspended", "accounts", accounts, "sandboxes", suspended)
		}
	}

	// The attention box's delayed digest (ADR-0025): an item open for longer
	// than the delay, and still open, becomes an email. What was resolved before
	// the cut-off does not — whoever was in the cockpit has already resolved it.
	//
	// The delay is a QUERY PREDICATE, not a scheduler: there is no timer to
	// cancel when the item closes, and a cancellation path only runs in the rare
	// case, which is where it breaks quietly.
	if accounts, notices, err := buildNotification(deps).SweepDigest(ctx); err != nil {
		log.Error("the attention digest sweep failed", logging.FieldError, err.Error())
	} else if notices > 0 {
		log.Info("attention digests sent", "accounts", accounts, "notices", notices)
	}

	log.Debug("scheduler cycle")
}

// partitionsAhead is slack, not a forecast: with 3 months the scheduler can be
// down for weeks without anyone noticing the difference.
const partitionsAhead = 3
