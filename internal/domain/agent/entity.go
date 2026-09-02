// Package agent is the agent RUNTIME: the conversation turn with the model, from
// context to published finding (ADR-0023).
//
// It lived in the BFF (ADR-0016) and came back here for one security reason and
// one design reason. The security one: the runtime needs the provider's
// credential, which lives in the vault, in the core, and the core NEVER returns a
// secret — giving the BFF the vault was vetoed, because it is the layer exposed to
// the internet. With the runtime here, the credential does not cross the network.
// The design one: the six things a turn does — assemble context, route the model,
// record consumption, post a message, publish a finding, respect the budget — were
// all already core operations, performed from outside over gRPC. The runtime was
// on the wrong side of the boundary.
//
// House rule, holding here as in every domain: this package knows no Postgres, no
// gRPC, no vendor SDK and no `ports.SecretStore`. It declares what it needs as a
// PORT (repository.go) and the composition root wires it. In particular: it does
// not know `knowledge`, `cost` or `demand` — it talks to all three through narrow
// ports declared here, in ITS vocabulary.
//
// Two modules of the Python implementation did NOT cross over, and ADR-0023
// explains why: `credentials.py` existed to work around an inaccessible vault
// (here the credential comes from `ports.SecretStore` at the composition root, as
// with git — see internal/app/agentproviders.go), and `catalog.py` existed
// retracing the path from the model's NAME back to its CLASS, because the routing
// decision crossed the network and lost the class on the way. In-process,
// `Decision.Class` arrives whole and the way back ceases to exist.
//
// ════════════════════════════════════════════════════════════════════════════
// WHERE THE PROVIDERS DIVERGE  (the real work — ADR-0022)
// ════════════════════════════════════════════════════════════════════════════
//
// Every divergence below has its own subtest in test/contract/agentprovider.go,
// running against ALL adapters. They changed language, not design.
//
// D1 — PREFIX CACHE SEMANTICS. The most expensive one, because ADR-0012 depends on
// it. Anthropic has EXPLICIT caching: a `cache_control` marks the end of the
// prefix, a 5-minute TTL, reads at ~0.1× the input and writes at 1.25×, and the
// response separates `cache_read_input_tokens` from `cache_creation_input_tokens`.
// OpenAI has AUTOMATIC caching: no breakpoint, no controllable TTL, and the
// response reports only `cached_tokens` (reads) — cache CREATION is not reported.
// So `Usage.CacheCreationTokens` is always 0 in the OpenAI adapter, and that does
// NOT mean "nothing was written to the cache": it means "it cannot be known". The
// silent-invalidator alert of ADR-0012 §1 (zero cache reads on a stable prefix —
// see `cost.UsageEvent.SuspectCacheMiss`) is only FAITHFUL under a provider with
// explicit accounting. `CapCacheCreationAccounting` says who has it, and the
// capability travels in the `Reply` so whoever reads the telemetry knows what the
// number means instead of discovering it at reconciliation.
//
// D2 — TOKEN COUNTING: THE DOUBLE-COUNTING TRAP. At Anthropic, `input_tokens`
// EXCLUDES what came from cache — the three input parts are disjoint and sum to
// the total. At OpenAI, `prompt_tokens` INCLUDES the cached ones and
// `cached_tokens` is a SUBSET of it. Summing OpenAI's fields as if they were
// disjoint inflates ADR-0011's measurement without anything failing. The port
// NORMALIZES to the disjoint semantics, which is what `cost.UsageEvent` assumes:
// the OpenAI adapter subtracts, and that is the most important line in that file.
//
// D3 — THE OPERATOR CHANNEL. The operator's intervention has to enter the middle
// of the conversation without rewriting the top of the prompt (ADR-0012 §1).
// Anthropic has a `role:"system"` message inside `messages` — on the Opus 5/4.8
// and Fable/Mythos models, and NOT on Sonnet 5, which answers 400. OpenAI has
// `role:"developer"`, accepted in any position. The port exposes `RoleOperator`
// and each adapter resolves it — including the FALLBACK to a marked block inside
// the user's turn when the model refuses the channel, always with a warning. What
// the port GUARANTEES is that the operator's instruction is never attributed to
// the user: it is what authorizes, and flattening the two opens the door to prompt
// injection.
//
// D4 — EFFORT. The core's vocabulary is `low|medium|high|xhigh|max` (ADR-0011 §3,
// mirrored in `cost.Effort`). Anthropic accepts all five; OpenAI accepts three.
// The adapter MAPS and declares what it applied in `Reply.EffortApplied` — it never
// pretends it applied `max`. On a critical task (ADR-0007: you do not save on the
// critic) that difference is a product decision, not a detail, and that is why it
// also comes out as a readable warning in `Reply.Warnings`.
//
// D5 — STOP REASON. Anthropic: `end_turn | max_tokens | stop_sequence | tool_use |
// pause_turn | refusal | model_context_window_exceeded`. OpenAI: `stop | length |
// tool_calls | content_filter`. Normalized into `StopReason` — the domain needs to
// tell apart "it finished", "it was cut" and "it refused", and only that.
//
// D6 — UNAVAILABILITY. The two error families look nothing alike. Both become
// `*Unavailable` (see below), because a third party's unavailability must not
// reach the client as an error of ours: a generic internal error sends the wrong
// team to investigate and sends the user to wait for a fix that does not exist.
//
// ════════════════════════════════════════════════════════════════════════════
// TOOLS: THE FIVE AXES ON WHICH THE TWO DIVERGE  (D7–D11)
// ════════════════════════════════════════════════════════════════════════════
//
// Tools stayed OUT of the port in the previous delivery, and the recorded reason
// was a good one: "the formats diverge on three axes at once, and a loop written
// over the average of the two would be a loop neither executes well". The reason
// still holds — what changed is that the axes were named one by one, each gained a
// contract subtest in BOTH adapters, and the normalization stopped being an
// average and became a translation. There were three; looked at closely, there are
// five.
//
// D7 — DECLARATION. Anthropic puts the schema AT THE TOP of the tool object:
// `{name, description, input_schema}`. OpenAI nests it: `{type:"function",
// function:{name, description, parameters, strict}}` — a different name for the
// same field (`parameters`, not `input_schema`), one more level of nesting and a
// `type` key that exists only there. The common ground is NAME + DESCRIPTION +
// SCHEMA, and that is exactly what `ToolSpec` carries; each adapter builds its own
// shell. A consequence worth recording: the declaration is PREFIX — it changes per
// thread, not per turn — and so it goes BEFORE the messages in the body, the same
// way the stable prefix does (the port's guarantee 15).
//
// D8 — THE CALL IN THE RESPONSE. Here the difference is not of name, it is of
// TYPE. At Anthropic the call is a `tool_use` block INSIDE `content`, next to the
// text blocks, and `input` is already a decoded JSON OBJECT. At OpenAI the call
// comes in a `tool_calls` array OUTSIDE `content`, and `function.arguments` is a
// STRING that still needs `json.Unmarshal`. The trap is the degenerate case: the
// model can emit a string that is not valid JSON. Failing the turn there would be
// the wrong answer — the one who can fix the argument is the MODEL, and it only
// fixes it if it gets the error back. So the port sends the call up with a nil
// `Input` and a warning, and the loop answers with an error result. It is the exact
// twin of D2's rule: the adapter absorbs the provider's shape and delivers the
// domain's semantics.
//
// D9 — THE RESULT GOING BACK. Anthropic receives the result as a `tool_result`
// block inside a `role:"user"` message — several results fit in the SAME message,
// and there is an `is_error` boolean. OpenAI receives ONE `role:"tool"` message PER
// result, with a `tool_call_id`, and has NO error field: a tool failure is text
// like any other. Two consequences: the number of messages in the body differs
// between providers for the same domain turn (which is why the suite compares
// content, never counts), and where the boolean does not exist the adapter
// PREFIXES the content readably — losing the error marker would make the model read
// a failure as normal output and carry on.
//
// D10 — PARALLELISM. Both emit several calls per round, and by default. Anthropic
// turns it off through `tool_choice.disable_parallel_tool_use`; OpenAI through a
// top-level field, `parallel_tool_calls`. The default is what we want — several
// calls in one round is one round fewer, and a round costs the whole turn resent —
// and neither adapter sends the flag. What the port guarantees is the ORDER: the
// calls reach `Reply.ToolCalls` in the order the provider emitted them, and the
// results go back in the calls' order. Reordering is what turns "I ran the test and
// then read the log" into "I read the log and then ran the test" in the model's
// reading.
//
// D11 — UNDECLARED TOOL. Neither prevents the model from calling a name that was
// not declared — both validate the BODY we send, not the model's imagination. And
// both refuse the inverse with a 400: a result whose call id they do not know (an
// orphan `tool_use_id` at Anthropic, a `tool_call_id` with no pair at OpenAI).
// Hence the loop's two rules, which are symmetric: a call to an unknown tool
// becomes an ERROR RESULT carrying the id the provider sent — never a turn error,
// never a silent result — and no result is invented without a call to justify it.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── domain vocabulary ────────────────────────────────────────────────────────

// ModelClass is a model CLASS, not a name — it mirrors `cost.ModelClass`.
//
// Model names change leader every six months (ADR-0001) and do not survive a
// policy table; the class is what the decision MEANS. Resolving class → concrete
// name is the CATALOG's job, and the catalog is PER PROVIDER: Anthropic's strong
// class and OpenAI's are different models, with different prices and limits, and
// `cost/router.go`'s policy should not even know that.
type ModelClass string

const (
	ClassCheap  ModelClass = "cheap"
	ClassMedium ModelClass = "medium"
	ClassStrong ModelClass = "strong"
)

// Effort is the reasoning effort, in the core's vocabulary (ADR-0011 §3). The
// five values are the same as `cost.Effort` — string for string, on purpose: the
// glue between the two domains is a named-type swap, not a translation, and on
// the day they diverge it is better that it breaks at the glue.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// ValidEffort answers whether the value is in the vocabulary. A value from
// outside does NOT stop work — see NormalizeEffort.
func ValidEffort(e Effort) bool {
	switch e {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return true
	}
	return false
}

// NormalizeEffort falls back to HIGH when it does not recognize the value, not
// to low: ADR-0011 §3 already decided that, in doubt, you do not save — erring
// towards expensive shows up in the measurement, erring towards cheap shows up
// in rework, which shows up nowhere.
func NormalizeEffort(e Effort) Effort {
	if ValidEffort(e) {
		return e
	}
	return EffortHigh
}

// Role is who speaks. `RoleOperator` is a channel of its OWN, and the
// distinction is a security one: an operator instruction carries authority, a
// user's text does not. Flattening the two into a single role is the classic
// prompt-injection path — whoever writes into a user input gains the ability to
// forge an instruction. See D3.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleOperator  Role = "operator"
	// RoleToolResult is what the TOOL returned. A role of its own, and not the
	// user's, for RoleOperator's security reason inverted: a command's output
	// is UNTRUSTED content (substrate spec §6) and must not reach the model
	// with the authority of whoever asked for the work. A malicious `README`
	// read by `cat` cannot become an instruction.
	RoleToolResult Role = "tool_result"
)

// StopReason is why the model stopped, in the DOMAIN's vocabulary (see D5).
//
// Four things the domain needs to tell apart: it finished, it was cut, it
// refused and it ASKED FOR A TOOL. The fourth existed here before tools entered
// the port — it was written so that the day they entered would not change this
// vocabulary, and that day arrived without changing it. `StopUnknown` exists so
// that a new provider reason does not become an error — stopping work because of
// an unknown string would be worse than recording that it appeared.
type StopReason string

const (
	StopCompleted StopReason = "completed"
	StopMaxTokens StopReason = "max_tokens"
	StopRefused   StopReason = "refused"
	StopToolUse   StopReason = "tool_use"
	StopUnknown   StopReason = "unknown"
)

// Capability is what THIS adapter can deliver — the divergences, as DATA. It
// exists so whoever reads the telemetry knows what the number means.
type Capability string

const (
	// CapExplicitPrefixCache: the cached prefix is marked explicitly
	// (breakpoint), not guessed by the provider.
	CapExplicitPrefixCache Capability = "explicit_prefix_cache"
	// CapCacheCreationAccounting: the response separates cache CREATION from
	// cache READS (ADR-0012 §1). Without it, a zero in CacheCreationTokens
	// means "it cannot be known", never "nothing was written".
	CapCacheCreationAccounting Capability = "cache_creation_accounting"
	// CapOperatorChannel: an operator instruction has a channel of its own in
	// the provider's protocol (D3).
	CapOperatorChannel Capability = "operator_channel"
	// CapFullEffortRange: the five levels of ADR-0011 §3, with no downgrade.
	CapFullEffortRange Capability = "full_effort_range"
	// CapStructuredOutput: structured output validated by the provider —
	// ADR-0012 §2's terse finding, with no re-parse on our side.
	CapStructuredOutput Capability = "structured_output"
	// CapToolUse: the adapter declares tools, reads the call and returns the
	// result (D7–D11). BOTH providers have it — and it exists anyway, because
	// a capability is DATA: a third adapter that did not implement the loop
	// needs to be able to say so, instead of letting the runtime declare tools
	// nobody executes.
	CapToolUse Capability = "tool_use"
)

// Capabilities is the set, as an ordered slice and not as a map: it travels in
// the `Reply`, reaches logs and telemetry, and a Go map iterates randomly —
// output that changes order between runs is impossible to match in a test and
// annoying to read.
type Capabilities []Capability

func (c Capabilities) Has(want Capability) bool {
	for _, got := range c {
		if got == want {
			return true
		}
	}
	return false
}

// ── tools (D7–D11) ───────────────────────────────────────────────────────────

// ToolSpec is a tool's DECLARATION, on the two providers' common ground (D7):
// name, description and input schema.
//
// There is no field for each provider's shell — no `type:"function"`, no
// `strict` — on purpose: the day one of them shows up here, the port will have
// become one provider's format under another name.
type ToolSpec struct {
	Name        string
	Description string
	// InputSchema is JSON Schema. A map, not a struct, because the schema is
	// data from the tool catalog and not this port's vocabulary — and
	// `json.Marshal` orders map keys alphabetically, which keeps serialization
	// deterministic (guarantee 4).
	InputSchema map[string]any
}

// ToolCall is what the model ASKED FOR.
type ToolCall struct {
	// ID is opaque and comes from the provider. It is what ties the call to
	// the result (D11): inventing an id here would make both providers refuse
	// the next turn with a 400.
	ID   string
	Name string
	// Input is the argument ALREADY DECODED. Nil means the provider sent
	// something that does not decode — at OpenAI, a string that is not JSON
	// (D8). Nil is NOT "no arguments": `map[string]any{}` is that.
	Input map[string]any
	// RawInput is the argument as it came, for when `Input` is nil. It goes
	// into the error result returned to the model — without it, the model
	// would get "your argument is invalid" without knowing which argument.
	RawInput string
}

// ToolResult is what the execution RETURNED, ready to go back to the model.
type ToolResult struct {
	// CallID matches ToolCall.ID.
	CallID string
	Name   string
	// Content is what the model will READ. Text, always: both providers accept
	// text in the result, and only one accepts structure.
	Content string
	// IsError says the tool FAILED. A field of its own, and not a prefix in
	// the text, because one of the providers has the boolean natively —
	// throwing the distinction away to fit the smaller of the two would be the
	// common denominator ADR-0001 refuses. Whoever lacks the field writes the
	// marker into the text (D9).
	IsError bool
}

// ── the conversation ─────────────────────────────────────────────────────────

// Message is one utterance: the VOLATILE part of the conversation, which goes
// after the breakpoint.
//
// The three extra fields are exclusive per role, and that exclusivity is the
// contract: `ToolCalls` only on RoleAssistant, `ToolResults` only on
// RoleToolResult. One struct with all three instead of three types is a
// deliberate choice — the alternative (interface + type switch) would spread
// knowledge of the conversation's shape across every adapter, which is exactly
// what the port exists to prevent.
type Message struct {
	Role Role
	Text string
	// ToolCalls is what the model asked for on the PREVIOUS round, resent as
	// history. Without it, the provider receives a result with no call to
	// justify it and refuses the turn (D11).
	ToolCalls []ToolCall
	// ToolResults are those calls' results, in THEIR order (D10).
	ToolResults []ToolResult
}

// Micros is a monetary value in 10^-6 of the currency unit — it mirrors
// `cost.Micros`.
//
// Money is not a float here for the same arithmetic reason as there: summing
// millions of lines in floating point accumulates error, and a budget that is
// wrong is not a budget. Repeating the type — instead of importing `cost` — is
// the price of the runtime not knowing the cost domain; the glue converts, and
// the conversion is a named-type swap, not a calculation.
type Micros int64

// Usage is ONE call's consumption, with the four parts DISJOINT (see D2).
//
// Disjoint is the CONTRACT: InputTokens does not include what came from cache.
// `cost.UsageEvent` assumes that — `PromptTokens()` there sums the three input
// parts — and an adapter returning the inclusive count would inflate ADR-0011's
// budget silently.
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// Price is a model's price, in MICROS per 1,000 tokens, with the currency
// alongside.
//
// Per 1,000 and not per token: a cache read costs 0.1× the input, and a
// per-token price would become a fraction — which vanishes in integers and lies
// in floats. All the arithmetic here is integer, start to finish.
//
// Why the price lives in the ADAPTER: it is the one who knows which provider
// charged what. `cost.UsageEvent.CostMicros` is filled in by whoever called the
// model for exactly that reason — the cost domain does not recompute a third
// party's price, it records what was spent.
type Price struct {
	Currency           string
	InputPer1k         Micros
	OutputPer1k        Micros
	CacheReadPer1k     Micros
	CacheCreationPer1k Micros
}

// CostMicros is this call's cost. Integer division at the END, and only once:
// rounding part by part would throw away hundredths of a micro on every line,
// and the month's total would sit systematically below the invoice.
func (p Price) CostMicros(u Usage) Micros {
	total := Micros(u.InputTokens)*p.InputPer1k +
		Micros(u.OutputTokens)*p.OutputPer1k +
		Micros(u.CacheReadTokens)*p.CacheReadPer1k +
		Micros(u.CacheCreationTokens)*p.CacheCreationPer1k
	return total / 1000
}

// Turn is the conversation to send: stable prefix first, volatile after.
//
// The separation is ADR-0012 §1 turned into a TYPE. As long as StablePrefix is a
// field of its own, nobody interpolates the turn's text in there out of
// distraction — which is the worst way to lose the cache saving, because it does
// not fail, it just gets expensive.
type Turn struct {
	StablePrefix string
	Messages     []Message
	// OutputSchema is the response's JSON schema: a terse finding, validated
	// by the provider (ADR-0012 §2). Nil = free text.
	OutputSchema map[string]any
	// Tools are the tools DECLARED on this turn (D7). Empty = the agent only
	// converses, and the adapter does NOT send the field — an empty array in
	// the body is different from an absent field, and sending it would invite
	// the model to call what does not exist (guarantee 16).
	//
	// The order matters and comes from the CATALOG, already sorted by name
	// (see tools.go): the declaration is a stable part of the prompt, and a
	// list that changes order between turns invalidates the cached prefix just
	// like an iterated map does.
	Tools           []ToolSpec
	MaxOutputTokens int
}

// Fingerprint is the PREFIX's fingerprint, and only its.
//
// Two turns of the same thread have to yield the same fingerprint, even with
// different messages: that is what "the prefix is stable" means, and it is what
// the contract suite compares between turns.
func (t Turn) Fingerprint() string {
	h := sha256.Sum256([]byte(t.StablePrefix))
	return hex.EncodeToString(h[:])
}

// Reply is the response, in DOMAIN types. No provider object crosses from here
// upwards.
type Reply struct {
	Text       string
	Usage      Usage
	Model      string
	Provider   string
	StopReason StopReason
	// Data is the structured output already decoded, when there was an
	// OutputSchema.
	Data map[string]any
	// ToolCalls are the tools the model asked for, IN THE ORDER the provider
	// emitted them (D10). Empty when it only spoke.
	ToolCalls []ToolCall
	// EffortApplied is the effort ACTUALLY applied. It may be lower than the
	// one requested — see D4. It is never the requested one "out of politeness".
	EffortApplied Effort
	// Capabilities is what this provider delivers. It travels along so the
	// telemetry is readable without consulting the adapter's code.
	Capabilities Capabilities
	// Warnings are readable warnings: effort downgraded, operator channel
	// backed off, cache accounting absent.
	Warnings []string
}

// ProviderInfo is the adapter's data sheet — what the edge shows and the report
// audits. No I/O: it is a sheet, not a query.
type ProviderInfo struct {
	Name         string
	Catalog      map[ModelClass]string
	Capabilities Capabilities
	// Prices is by CONCRETE model name. An absent model means the price is
	// UNKNOWN, and unknown does not silently become zero — see PriceFor.
	Prices map[string]Price
}

// ResolveModel translates CLASS → concrete name through THIS provider's catalog.
//
// A class outside the catalog does not stop work: it falls back to the strong
// class. It is the same choice as the cost domain's `Router.Route` — in doubt you
// do not save, because the spend shows up in the measurement and the rework does
// not.
func (p ProviderInfo) ResolveModel(c ModelClass) string {
	if name, ok := p.Catalog[c]; ok && name != "" {
		return name
	}
	return p.Catalog[ClassStrong]
}

func (p ProviderInfo) Supports(c Capability) bool { return p.Capabilities.Has(c) }

// PriceFor returns this model's price and whether it is KNOWN.
//
// The boolean, and not a zeroed price: zero would assert the call was free, and
// a budget fed with zeros is exactly the fiction ADR-0011 §2 exists to prevent.
// Whoever gets `false` has to SAY it does not know — which is what the turn's
// cycle does, with a readable warning in the output.
func (p ProviderInfo) PriceFor(model string) (Price, bool) {
	pr, ok := p.Prices[model]
	return pr, ok
}

// ── provider unavailability (D6) ─────────────────────────────────────────────

// UnavailableReason is why the provider did not answer. A CLOSED vocabulary on
// purpose: the client decides what to do FROM HERE, and an open vocabulary would
// become `strings.Contains` scattered across three consumers.
type UnavailableReason string

const (
	// ReasonMissingCredential: there is no credential configured for this
	// `agent`-category resource (ADR-0013).
	ReasonMissingCredential UnavailableReason = "missing_credential"
	// ReasonRejectedCredential: the credential exists and was REFUSED
	// (401/403).
	ReasonRejectedCredential UnavailableReason = "rejected_credential"
	// ReasonUnreachable: network — DNS, deadline exceeded, connection
	// refused, egress blocked.
	ReasonUnreachable UnavailableReason = "unreachable"
	// ReasonProviderError: the provider answered with an error (5xx,
	// overload, rate limit).
	ReasonProviderError UnavailableReason = "provider_error"
	// ReasonUnknownModel: the routed model does not exist in the provider's
	// catalog.
	ReasonUnknownModel UnavailableReason = "unknown_model"
	// ReasonUnknownProvider: there is no adapter for this provider in this
	// installation.
	ReasonUnknownProvider UnavailableReason = "unknown_provider"
)

// guidance is what the human DOES about each reason. Written once, here: three
// consumers drafting the same guidance is how two of them get it wrong.
var guidance = map[UnavailableReason]string{
	ReasonMissingCredential: "configure the credential of the 'agent'-category resource " +
		"(ADR-0013) — it is read from the vault, in the core, and never travels",
	ReasonRejectedCredential: "the provider credential was refused; renew it",
	ReasonUnreachable:        "the agent provider did not answer; check the network and the egress allowlist",
	ReasonProviderError:      "the agent provider failed; try again later",
	ReasonUnknownModel: "the routed model does not exist in this provider's catalog; " +
		"adjust the adapter's catalog or the routing policy (ADR-0011 §3)",
	ReasonUnknownProvider: "there is no adapter for this agent provider in this installation",
}

// Unavailable is the PROVIDER's unavailability — distinguishable from an error
// of ours.
//
// It carries three machine-readable things: WHO (the provider), WHY (the reason,
// a closed vocabulary) and what the human should do. And `Error()` NEVER includes
// the provider's raw message: an API error body carries URLs, headers and, in
// some cases, a prefix of the key. The raw detail stays in `detail`, an
// UNEXPORTED field that only comes out through `Detail()` — whoever wants to log
// it has to ask.
type Unavailable struct {
	Provider string
	Reason   UnavailableReason
	detail   string
}

// Unavailability builds the error. `detail` comes in as a parameter and not as a
// public field so nobody embeds it in a message by mistake while constructing.
func Unavailability(provider string, reason UnavailableReason, detail string) *Unavailable {
	return &Unavailable{Provider: provider, Reason: reason, detail: detail}
}

func (e *Unavailable) Error() string {
	return fmt.Sprintf("agent provider %q unavailable (%s): %s",
		e.Provider, e.Reason, guidance[e.Reason])
}

// Detail is the provider's RAW detail, for logging. Outside `Error()` on
// purpose: what goes up to the client is the sentence above, always the same.
func (e *Unavailable) Detail() string { return e.detail }

// kind translates the reason into the house's error vocabulary.
//
// The translation is not decorative: it decides the gRPC status and therefore
// who gets sent to investigate. A missing credential is a PRECONDITION —
// something is unconfigured, and it is exactly what `internal/app/gitproviders.go`
// returns in git's twin case. A refused credential is UNAUTHENTICATED, like the
// git adapter's 401. A non-existent model is NOT FOUND. Network and provider
// errors are UNAVAILABLE, which is what separates "the third party went down"
// from "we have a defect".
func (e *Unavailable) kind() errs.Kind {
	switch e.Reason {
	case ReasonMissingCredential:
		return errs.KindPrecondition
	case ReasonRejectedCredential:
		return errs.KindUnauthorized
	case ReasonUnknownModel:
		return errs.KindNotFound
	case ReasonUnknownProvider:
		return errs.KindInvalid
	default:
		return errs.KindUnavailable
	}
}

// Classifier registration: `errs.KindOf` needs to know how to translate this
// error without the `errs` package knowing the `agent` package (which would
// create a cycle). Same mechanics as `ctxutil`.
func init() {
	errs.RegisterClassifier(func(err error) (errs.Kind, bool) {
		var u *Unavailable
		if errors.As(err, &u) {
			return u.kind(), true
		}
		return "", false
	})
}
