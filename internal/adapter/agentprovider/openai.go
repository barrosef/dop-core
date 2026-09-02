// An agent.AgentProvider adapter over the OpenAI/Codex API — the SECOND adapter.
//
// It exists out of ADR-0001's discipline: *a port with a single adapter is
// guesswork*. While only the Anthropic adapter existed, "agent provider port"
// would be Anthropic's API under another name — and the differences the platform
// is going to pay for (cache, counting, effort) would only show up on the day of
// the swap.
//
// THE DIVERGENCES, as this adapter handles them (numbering from
// agent/entity.go):
//
// D1 (cache) — Here the prefix cache is AUTOMATIC: there is no breakpoint to
// mark, no TTL to choose and, above all, NO CACHE CREATION ACCOUNTING. The
// response reports only `prompt_tokens_details.cached_tokens` (reads). That is
// why `Usage.CacheCreationTokens` is always 0 here, and the adapter does NOT
// announce `CapCacheCreationAccounting`. The consequence is a product one:
// ADR-0012 §1's silent-invalidator alert ("zero cache reads on a prefix that
// should be stable", which is `cost.UsageEvent.SuspectCacheMiss`) keeps working,
// but the other half — "it wrote cache and never read it" — is INVISIBLE under
// this provider. Whoever reads the telemetry needs to see the capability next to
// the number, and that is why it travels in the `Reply`.
//
// D2 (counting) — HERE IS THE DOUBLE-COUNTING TRAP. In this provider
// `prompt_tokens` INCLUDES the tokens served from cache; `cached_tokens` is a
// SUBSET of it. Summing the two fields as if they were disjoint parts — which is
// what `cost.UsageEvent` assumes, because it is Anthropic's semantics — would
// inflate ADR-0011's measurement with no error showing up at all. This adapter
// SUBTRACTS, and the subtraction is the most important line in the file.
//
// D3 (operator) — `role:"developer"` is this provider's authority channel, and it
// is accepted in any position. The stable prefix and the operator's intervention
// BOTH go as `developer`, which is correct: both are the platform's authorship.
// What the port guarantees and this adapter delivers is that an operator's
// intervention never goes out as `role:"user"`.
//
// D4 (effort) — Here only `low|medium|high` exist. `xhigh` and `max` are
// DOWNGRADED to `high`, with a readable warning in the `Reply`. On critical work
// (ADR-0007: you do not save on the critical path) that is a product decision,
// not an adapter detail: whoever routes to `max` and gets `high` needs to know
// that they got it.
//
// D5 (stop) — `stop | length | tool_calls | content_filter`, normalized.
//
// D7 (declaration) — Here the tool comes wrapped: `{type:"function",
// function:{name, description, parameters}}`. Two different names for the same
// thing (`parameters`, not `input_schema`) and one more level of nesting. It is
// only a shell — and that is why the port's common ground is name + description +
// schema, and not either one's format.
//
// D8 (call) — **HERE IS THIS FILE'S SECOND TRAP**, sibling to the double
// counting. The call comes in `tool_calls`, OUTSIDE `content`, and
// `function.arguments` is a **STRING**, not an object: it still needs
// `json.Unmarshal`. And the model may emit a string that is not valid JSON — an
// argument cut in half, wrong quotes. There, failing the turn would be the wrong
// answer: the one who fixes the argument is the MODEL, and it only fixes it if it
// gets the error back. This adapter returns the call with a nil `Input`, a filled
// `RawInput` and a warning; the loop turns it into an error result.
//
// D9 (result) — One `role:"tool"` message PER result, with a `tool_call_id`, and
// NO error field. The port's `IsError` boolean has nowhere to fit, so it becomes
// a MARKER IN THE TEXT. Losing the marker would make the model read a failure as
// normal output — which is the difference between "the test passed" and "the test
// did not even run".
//
// D10 (parallelism) — `parallel_tool_calls` is a top-level field here and the
// default is true, which is what we want; the adapter does not send the flag.
//
// D11 (undeclared) — a `tool_call_id` with no pair is a 400, the same as the
// other one. That is why the assistant's message with `tool_calls` is always
// resent before the results.
package agentprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

const (
	NameOpenAI  = "openai"
	BaseOpenAI  = "https://api.openai.com/v1"
	routeOpenAI = "/chat/completions"
)

// CatalogOpenAI is this provider's STARTING catalog — the same nature as
// `cost.DefaultCatalog()`: replaceable names, not an assertion about OpenAI's
// current catalog. When the provider comes configured on the account's resource
// (ADR-0013), the catalog comes from there and this remains only a default.
func CatalogOpenAI() map[agent.ModelClass]string {
	return map[agent.ModelClass]string{
		agent.ClassCheap:  "gpt-5-mini",
		agent.ClassMedium: "gpt-5",
		agent.ClassStrong: "gpt-5-codex",
	}
}

// PricesOpenAI is EMPTY on purpose.
//
// Filling this table with numbers nobody checked would be worse than leaving it
// empty: an invented price feeds ADR-0011's budget with convincing fiction, and
// nobody double-checks a plausible number. Empty, `PriceFor` returns "unknown"
// and the turn's cycle SAYS it cannot compute this provider's cost — instead of
// recording a zero, which would assert it was free.
//
// Filling it is the job of whoever holds the contract's table, and the right
// place is the configuration of the `agent`-category resource (ADR-0013).
func PricesOpenAI() map[string]agent.Price { return map[string]agent.Price{} }

// effortOpenAI: the core's five levels → the three here (D4). `xhigh` and `max`
// fall into `high`, which is the REAL ceiling — pretending it applied `max` would
// be worse than downgrading, because whoever trusts `max` on critical work would
// have no way of finding out.
var effortOpenAI = map[agent.Effort]agent.Effort{
	agent.EffortLow:    agent.EffortLow,
	agent.EffortMedium: agent.EffortMedium,
	agent.EffortHigh:   agent.EffortHigh,
	agent.EffortXHigh:  agent.EffortHigh,
	agent.EffortMax:    agent.EffortHigh,
}

var stopOpenAI = map[string]agent.StopReason{
	"stop":           agent.StopCompleted,
	"length":         agent.StopMaxTokens,
	"tool_calls":     agent.StopToolUse,
	"content_filter": agent.StopRefused,
}

type OpenAIConfig struct {
	APIBase string
	// APIKey is the ALREADY RESOLVED value of the resource credential (ADR-0013).
	APIKey  string
	Catalog map[agent.ModelClass]string
	Timeout time.Duration
	Client  httpDoer
}

type OpenAI struct {
	c       *client
	catalog map[agent.ModelClass]string
}

func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	base := cfg.APIBase
	if base == "" {
		base = BaseOpenAI
	}
	authorize := func(r *http.Request) {
		if cfg.APIKey != "" {
			r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}
		r.Header.Set("User-Agent", "dop-core")
	}
	cat := cfg.Catalog
	if len(cat) == 0 {
		cat = CatalogOpenAI()
	}
	return &OpenAI{
		c:       newClient(base, NameOpenAI, cfg.Client, cfg.Timeout, authorize, cfg.APIKey),
		catalog: cat,
	}
}

var _ agent.AgentProvider = (*OpenAI)(nil)

func (o OpenAI) String() string { return "agentprovider.OpenAI{}" }

func (o *OpenAI) Info() agent.ProviderInfo {
	return agent.ProviderInfo{
		Name:    NameOpenAI,
		Catalog: o.catalog,
		Capabilities: agent.Capabilities{
			// No CapExplicitPrefixCache: the cache is automatic (D1).
			// No CapCacheCreationAccounting: creation is not reported (D1).
			// No CapFullEffortRange: xhigh and max do not exist (D4).
			agent.CapOperatorChannel,
			agent.CapStructuredOutput,
			agent.CapToolUse,
		},
		Prices: PricesOpenAI(),
	}
}

// ── the provider's shapes ───────────────────────────────────────────────────
//
// Declaration order is the serialized body's order, and `Messages` comes early
// because THIS provider's only cache lever is the ORDER (D1): stable first,
// volatile after. There is no breakpoint to mark here — what exists is the
// discipline of leaving nothing volatile before the prefix.

// oaiCallFunction is a call's body. `Arguments` is a STRING — it is D8 in the
// type, and that is why it shows up both in the assembly and in the reading.
type oaiCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiCallFunction `json:"function"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls only on `role:"assistant"`; ToolCallID only on `role:"tool"`.
	// Both with `omitempty`: sending `tool_calls: null` on an ordinary message
	// is refused by some versions of the API.
	ToolCalls  []oaiCall `json:"tool_calls,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
}

// oaiToolFunction is the declaration's core (D7): `parameters`, not
// `input_schema`.
type oaiToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oaiTool struct {
	Type     string          `json:"type"`
	Function oaiToolFunction `json:"function"`
}

type oaiSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type oaiFormat struct {
	Type       string    `json:"type"`
	JSONSchema oaiSchema `json:"json_schema"`
}

type oaiRequest struct {
	Model string `json:"model"`
	// Tools BEFORE Messages, for guarantee 15's same reason: the declaration is
	// stable per thread and what is stable comes first. Here the cache is
	// automatic (D1) and the ORDER is the only lever that exists — leaving the
	// declaration after the conversation would take it out of the common prefix
	// with no error at all, only with an invoice.
	//
	// `omitempty` is guarantee 16.
	Tools               []oaiTool    `json:"tools,omitempty"`
	Messages            []oaiMessage `json:"messages"`
	MaxCompletionTokens int          `json:"max_completion_tokens"`
	ReasoningEffort     string       `json:"reasoning_effort"`
	ResponseFormat      *oaiFormat   `json:"response_format,omitempty"`
}

type oaiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// D8: OUTSIDE `content`, and with the arguments as a STRING.
			ToolCalls []oaiCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// ── assembly ────────────────────────────────────────────────────────────────

func (o *OpenAI) Render(t agent.Turn, model string, effort agent.Effort) ([]byte, []string, error) {
	var warnings []string

	// The stable prefix is the FIRST message. There is no breakpoint to mark
	// (D1): the only cache lever is the order, and it is what this adapter
	// respects.
	messages := []oaiMessage{{Role: "developer", Content: t.StablePrefix}}
	for _, m := range t.Messages {
		switch m.Role {
		case agent.RoleOperator:
			// The authority channel — NEVER `user` (D3).
			messages = append(messages, oaiMessage{Role: "developer", Content: m.Text})
		case agent.RoleAssistant:
			// D11: the calls come back along with the utterance. Without them,
			// the `tool_call_id` of the following `tool` messages is orphaned
			// and the API refuses the turn.
			messages = append(messages, oaiMessage{
				Role: "assistant", Content: m.Text, ToolCalls: oaiCalls(m.ToolCalls),
			})
		case agent.RoleToolResult:
			// D9: ONE message per result — it is the most visible difference in
			// shape between the two providers for the same domain turn, and that
			// is why the contract suite compares CONTENT and never message
			// counts.
			for _, r := range m.ToolResults {
				messages = append(messages, oaiMessage{
					Role: "tool", ToolCallID: r.CallID, Content: oaiResultText(r),
				})
			}
		default:
			messages = append(messages, oaiMessage{Role: "user", Content: m.Text})
		}
	}

	applied, known := effortOpenAI[effort]
	if !known {
		// An effort outside the core's vocabulary falls to this provider's real
		// ceiling, not its floor: in doubt you do not save (ADR-0011 §3).
		applied = agent.EffortHigh
	}
	if applied != effort {
		warnings = append(warnings,
			"effort '"+string(effort)+"' does not exist in this provider: applied '"+
				string(applied)+"' (D4). On critical work, that is a product decision.")
	}

	request := oaiRequest{
		Model:               model,
		Tools:               oaiTools(t.Tools),
		Messages:            messages,
		MaxCompletionTokens: t.MaxOutputTokens,
		ReasoningEffort:     string(applied),
	}
	if t.OutputSchema != nil {
		request.ResponseFormat = &oaiFormat{
			Type:       "json_schema",
			JSONSchema: oaiSchema{Name: "dop_turn", Strict: true, Schema: t.OutputSchema},
		}
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, nil, agent.Unavailability(NameOpenAI, agent.ReasonProviderError,
			"unreadable request: "+err.Error())
	}
	return body, warnings, nil
}

// oaiTools wraps the domain's declaration in this provider's shell (D7). An
// empty list returns nil and `omitempty` takes care of the rest (guarantee 16).
func oaiTools(specs []agent.ToolSpec) []oaiTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]oaiTool, 0, len(specs))
	for _, s := range specs {
		out = append(out, oaiTool{
			Type: "function",
			Function: oaiToolFunction{
				Name: s.Name, Description: s.Description, Parameters: s.InputSchema,
			},
		})
	}
	return out
}

// oaiCalls resends the calls in the history (D11).
//
// `Arguments` becomes a STRING again, and this is where `RawInput` earns its
// keep: when the argument arrived unreadable, it is what gets resent —
// re-serializing the nil `Input` would send `null` in place of what the model
// wrote, and the model would lose the chance to see its own error.
func oaiCalls(calls []agent.ToolCall) []oaiCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]oaiCall, 0, len(calls))
	for _, c := range calls {
		args := c.RawInput
		if c.Input != nil {
			if b, err := json.Marshal(c.Input); err == nil {
				args = string(b)
			}
		}
		if args == "" {
			args = "{}"
		}
		out = append(out, oaiCall{
			ID: c.ID, Type: "function",
			Function: oaiCallFunction{Name: c.Name, Arguments: args},
		})
	}
	return out
}

// oaiResultText is guarantee 21's HARD half.
//
// This provider has no error field on the `tool` message (D9): the port's
// `IsError` boolean has nowhere to fit. Instead of losing it — which would make
// the model read a failure as normal output and carry on asserting the opposite
// of what happened — the marker goes in the TEXT, in capitals and on the first
// line, which is where the model reads it before anything else.
func oaiResultText(r agent.ToolResult) string {
	if !r.IsError {
		return r.Content
	}
	return "TOOL ERROR:\n" + r.Content
}

// ── sending ─────────────────────────────────────────────────────────────────

func (o *OpenAI) Send(ctx context.Context, t agent.Turn, model string,
	effort agent.Effort) (*agent.Reply, error) {

	if !o.c.hasCredential {
		return nil, agent.Unavailability(NameOpenAI, agent.ReasonMissingCredential, "")
	}

	body, warnings, err := o.Render(t, model, effort)
	if err != nil {
		return nil, err
	}
	status, resp, err := o.c.post(ctx, routeOpenAI, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, o.c.failure(status, resp)
	}

	var out oaiResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, agent.Unavailability(NameOpenAI, agent.ReasonProviderError,
			"unreadable response: "+o.c.redact(err.Error()))
	}

	var text, reason string
	var calls []agent.ToolCall
	if len(out.Choices) > 0 {
		text = out.Choices[0].Message.Content
		reason = out.Choices[0].FinishReason
		for _, tc := range out.Choices[0].Message.ToolCalls {
			// ── D8, the line that stops the turn from dying over a bad
			// argument ── `arguments` is a STRING here. It may not be valid
			// JSON, and in that case the call GOES UP anyway, with a nil Input
			// and the raw text alongside: the one who fixes the argument is the
			// model, and it only fixes it if it gets the error back.
			c := agent.ToolCall{
				ID: tc.ID, Name: tc.Function.Name, RawInput: tc.Function.Arguments,
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &c.Input); err != nil || c.Input == nil {
				c.Input = nil
				warnings = append(warnings,
					"the model sent unreadable arguments for the '"+
						tc.Function.Name+"' tool: the call was returned to it as an error (D8)")
			}
			calls = append(calls, c)
		}
	}

	cached := out.Usage.PromptTokensDetails.CachedTokens
	// ── D2, the line that stops the double counting ─────────────────────────
	// `prompt_tokens` INCLUDES `cached_tokens` in this provider; the cost domain
	// expects DISJOINT parts. The floor at 0 is not paranoia: if one day the
	// provider changes the semantics, the worst case becomes underestimating the
	// input — and not recording an impossible number in the budget.
	input := out.Usage.PromptTokens - cached
	if input < 0 {
		input = 0
	}

	stop, ok := stopOpenAI[reason]
	if !ok {
		stop = agent.StopUnknown
	}

	applied, known := effortOpenAI[effort]
	if !known {
		applied = agent.EffortHigh
	}

	return &agent.Reply{
		Text: text,
		Usage: agent.Usage{
			InputTokens:     input,
			OutputTokens:    out.Usage.CompletionTokens,
			CacheReadTokens: cached,
			// NOT a zero-assertion: it is "it cannot be known" (D1). The
			// capability's ABSENCE from Capabilities is what says that to
			// whoever reads it.
			CacheCreationTokens: 0,
		},
		Model:         out.Model,
		Provider:      NameOpenAI,
		StopReason:    stop,
		Data:          decodeText(text),
		ToolCalls:     calls,
		EffortApplied: applied,
		Capabilities:  o.Info().Capabilities,
		Warnings:      warnings,
	}, nil
}
