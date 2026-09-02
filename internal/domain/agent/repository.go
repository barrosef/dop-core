package agent

import "context"

// ════════════════════════════════════════════════════════════════════════════
// THE RUNTIME'S PORTS
//
// This domain has no persistence of its own: a turn keeps no state, it PRODUCES
// state in the neighbouring domains (message, consumption, finding). What this
// file declares are two families of port:
//
//   - AgentProvider, the INFRASTRUCTURE port — the model provider, with one
//     adapter per provider in internal/adapter/agentprovider;
//   - Knowledge, Routing and Conversation, the NARROW ports to the neighbouring
//     domains. The runtime imports neither `knowledge`, nor `cost`, nor
//     `demand`: it says what it needs, in its own vocabulary, and the
//     composition root wires it. It is the same choice as `resource.Access`
//     over `identity.Service`, and its price is the glue in internal/app — what
//     it buys is that changing a neighbour's internal shape does not break the
//     runtime.
// ════════════════════════════════════════════════════════════════════════════

// ── the provider port ────────────────────────────────────────────────────────

// AgentProvider is the model provider. One adapter per provider, all under THIS
// contract.
//
// A hard consequence, and the reason this package's vocabulary exists: nothing
// above this port may see a provider type. No API response struct, no raw
// `map[string]any`, no SDK field name. The turn's cycle (service.go) talks only
// to entity.go's types; changing provider is changing the adapter.
//
// GUARANTEES verified by the contract suite, in EVERY adapter:
//
//  1. Info() does no I/O and is stable: same instance, same data sheet. It is a
//     sheet, not a query — the edge displays it and the report audits it
//     without spending network;
//
//  2. Info().ResolveModel returns a concrete and NON-EMPTY name for the three
//     classes, and is PURE: same class, same name, no I/O and no clock;
//
//  3. Render puts the stable prefix BEFORE the messages in the SERIALIZED
//     request. That is ADR-0012 §1's saving, and it breaks without a sound: a
//     prefix that went to the end of the body is not wrong, it just costs 10×
//     and nobody sees it;
//
//  4. Render is DETERMINISTIC: same Turn, same bytes. A map iterated in random
//     order, a timestamp or a request id in the body invalidates the cached
//     prefix every turn — the most silent invalidator there is;
//
//  5. the operator message is NEVER serialized as the user speaking (D3). When
//     the model has no channel of its own, the adapter may fall back to a
//     MARKED block inside the user's turn — and in that case it returns a
//     warning. A fallback with no warning is the same thing as an injection
//     with our signature;
//
//  6. Send returns Usage with the four parts DISJOINT (D2), whatever the
//     provider's semantics. Whoever includes cached tokens in the total
//     subtracts them;
//
//  7. StopReason is always in the domain's vocabulary (D5). A new provider
//     reason becomes StopUnknown, never an error;
//
//  8. EffortApplied never CLAIMS more than the provider applied (D4). When
//     there was a downgrade, there is a readable warning in Warnings;
//
//  9. a missing credential, a refused credential, a network outage and a
//     provider error all become *Unavailable with the right reason — never a
//     raw transport error and never an internal error of ours (D6);
//
//  10. the CREDENTIAL does not appear in Error(), in Render(), nor in the
//     adapter's own formatting (`%v`, `%+v`). It is the guarantee whose failure
//     costs the whole bill: an agent key in a log is a key at rest;
//
//  11. whoever announces CapExplicitPrefixCache MARKS the breakpoint in the
//     body, and the marker sits at the END OF THE PREFIX — not at the end of
//     the prompt. The three cache guarantees (3, 4 and this one) are
//     independent: you can get the order right, get determinism right and still
//     request no cache at all. None of the three fails with an error; all three
//     fail with an invoice;
//
//  12. whoever announces CapStructuredOutput sends `Turn.OutputSchema` ON THE
//     WIRE. Decoding happens on our side and keeps working as long as the model
//     cooperates — which makes the schema's absence invisible until the day it
//     does not cooperate (ADR-0012 §2);
//
//  13. `Turn.MaxOutputTokens` reaches the provider. A ceiling the adapter picks
//     on its own cuts the response at a limit nobody asked for, and the cut
//     comes out as a normal StopReason, with nothing to explain it;
//
//  14. `Send` sends exactly what `Render` shows. Without that, `Render` is a
//     shop window next to a different dispatch, and guarantees 3, 5, 11, 12 and
//     13 — all audited over `Render` — would be looking at the wrong place.
//
// ── TOOLS: the guarantees that hold the loop up (D7–D11) ────────────────────
//
//  15. whoever announces CapToolUse puts `Turn.Tools` ON THE WIRE, and the
//     declaration comes BEFORE the volatile text in the serialized body. A
//     declaration is prefix: it changes per thread, not per turn, and shoving
//     it after the conversation would take it out of the cacheable stretch by
//     guarantee 3's very mechanism — with no error at all, only an invoice;
//
//  16. a turn WITHOUT tools does not send the field. An empty array is
//     different from an absent field: it takes up room in the prompt and
//     invites the model to call what does not exist;
//
//  17. the call arrives NORMALIZED in `Reply.ToolCalls`: opaque id, name, and
//     `Input` as a MAP — whatever the provider's dialect (an object at
//     Anthropic, a string to decode at OpenAI — D8). And the ORDER is the one
//     the provider emitted (D10);
//
//  18. an UNREADABLE argument does not bring the turn down. The call comes up
//     with a nil `Input`, `RawInput` filled in and a readable warning. The one
//     who fixes the argument is the model, and it only fixes it if it gets the
//     error back (D8);
//
//  19. `StopReason` is `StopToolUse` when the provider asked for a tool — in
//     both, under the native names each one uses (D5);
//
//  20. the result goes back TIED to the call: the call's id appears in the
//     serialized body, and the result's content is NEVER attributed to the
//     user. It is guarantee 5's rule, for the inverse reason: an operator
//     instruction must not become data, and a tool's output — which is
//     untrusted content, substrate spec §6 — must not become an instruction;
//
//  21. a result marked as an ERROR reaches the model readably. Where the
//     provider has the boolean, it is used; where it does not, the marker goes
//     in the text (D9). Losing the marker makes the model read a failure as
//     normal output and carry on asserting the opposite of what happened.
//
// ── WHAT WAS LEFT OUT, EXPLICITLY ───────────────────────────────────────────
//
// This project's rule for ports: what is not deliverable by ALL adapters does
// not get in — because a port only one provider honours is that provider under
// another name.
//
//   - FORCED TOOL CHOICE (`tool_choice`). Both have "auto", "any" and "force a
//     specific tool", in different shapes — but turning parallelism off lives
//     INSIDE that field in one of them and in a top-level field in the other
//     (D10), and neither use case has come up. The loop works with the default,
//     which is what we want: several calls per round is one round fewer, and a
//     round resends the whole conversation;
//
//   - SERVER TOOLS (web search, the provider's code execution). They run on
//     THEIR infrastructure, are billed separately and do not go through the
//     sandbox — the opposite of this delivery's entire point, which is the
//     agent acting inside the demand's isolated sandbox (substrate spec §1);
//
//   - STREAMING. The platform's live follow-along is the event log (ADR-0006):
//     the published message BECOMES an event and reaches the cockpit through
//     the WatchDemand that already exists. A second streaming path here would
//     be a second source of truth for the same timeline;
//
//   - CONTEXT WINDOW and compaction. Each provider has its own, and ADR-0012 §3
//     already decided that resumption is by RECONSTRUCTION (package +
//     findings), not by replaying the transcript. Compaction is a safety net
//     for a continuous session — the adapter's business, not the port's;
//
//   - RETRY. Whoever decides to try again is whoever knows if it is still worth
//     spending: ADR-0011 §2's budget and the human in the attention box. A
//     retry hidden in the adapter would spend twice and report once.
type AgentProvider interface {
	// Info is the data sheet: name, catalog, capabilities and prices. No I/O.
	Info() ProviderInfo

	// Render assembles the provider's request WITHOUT sending it, already
	// serialized.
	//
	// It returns BYTES, and not a map, for two reasons that justify the public
	// method. The first is auditing: it is how the contract suite verifies the
	// prefix's ORDER in ANY provider, looking for the two slices in the real
	// body without knowing anyone's format — a remarshalled map would come out
	// with its keys in alphabetical order and erase exactly what is being
	// verified. The second is that it allows recording what WOULD be sent
	// without spending a call.
	//
	// The warnings returned are the assembly's (effort downgraded, operator
	// channel backed off) and travel to the `Reply`.
	Render(t Turn, model string, effort Effort) (body []byte, warnings []string, err error)

	// Send sends the conversation. It ALWAYS fails as *Unavailable
	// (guarantee 9).
	Send(ctx context.Context, t Turn, model string, effort Effort) (*Reply, error)
}

// Providers resolves WHICH adapter serves this call, credential included.
//
// The choice is PER REQUEST, not at boot: an agent provider is an
// `agent`-category resource (ADR-0013), chosen per account and per project, and
// several accounts coexist in the same process. There is no "the adapter"
// assembled at boot, as happens with the SecretStore.
//
// What this port hides is ADR-0023's entire point: whoever implements it
// (internal/app/agentproviders.go) reads the credential from the vault — in the
// core, in the same process — and hands over a ready adapter. This domain does
// not know `ports.SecretStore`, does not receive a vault as a parameter and does
// not know a vault exists. The credential crosses no boundary, neither of
// network nor of package.
//
// An empty resourceID means "this account's default provider". An UNKNOWN
// provider is an explicit refusal, never a silent fallback to another one:
// falling back to another provider without warning would change the model, the
// price and the cache semantics of a whole demand, and the only place it would
// show up is the invoice.
type Providers interface {
	For(ctx context.Context, resourceID string) (AgentProvider, error)
}

// ── the substrate port ───────────────────────────────────────────────────────

// SandboxCommand is ONE command to run in the demand's sandbox, in the
// runtime's vocabulary.
//
// Note what is NOT here, and it is `ports.ExecRequest`'s same absence: there is
// no environment, no directory, no credential. **It is not discipline, it is an
// absent field** — there is no way for a secret to enter the sandbox through
// this path. The sandbox runs agent code, which reads untrusted content
// (substrate spec §6): the model provider's key lives in the vault, is read by
// the composition root and used in the SAME process (ADR-0023), and this struct
// is the boundary that guarantees it goes no further than that.
type SandboxCommand struct {
	Command        []string
	TimeoutSeconds int
	MaxOutputBytes int
}

// SandboxOutput is what the command produced. No error field, and on purpose: a
// command that fails is a RESULT.
type SandboxOutput struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
	TimedOut  bool
}

// Failed answers the question the loop asks: was this a failure?
//
// Code -1 means "there was no code" (a hung process, a substrate that could not
// say) and counts as a failure: the model needs to treat "I do not know whether
// it finished" as a problem, not as a success.
func (o SandboxOutput) Failed() bool { return o.ExitCode != 0 || o.TimedOut }

// Sandbox is the NARROW port to the execution substrate: ONE operation.
//
// It is the bridge between the agent and the sandbox, and it is this service's
// only OPTIONAL port (see NewService). Without it, the agent converses; with it,
// it acts. The optionality is real and is not laxity: an installation with no
// substrate wired keeps running turns — what it may NOT do is grant tools on the
// card and watch the agent stay quiet about it, which is why the absence becomes
// a readable warning in the turn's result, never silence.
//
// What this port hides: which sandbox serves the demand, whether it is running,
// whether it exists. The runtime asks by DEMAND — which is its vocabulary — and
// resolving demand → sandbox is the execution domain's job, on the other side of
// the glue.
//
// THE ERROR CONTRACT, and it is the most important thing here:
//
//   - a command that exits with a code != 0, that blows the deadline or whose
//     output was cut is a SUCCESS of this port, with the facts in
//     `SandboxOutput`. The model needs to SEE that the command failed in order
//     to fix it;
//   - an error is reserved for the SUBSTRATE: a nonexistent sandbox, a
//     suspended one, a cluster that is down. The loop tells the two apart by
//     `errs.Kind` (see toolloop.go) and only the second kills the turn.
type Sandbox interface {
	RunCommand(ctx context.Context, demandID string, cmd SandboxCommand) (SandboxOutput, error)
}

// ── narrow ports to the neighbours ───────────────────────────────────────────

// ContextArtifact is an artifact of the context package, with the MINIMUM the
// prompt assembly consumes.
//
// Note what is NOT here: `id` and `version`. They change when the core rewrites
// the artifact without the content changing, and they would enter the cached
// prefix and invalidate it for nothing (ADR-0012 §1). Leaving them out of the
// port is stronger than remembering not to use them.
type ContextArtifact struct {
	Name      string
	Body      string
	ObjectRef string
}

// ContextFinding is a finding already published on the demand, as it enters the
// prompt.
type ContextFinding struct {
	Title   string
	Summary string
}

// ContextDropped is what was left OUT of the package for want of token budget.
//
// It is first-class information (ADR-0012), not a detail: without it the agent
// asserts things about what it did not read, and the human reads a wrong
// conclusion nobody can explain afterwards. That is why it appears in TWO
// places — in the prefix, speaking to the agent, and in the thread, speaking to
// the human.
type ContextDropped struct {
	Rules    int
	Findings int
	Index    int
	Memories int
}

func (d ContextDropped) Any() bool { return d.Rules+d.Findings+d.Index+d.Memories > 0 }

// ContextPackage is the agent's carry-on luggage (ADR-0009 §3), in the
// runtime's vocabulary. The lists' ORDER is the core's curation and is
// PRIORITY — reordering here would undo the selection that cost the whole
// budget.
type ContextPackage struct {
	Rules    []string
	Index    []ContextArtifact
	Memories []ContextArtifact
	Findings []ContextFinding
	Dropped  ContextDropped
}

func (p ContextPackage) Truncated() bool { return p.Dropped.Any() }

// Knowledge is the narrow port to the knowledge domain: a single question.
type Knowledge interface {
	ContextPackage(ctx context.Context, demandID string) (ContextPackage, error)
}

// Decision is the cost domain's routing decision, with the CLASS alongside.
//
// The class is the field the network boundary used to eat. While the runtime
// lived in the BFF, `dop.v1.RoutingDecision` carried only the name from the
// core's catalog, and the other side needed a table (the extinct `catalog.py`)
// to undo the name → class path and then ask the ACTIVE provider for ITS name.
// In-process the class arrives whole, that table does not exist, and the risk it
// carried — guessing an unknown name's class and silently swapping the model the
// core chose — went with it.
//
// Reason travels WHOLE: it is what allows auditing "why did this demand run on
// the expensive model?" without opening the code (ADR-0011 §3).
type Decision struct {
	TaskKind string
	Class    ModelClass
	Model    string
	Effort   Effort
	Reason   string
}

// Consumption is a consumption to record, in the runtime's vocabulary.
type Consumption struct {
	DemandID            string
	ThreadID            string
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	CostMicros          Micros
	Currency            string
}

// BudgetView is a blown ceiling, as the attention box shows it.
type BudgetView struct {
	Scope       string
	ScopeID     string
	LimitMicros Micros
	SpentMicros Micros
	Currency    string
}

// Accounting is what the cost domain answered to the record.
//
// Exceeded is STATE, not a transition — repeating a call returns the original's
// same warning, because whoever asks "may I go on?" needs the right answer even
// when the write did not happen again.
type Accounting struct {
	BudgetExceeded bool
	Exceeded       []BudgetView
}

// Routing is the narrow port to the cost domain: decide and measure.
//
// The two operations live in the same port because they are the two halves of
// the same fact — choosing how much to spend and recording how much was spent.
// Separating them would give the composition root two pieces of glue for the
// same service with nobody gaining anything.
type Routing interface {
	Route(ctx context.Context, taskKind, demandID string) (Decision, error)
	// RecordUsage REQUIRES an idempotency key: a duplicate consumption
	// collides with nothing, would enter as legitimate spend and the budget
	// would become fiction (it is the same requirement `cost.Service.RecordUsage`
	// makes).
	RecordUsage(ctx context.Context, c Consumption, idemKey string) (Accounting, error)
}

// AgentCard is the thread's card (ADR-0010 §2), in the runtime's vocabulary.
//
// It enters the PREFIX because it is stable per thread: purpose and granted
// tools do not change every turn. And when it declares a model, it BEATS the
// router — the card is that thread's frozen contract, and changing its model
// midway would invalidate the cached prefix of every previous turn, because
// cache is per model (ADR-0012 §1).
type AgentCard struct {
	Purpose      string
	Tools        []string
	Model        string
	Effort       string
	BudgetMicros int64
}

// Thread is the minimum the runtime needs to know about the conversation: what
// it is called and which card it was born with.
type Thread struct {
	ID   string
	Key  string
	Card AgentCard
}

// FindingRef is the published finding, as a reference.
type FindingRef struct {
	ID    string
	Title string
}

// Conversation is the narrow port to the demand domain.
//
// PostMessage returns only the id: the runtime publishes and moves on, it does
// not re-read what it wrote. And AUTHORSHIP is not a parameter here on purpose —
// it comes from the call context (`ctxutil.Call`), as everywhere else in the
// core. It is the service that swaps the actor for the AGENT before publishing
// the reply; see service.go.
type Conversation interface {
	Thread(ctx context.Context, demandID, threadID string) (Thread, error)
	PostMessage(ctx context.Context, threadID, text, idemKey string) (string, error)
	PublishFinding(ctx context.Context, demandID, threadID, title string,
		payload map[string]any, idemKey string) (FindingRef, error)
}
