package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ════════════════════════════════════════════════════════════════════════════
// THE TURN'S CYCLE, and the decisions that are not obvious.
//
// 0. THE PROVIDER FIRST. A missing credential is discovered on the first line,
//    not after assembling context and spending two reads. The error that reaches
//    the user talks about configuration, which is what it is.
//
// 1. CONTEXT AND THREAD. The package already arrives cut by token budget
//    (ADR-0009 §3) and reports what was DROPPED — which does not vanish: it
//    enters the prefix (the agent needs to know it is reading partial context)
//    and becomes a message on the thread (the human needs to know why the reply
//    came out the way it did).
//
//    The BFF's version did these two reads in PARALLEL, and rightly so: they
//    were two network round trips, and summing the latencies turned aggregation
//    into a cost. Here they are two in-process calls, and concurrency would buy
//    microseconds at the price of a goroutine, a channel and two possible error
//    orderings. It is sequential on purpose — it is one of the six gRPC round
//    trips ADR-0023 went after.
//
// 2. ROUTING BELONGS TO THE COST DOMAIN, and its justification travels whole
//    (ADR-0011 §3). What the runtime does is the half the policy cannot do:
//    translate the CLASS into the ACTIVE provider's concrete name — the same
//    policy × catalog separation as `cost/router.go`. When the thread's card
//    declares a model (ADR-0010 §2), it wins: the card is that thread's frozen
//    contract, and changing its model midway would invalidate every previous
//    turn's cached prefix, because cache is per model.
//
// 3. STABLE PREFIX FIRST, VOLATILE AFTER. See prompt.go.
//
// 3b. TOOLS COME FROM THE CARD (ADR-0010 §2), AND THE LOOP BELONGS HERE. The
//    card grants names; the runtime's catalog (tools.go) resolves what exists;
//    the loop (toolloop.go) executes. Three things can go wrong before the first
//    call, and all three BECOME WARNINGS instead of silence or an error: a
//    granted name that does not exist, a card granting tools in an installation
//    with no substrate, and a model asking for a tool when none was declared.
//
// 4. MEASUREMENT IS IDEMPOTENT, AND THE KEY IS DERIVED FROM THE TURN. A
//    duplicate consumption record collides with nothing: it would enter as
//    legitimate spend and the budget would become fiction. Every write in this
//    cycle derives from the SAME key (`:msg-in`, `:notice`, `:usage:<n>`,
//    `:msg-out`, `:loop-notice`, `:finding`), so that repeating the same request
//    repeats ZERO effects.
//
//    The consumption's `<n>` is the loop's ROUND, and it is what changed with
//    tools: a turn consumes once per round, and a single key would make the cost
//    domain discard everything from the second onwards as a duplicate — the
//    budget would see an eighth of the real spend in an eight-round turn. See
//    usageKeyFor.
//
//    A deliberate difference from the BFF's version: there, with no key from the
//    client, the runtime GENERATED one — every call became a new turn. Here the
//    key is MANDATORY. On the core's side, generating it would turn a network
//    retry into double consumption and a duplicated message on the thread,
//    exactly what `cost.Service.RecordUsage` refuses to do by requiring the key.
//    Whoever knows they are retrying is the client, and now they have to say so.
//
// 5. EVERY MESSAGE IS AN EVENT (ADR-0006), and that is how the cockpit finds
//    out: the WatchDemand that already exists delivers the events on its own.
//    There is NO second streaming path here, on purpose — it would be a second
//    source of truth for the same timeline.
//
// 6. THE REPLY'S AUTHORSHIP BELONGS TO THE AGENT. The question belongs to the
//    human who wrote it; the reply belongs to the agent that produced it. The
//    core derives authorship from `ctxutil.Call`, so the cycle SWAPS the actor
//    before publishing the reply and the finding. Recording an agent's utterance
//    as a human's would make the event log — which is the demand's truth
//    (ADR-0006) — lie about who did what, on a platform whose entire premise is
//    telling the two apart.
//
// 7. CONCLUDING REQUIRES PUBLISHING A FINDING (spec §1), AND REQUIRES HAVING
//    FINISHED. Refusing the empty conclusion happens in turn.go; refusing the
//    conclusion of a loop that stopped at the cap or on budget happens here. A
//    finding is durable — it goes to the project's memory and to its siblings'
//    context — and publishing one written mid-work is worse than publishing none.
//
// 8. A BLOWN BUDGET PAUSES, IT DOES NOT KILL (ADR-0011 §2), AND NOW ON TWO
//    LEVELS. Between turns, as always: the turn that already ran is delivered
//    whole and the NEXT one does not go out. And INSIDE the turn, which is new:
//    blowing mid-loop stops the loop on the next round, with what already ran
//    delivered. Aborting in either case would be the hard cut the ADR refused,
//    and on top of that it would throw away tokens already paid for.
// ════════════════════════════════════════════════════════════════════════════

// Service executes agent turns. It takes only PORTS.
type Service struct {
	providers Providers
	knowledge Knowledge
	routing   Routing
	conv      Conversation
	// sandbox is the ONLY optional port here. Nil = the agent converses and
	// does not act; see Option and the Sandbox port.
	sandbox Sandbox
	// maxToolRounds is the tool-round cap per turn (ADR-0011).
	maxToolRounds int
}

// Option adjusts the service at assembly time.
//
// Why a variadic option and not a constructor parameter, going against the
// house's other services' style: the four mandatory parameters are used on EVERY
// turn, and that is why nil in them is an assembly error deserving a panic.
// These two are not — an installation with no execution substrate runs turns,
// and the cap has a domain default that is the right answer in most
// installations. On top of that, the existing wiring keeps compiling: adding a
// capability must not cost a change to whoever does not use it.
type Option func(*Service)

// WithSandbox wires the execution substrate — it is what turns a "platform that
// MODELS agent work" into a "platform that EXECUTES agent work".
func WithSandbox(s Sandbox) Option {
	return func(svc *Service) { svc.sandbox = s }
}

// WithMaxToolRounds adjusts the tool-round cap per turn.
//
// A value <= 0 is IGNORED and the default applies. Accepting zero as "no cap"
// would give whoever forgot to configure it exactly the behaviour ADR-0011
// forbids — and forgetting is the likeliest case.
func WithMaxToolRounds(n int) Option {
	return func(svc *Service) {
		if n > 0 {
			svc.maxToolRounds = n
		}
	}
}

// NewService refuses a nil dependency.
//
// The panic is deliberate, and for `identity.NewService`'s same reason: this is
// an ASSEMBLY error, and an assembly error has to show up at boot, not at three
// in the morning on the first turn somebody tries to run. Accepting nil and
// falling back internally is what turns a port into decoration.
//
// Note what is NOT on the list: `ports.Clock`. This service stamps no instant —
// whoever records a message, a consumption and a finding is the domain that owns
// each, and each has its own clock. More than that: the prompt's prefix has to
// be clock-free (ADR-0012 §1, prompt.go's layer 2), and a clock available on the
// service would be a permanent invitation to stamp the prefix.
func NewService(providers Providers, knowledge Knowledge, routing Routing, conv Conversation,
	opts ...Option) *Service {

	switch {
	case providers == nil:
		panic("agent.NewService: a provider factory is mandatory — without it there is nobody to talk to")
	case knowledge == nil:
		panic("agent.NewService: the knowledge port is mandatory — an agent with no context is a blind agent")
	case routing == nil:
		panic("agent.NewService: the cost port is mandatory — a turn with no measurement is a fictional budget")
	case conv == nil:
		panic("agent.NewService: the demand port is mandatory — a reply that does not become a message vanishes")
	}
	s := &Service{
		providers: providers, knowledge: knowledge, routing: routing, conv: conv,
		maxToolRounds: DefaultMaxToolRounds,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Translation keys for the refusals a person reads.
const (
	KeyTurnNeedsThread   = "agent.turn.demand_and_thread_required"
	KeyTurnNeedsText     = "agent.turn.text_required"
	KeyTurnNeedsTaskKind = "agent.turn.task_kind_required"
	KeyTurnNeedsIdemKey  = "agent.turn.idempotency_key_required"
)

// TurnRequest is a turn to execute on a thread.
type TurnRequest struct {
	DemandID string
	ThreadID string
	Text     string
	// TaskKind is an OPEN vocabulary: the router handles the unknown by falling
	// back to the expensive option and SAYING that it did (ADR-0011 §3). Empty
	// is what does not pass — with no kind of work there is no decision to
	// audit.
	TaskKind string
	// ResourceID is the `agent`-category resource (ADR-0013) serving this turn.
	// Empty = the account's default provider.
	ResourceID string
	// OperatorNote is the OPERATOR's intervention, coming from the attention
	// box. It enters through the provider's authority channel, never as user
	// text (see D3).
	OperatorNote    string
	MaxOutputTokens int
	// MaxToolRounds lets the caller LOWER this turn's round cap. Zero uses the
	// service's; a value HIGHER than the service's is ignored.
	//
	// Downwards only, and it is the decision that matters here: a cap the
	// client can raise is not a cap, it is a suggestion — and ADR-0011 §2 does
	// not ask for a suggestion. Whoever wants to spend more changes the
	// installation's policy, where the change is visible, and not a request
	// field nobody audits.
	MaxToolRounds int
}

// RoutingView is the decision that applied, with the WHOLE justification.
type RoutingView struct {
	TaskKind      string
	Class         ModelClass
	Model         string
	Effort        Effort
	EffortApplied Effort
	Reason        string
	// FromAgentCard is true when the thread's card beat the router.
	FromAgentCard bool
}

// TurnUsage is the four disjoint parts, the cost and — what matters most —
// whether each number is KNOWN.
type TurnUsage struct {
	Usage
	CostMicros Micros
	Currency   string
	// CacheCreationKnown false means the provider does not report cache
	// creation (D1). Zero would assert nothing was written, which is a
	// different thing.
	CacheCreationKnown bool
	// CostKnown false means there is no price table for this model. The cost
	// does NOT become a consolation zero: a budget fed with zeros is the
	// fiction ADR-0011 §2 exists to prevent.
	CostKnown bool
}

// TurnOutcome is the turn's result.
type TurnOutcome struct {
	DemandID string
	ThreadID string
	Provider string
	Routing  RoutingView
	Reply    string
	// MessageIDs are the messages published on the thread, in the order they
	// entered.
	MessageIDs       []string
	Concluded        bool
	Finding          *FindingRef
	Usage            TurnUsage
	ContextTruncated bool
	// ── the tool loop ──────────────────────────────────────────────────────
	// ToolRounds is how many ROUNDS the turn took — how many times the model
	// was called. One round is the turn with no tool at all, which is still the
	// common case.
	ToolRounds int
	// ToolCalls is how many tools were REQUESTED across the whole turn.
	ToolCalls int
	// LoopStop is why the loop stopped. `finished` is the only one that means
	// "the reply above is complete" — see LoopStop.
	LoopStop LoopStop
	// MaxToolRounds is the cap that applied. It travels along because "I hit
	// the cap" is only actionable for whoever knows what the cap was.
	MaxToolRounds int
	// Paused: the budget blew and the demand becomes a decision item
	// (ADR-0011 §2).
	Paused   bool
	Notice   string
	Budgets  []BudgetView
	Warnings []string
}

// idemKeys derives the idempotency keys of this turn's five writes.
//
// Derived and not drawn at random: it is what makes resending the same request
// repeat ZERO effects — the message does not duplicate, the consumption does not
// count twice and the finding is not published again.
func idemKeys(turnKey string) map[string]string {
	m := make(map[string]string, 6)
	for _, target := range []string{"msg-in", "notice", "loop-notice", "msg-out", "finding"} {
		m[target] = turnKey + ":" + target
	}
	return m
}

// usageKeyFor derives the idempotency key of ONE round's measurement.
//
// The key is `<turn>:usage:<n>` and not `<turn>:usage` because with tools a turn
// consumes SEVERAL times, and a single key would make the cost domain discard
// everything from the second round onwards as a duplicate — the budget would
// start seeing an eighth of the real spend in an eight-round turn.
//
// Repeating the same request still repeats zero effects: the rounds get the same
// keys in the same order. A repetition needing MORE rounds than the original
// writes new lines only for the new rounds — and that is right, because those
// tokens were really spent.
func usageKeyFor(turnKey string) func(int) string {
	return func(round int) string {
		return turnKey + ":usage:" + strconv.Itoa(round)
	}
}

// asAgent returns the context with authorship swapped to the AGENT.
//
// The account, the request id and everything else stay intact: what changes is
// WHO speaks. The actor is the thread — which is what `dop.v1.ActorRef` already
// documents for an agent ("id = the agent's thread_id") — and the name is the
// thread's key, which is how the human sees it in the cockpit.
//
// This function is the most important line of this file from the product's point
// of view. Without it, the agent's reply would enter the event log signed by
// whoever pressed the button, and the platform would lose the one distinction it
// exists to maintain.
func asAgent(ctx context.Context, t Thread) context.Context {
	call, _ := ctxutil.From(ctx)
	call.ActorKind = ctxutil.ActorAgent
	call.ActorID = t.ID
	call.ActorName = t.Key
	return ctxutil.Into(ctx, call)
}

// RunTurn executes ONE turn on a thread. See the cycle in this file's header.
func (s *Service) RunTurn(ctx context.Context, req TurnRequest, idempotencyKey string) (*TurnOutcome, error) {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.DemandID) == "" || strings.TrimSpace(req.ThreadID) == "" {
		return nil, errs.Invalid("a turn requires a demand and a thread").
			WithCode(KeyTurnNeedsThread, nil)
	}
	if strings.TrimSpace(req.Text) == "" {
		return nil, errs.Invalid("a turn with no text").WithCode(KeyTurnNeedsText, nil)
	}
	if strings.TrimSpace(req.TaskKind) == "" {
		return nil, errs.Invalid("a turn with no kind of work: without it there is no routing to audit").
			WithCode(KeyTurnNeedsTaskKind, nil)
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		// See decision 4 in the header: generating one here would turn a
		// network retry into double consumption and a duplicated message.
		return nil, errs.Invalid("a turn requires an idempotency key: the five writes derive from it").
			WithCode(KeyTurnNeedsIdemKey, nil)
	}
	ks := idemKeys(idempotencyKey)

	// 0. The provider BEFORE anything else: a missing credential fails here,
	// cheaply, and with a message that talks about configuration.
	provider, err := s.providers.For(ctx, req.ResourceID)
	if err != nil {
		return nil, err
	}
	info := provider.Info()

	// 1. Thread (card and key) and context package.
	thread, err := s.conv.Thread(ctx, req.DemandID, req.ThreadID)
	if err != nil {
		return nil, err
	}
	pkg, err := s.knowledge.ContextPackage(ctx, req.DemandID)
	if err != nil {
		return nil, err
	}

	// 2. Routing.
	decision, err := s.routing.Route(ctx, req.TaskKind, req.DemandID)
	if err != nil {
		return nil, err
	}
	model, effort, fromCard := modelAndEffort(info, decision, thread.Card)

	// The question enters the thread BEFORE the model call: if the provider
	// goes down, the conversation shows what was asked instead of a hole.
	var ids []string
	inbound, err := s.conv.PostMessage(ctx, thread.ID, req.Text, ks["msg-in"])
	if err != nil {
		return nil, err
	}
	ids = append(ids, inbound)

	notice := TruncationNotice(pkg)
	if notice != "" {
		// The truncation does not vanish: whoever reads the thread needs to
		// know the reply below was produced without part of the context.
		note, err := s.conv.PostMessage(ctx, thread.ID, notice, ks["notice"])
		if err != nil {
			return nil, err
		}
		ids = append(ids, note)
	}

	// 3. The tools granted to THIS thread (ADR-0010 §2). A granted name that
	// does not exist in the catalog does not stop the turn: it becomes a
	// warning, and the prompt's brief lists only what actually exists (see
	// brief()).
	tools, unknown := ToolCatalog(thread.Card.Tools)
	var warnings []string
	if w := unknownToolsWarning(unknown); w != "" {
		warnings = append(warnings, w)
	}
	if len(tools) > 0 && s.sandbox == nil {
		// The card promises action and the installation has no substrate.
		// Declaring the tools anyway would make the agent plan on top of them
		// and find out on the first call; not declaring and not warning would
		// make the card look honoured. That leaves the third way: do not
		// declare, and SAY so.
		warnings = append(warnings, "this thread's card grants tool(s) ("+
			namesOf(tools)+"), but this installation has no execution substrate "+
			"wired: the turn ran WITHOUT tools")
		tools = nil
	}

	// 4. Stable prefix first, volatile after (ADR-0012 §1).
	turn := BuildTurn(pkg, thread.Key, thread.Card, req.Text, req.OperatorNote,
		req.MaxOutputTokens, tools)

	// 5. The loop: send, execute the tool, measure EVERY round, decide whether
	// to continue. The measurement comes before publishing the reply for the
	// usual reason: if the process dies midway, it is better to have recorded
	// tokens already paid for than to have published a reply for free.
	//
	// The loop runs with the ORIGINAL `ctx`, not the agent's (`asAgent`). The
	// actor swap exists for the AUTHORSHIP of what is published — who spoke —
	// and using it here would also swap the AUTHORIZATION: the command would
	// start being executed on behalf of a thread, which is a member of no
	// account. Whoever authorizes running a command in the sandbox is the
	// person who pressed the button, and it is their permission the execution
	// domain checks.
	roundCap := s.roundCap(req.MaxToolRounds)
	l := loop{
		provider: provider, sandbox: s.sandbox, routing: s.routing, info: info,
		demandID: req.DemandID, threadID: thread.ID,
		model: model, effort: effort, maxRounds: roundCap,
		usageKey: usageKeyFor(idempotencyKey), allowed: tools,
	}
	res, err := l.run(ctx, turn)
	if err != nil {
		return nil, err
	}
	execution := res.exec
	reply := execution.ModelReply
	warnings = append(warnings, res.warnings...)
	accounting := res.accounting

	effectiveModel := reply.Model
	if effectiveModel == "" {
		effectiveModel = model
	}
	if !res.priceKnown {
		warnings = append(warnings, fmt.Sprintf(
			"no price table for %q on %q: the consumption was recorded in tokens, "+
				"and the COST was left at zero for LACK of a table — not because it was free",
			effectiveModel, info.Name))
	}

	// 6 and 7. The reply on the thread, signed by the AGENT.
	agentCtx := asAgent(ctx, thread)
	outbound, err := s.conv.PostMessage(agentCtx, thread.ID, execution.Reply, ks["msg-out"])
	if err != nil {
		return nil, err
	}
	ids = append(ids, outbound)

	// A loop stop that was NOT "I finished" becomes a message on the thread,
	// for the truncation warning's same reason: whoever reads the conversation
	// needs to know the reply above is interrupted work, and not a conclusion.
	// It goes with the CALLER's authorship and not the agent's — it is the
	// platform's statement about the agent, and signing it as the agent would
	// put in its mouth something it did not say.
	if note := LoopNotice(res.stop, res.rounds, roundCap); note != "" {
		id, err := s.conv.PostMessage(ctx, thread.ID, note, ks["loop-notice"])
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	// 8. Concluding requires publishing a finding (spec §1) — and the finding
	// also belongs to the agent, for the message's same reason.
	//
	// `res.stop.Concluded()` is the second condition: a loop that hit the cap or
	// stopped on budget does NOT conclude, even if the model's last utterance
	// says it does. A finding written before the work finished is worse than no
	// finding at all — it is durable, it enters the project's memory and its
	// siblings' context, and it starts counting as truth.
	//
	// A SECOND LOCK, and it is worth recording why: today it is redundant. Every
	// stop that is not `finished` happens with pending tool calls, and
	// `executeTurn` already zeroes `concluded` in the presence of calls
	// (turn.go) — so that no test can exercise THIS line in isolation. It stays
	// because the two rules are independent and live in different places:
	// "whoever asks for a tool did not finish" is about the RESPONSE, "an
	// interrupted loop does not conclude" is about the LOOP. On the day the
	// first changes, this is what stops a finding written mid-work from entering
	// the project's memory. The rule itself is testable, and is tested, in its
	// pure form: LoopStop.Concluded.
	concluded := execution.Concluded && res.stop.Concluded()
	if execution.Concluded && !res.stop.Concluded() {
		warnings = append(warnings,
			"the model marked a conclusion on a round where the loop STOPPED because of "+
				string(res.stop)+": the conclusion was REFUSED and no finding was published")
	}
	var findingRef *FindingRef
	if concluded && execution.Finding != nil {
		payload := map[string]any{}
		for k, v := range execution.Finding.Payload {
			payload[k] = v
		}
		// PROVENANCE enters the finding: whoever audits it needs to know with
		// which model and under which policy it was produced, and the finding is
		// durable — it goes to the project's memory and to its siblings'
		// context.
		payload["provider"] = info.Name
		payload["model"] = effectiveModel
		payload["effort"] = string(reply.EffortApplied)
		payload["routing_reason"] = decision.Reason

		ref, err := s.conv.PublishFinding(agentCtx, req.DemandID, thread.ID,
			execution.Finding.Title, payload, ks["finding"])
		if err != nil {
			return nil, err
		}
		findingRef = &ref
	}

	return &TurnOutcome{
		DemandID: req.DemandID,
		ThreadID: thread.ID,
		Provider: info.Name,
		Routing: RoutingView{
			TaskKind:      decision.TaskKind,
			Class:         decision.Class,
			Model:         effectiveModel,
			Effort:        effort,
			EffortApplied: reply.EffortApplied,
			Reason:        decision.Reason,
			FromAgentCard: fromCard,
		},
		Reply:      execution.Reply,
		MessageIDs: ids,
		Concluded:  concluded,
		Finding:    findingRef,
		Usage: TurnUsage{
			// The sum of ALL the rounds, not the last: recording only the last
			// would underestimate an N-round turn's spend by a factor of N, and
			// a budget that is wrong is not a budget (ADR-0011 §2).
			Usage:              res.totalUsage,
			CostMicros:         res.totalCost,
			Currency:           res.currency,
			CacheCreationKnown: info.Supports(CapCacheCreationAccounting),
			CostKnown:          res.priceKnown,
		},
		ContextTruncated: notice != "",
		ToolRounds:       res.rounds,
		ToolCalls:        res.calls,
		LoopStop:         res.stop,
		MaxToolRounds:    roundCap,
		Paused:           accounting.BudgetExceeded,
		Notice:           budgetNotice(accounting),
		Budgets:          accounting.Exceeded,
		Warnings:         warnings,
	}, nil
}

// roundCap resolves THIS turn's cap: the service's, which the caller may LOWER
// but never raise. See TurnRequest.MaxToolRounds.
func (s *Service) roundCap(requested int) int {
	limit := s.maxToolRounds
	if limit <= 0 {
		// A service assembled through a path that did not go through the
		// constructor (a zero value, a test double): the domain's default
		// applies anyway. "No cap" is not a state this service can have.
		limit = DefaultMaxToolRounds
	}
	if requested > 0 && requested < limit {
		limit = requested
	}
	return limit
}

// modelAndEffort resolves (concrete model, effort, did the card win?).
//
// The rule, in three steps:
//
//  1. THE THREAD'S CARD first (ADR-0010 §2). Its name passes through INTACT: it
//     is a name from the agent integrations' menu, not a class, and translating
//     it would undo the thread's frozen choice;
//  2. otherwise, CLASS → THIS provider's catalog. It is the half the policy
//     cannot do, because the strong class changes name with every provider;
//  3. otherwise, the name the router returned, INTACT. That happens when the
//     decision brought no class — and guessing an unknown name's class would
//     silently swap the model the policy chose, which is the defect the late
//     `catalog.py` carried by design.
func modelAndEffort(info ProviderInfo, d Decision, card AgentCard) (string, Effort, bool) {
	fromCard := strings.TrimSpace(card.Model) != ""

	model := strings.TrimSpace(card.Model)
	switch {
	case fromCard:
	case d.Class == ClassCheap || d.Class == ClassMedium || d.Class == ClassStrong:
		model = info.ResolveModel(d.Class)
	default:
		model = d.Model
	}

	// The card's effort beats the router's when it declares a valid one. A
	// value outside the vocabulary falls back to high (see NormalizeEffort),
	// never to low.
	effort := d.Effort
	if e := Effort(strings.ToLower(strings.TrimSpace(card.Effort))); ValidEffort(e) {
		effort = e
	}
	return model, NormalizeEffort(effort), fromCard
}

// budgetNotice drafts the item the attention box shows.
//
// The sentence is assembled here, and not in the cost domain, because it is
// about THE TURN: what happened, what still holds and what the human needs to
// decide. The cost domain answers with facts (which scopes blew); translating a
// fact into a decision is the job of whoever knows the flow.
func budgetNotice(a Accounting) string {
	if !a.BudgetExceeded {
		return ""
	}
	scopes := make([]string, 0, len(a.Exceeded))
	for _, b := range a.Exceeded {
		scopes = append(scopes, fmt.Sprintf("%s %s (%d of %d micros %s)",
			b.Scope, b.ScopeID, b.SpentMicros, b.LimitMicros, b.Currency))
	}
	return "Budget blown on " + strings.Join(scopes, "; ") +
		". This turn was delivered whole; the next one does not go out until somebody decides " +
		"(raise the ceiling, cut scope or close) — ADR-0011 §2."
}
