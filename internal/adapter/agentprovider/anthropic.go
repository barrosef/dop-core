// An agent.AgentProvider adapter over Anthropic's API.
//
// What THIS adapter delivers and the other one does not (see the divergences in
// internal/domain/agent/entity.go):
//
//   - EXPLICIT PREFIX CACHE (D1): the stable prefix goes as a `system` block with
//     `cache_control: ephemeral`. The breakpoint is the END OF THE PREFIX, and
//     not the end of the prompt — putting the marker after the turn's message
//     would write a new cache entry every turn and read none. It is the error
//     that does not fail, it only charges;
//   - CACHE CREATION ACCOUNTING (D1): `cache_creation_input_tokens` and
//     `cache_read_input_tokens` come separately, which is what ADR-0012 §1's
//     telemetry needs in order to report a silent invalidator;
//   - THE FIVE EFFORT LEVELS (D4), including the `max` ADR-0007 requires on
//     critical work.
//
// TOOLS, as this provider handles them (D7–D11):
//
//   - D7: `tools: [{name, description, input_schema}]` — the schema at the TOP
//     of the object, with no shell. And the declaration goes BEFORE `system` in
//     the body, which is this provider's canonical cache order (tools → system →
//     messages);
//   - D8: the call is a `tool_use` block INSIDE `content`, next to the text
//     blocks, and `input` is already an OBJECT. Nothing to decode — it is the
//     other adapter that has that job;
//   - D9: the result comes back as a `tool_result` block in a `role:"user"`
//     message, SEVERAL in the same message, with a native `is_error` boolean;
//   - D10: parallel by default, and the adapter sends no flag at all;
//   - D11: an orphan `tool_use_id` is a 400. That is why the assistant's
//     utterance with the calls is RESENT along with the results, always.
//
// What it does differently from the obvious:
//
//   - `thinking: adaptive`. The fixed reasoning-token budget (`budget_tokens`)
//     was removed on the current models and returns a 400. What controls depth is
//     `output_config.effort`, which is exactly ADR-0011 §3's second axis — the
//     two decisions fit with no translation;
//   - AN OPERATOR MESSAGE WITH A FALLBACK. `role:"system"` in the middle of
//     `messages` is the unforgeable channel and it preserves the prefix, but it
//     does not exist on every model (Sonnet 5 answers 400). Instead of keeping a
//     model list that ages in silence, the adapter TRIES and, on the specific
//     400, redoes the call with the instruction marked inside the user's turn —
//     and WARNS. Falling back without warning would be delivering, as the user's
//     data, something that authorizes.
package agentprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

const (
	NameAnthropic       = "anthropic"
	BaseAnthropic       = "https://api.anthropic.com/v1"
	routeAnthropic      = "/messages"
	apiVersionAnthropic = "2023-06-01"
	keyHeaderAnthropic  = "x-api-key"
)

// CatalogAnthropic is THIS provider's catalog: class → concrete name. It changes
// when Anthropic ships a model; ADR-0011 §3's policy does not change with it —
// it is `cost/router.go`'s policy × catalog separation, continued here.
func CatalogAnthropic() map[agent.ModelClass]string {
	return map[agent.ModelClass]string{
		agent.ClassCheap:  "claude-haiku-4-5",
		agent.ClassMedium: "claude-sonnet-5",
		agent.ClassStrong: "claude-opus-5",
	}
}

// PricesAnthropic is the price table, in MICROS per 1,000 tokens (USD).
//
// A cache read at 0.1× the input and a write at 1.25× — it is that ratio that
// makes ADR-0012's saving worth the stable prefix's discipline, and it is what
// has to show up in the measurement.
//
// It is a STARTING table and it AGES: prices change, and when they do this is
// where you touch. A model outside it returns "unknown" from `PriceFor`, and the
// turn's cycle SAYS it does not know — it never records a zero (see
// `agent.ProviderInfo.PriceFor`).
func PricesAnthropic() map[string]agent.Price {
	return map[string]agent.Price{
		"claude-opus-5":    {Currency: "USD", InputPer1k: 5_000, OutputPer1k: 25_000, CacheReadPer1k: 500, CacheCreationPer1k: 6_250},
		"claude-sonnet-5":  {Currency: "USD", InputPer1k: 3_000, OutputPer1k: 15_000, CacheReadPer1k: 300, CacheCreationPer1k: 3_750},
		"claude-haiku-4-5": {Currency: "USD", InputPer1k: 1_000, OutputPer1k: 5_000, CacheReadPer1k: 100, CacheCreationPer1k: 1_250},
	}
}

// stopAnthropic translates the provider's reason into the domain's vocabulary
// (D5).
var stopAnthropic = map[string]agent.StopReason{
	"end_turn":                      agent.StopCompleted,
	"stop_sequence":                 agent.StopCompleted,
	"max_tokens":                    agent.StopMaxTokens,
	"model_context_window_exceeded": agent.StopMaxTokens,
	"refusal":                       agent.StopRefused,
	"tool_use":                      agent.StopToolUse,
	// `pause_turn` is the model asking to continue after a server tool. With no
	// server tools on the port it should not appear; mapped to TOOL_USE because
	// that is what it means, and the domain needs to know the response did NOT
	// finish.
	"pause_turn": agent.StopToolUse,
}

type AnthropicConfig struct {
	// APIBase allows pointing at a corporate gateway or at a test double
	// without touching the turn's cycle.
	APIBase string
	// APIKey is the ALREADY RESOLVED value of the resource credential
	// (ADR-0013). This package does not know `ports.SecretStore`.
	APIKey string
	// An empty Catalog uses CatalogAnthropic(). It exists because, when the
	// provider comes configured on the account's resource, the catalog comes
	// from there.
	Catalog map[agent.ModelClass]string
	Timeout time.Duration
	Client  httpDoer
}

type Anthropic struct {
	c       *client
	catalog map[agent.ModelClass]string
}

func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	base := cfg.APIBase
	if base == "" {
		base = BaseAnthropic
	}
	authorize := func(r *http.Request) {
		if cfg.APIKey != "" {
			r.Header.Set(keyHeaderAnthropic, cfg.APIKey)
		}
		// The version is PINNED in the adapter, not configurable: it is the
		// promise that the response's format does not change under us. Leaving
		// it out means accepting the default version, which changes on its own.
		r.Header.Set("anthropic-version", apiVersionAnthropic)
		r.Header.Set("User-Agent", "dop-core")
	}
	cat := cfg.Catalog
	if len(cat) == 0 {
		cat = CatalogAnthropic()
	}
	return &Anthropic{
		c:       newClient(base, NameAnthropic, cfg.Client, cfg.Timeout, authorize, cfg.APIKey),
		catalog: cat,
	}
}

var _ agent.AgentProvider = (*Anthropic)(nil)

// String: a VALUE receiver, so it also applies to `%+v` of a value.
func (a Anthropic) String() string { return "agentprovider.Anthropic{}" }

func (a *Anthropic) Info() agent.ProviderInfo {
	return agent.ProviderInfo{
		Name:    NameAnthropic,
		Catalog: a.catalog,
		Capabilities: agent.Capabilities{
			agent.CapExplicitPrefixCache,
			agent.CapCacheCreationAccounting,
			agent.CapOperatorChannel,
			agent.CapFullEffortRange,
			agent.CapStructuredOutput,
			agent.CapToolUse,
		},
		Prices: PricesAnthropic(),
	}
}

// ── the provider's shapes (only what the port uses) ─────────────────────────
//
// These structs' FIELD ORDER is significant: `encoding/json` serializes in
// declaration order, and it is what puts the PREFIX before the messages in the
// body (the port's guarantee 3). Reordering `System` to come after `Messages`
// would break no behaviour test and would cost 10× on the invoice — which is why
// the contract suite verifies the order in the serialized body.

type antTextBlock struct {
	Type         string       `json:"type"`
	Text         string       `json:"text"`
	CacheControl *antCacheCtl `json:"cache_control,omitempty"`
}

// antBlock is a `content` block that may be text, a call or a result.
//
// One struct with `omitempty` on everything, and not three types behind an
// interface: the fields are disjoint by `type`, and the alternative would turn
// every assembly in this file into a type switch. `Input` is `any` because the
// output block carries an object and the input one carries nothing.
type antBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// antTool is the DECLARATION (D7): the schema at the top, with no "function"
// shell.
type antTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type antCacheCtl struct {
	Type string `json:"type"`
}

type antMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type antThinking struct {
	Type string `json:"type"`
}

type antFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type antOutputConfig struct {
	Effort string     `json:"effort"`
	Format *antFormat `json:"format,omitempty"`
}

type antRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// Tools comes BEFORE System, and it is not aesthetics: this provider's
	// canonical cache order is tools → system → messages, and the contract suite
	// audits the order in the SERIALIZED body (guarantee 15). A declaration
	// after the conversation is not wrong — it just leaves the cacheable stretch
	// and charges for it.
	//
	// `omitempty` is guarantee 16: a turn with no tool does not send the field.
	// An empty array takes up room in the prompt and invites the model to call
	// what does not exist.
	Tools        []antTool       `json:"tools,omitempty"`
	System       []antTextBlock  `json:"system"`
	Messages     []antMessage    `json:"messages"`
	Thinking     antThinking     `json:"thinking"`
	OutputConfig antOutputConfig `json:"output_config"`
}

type antResponse struct {
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// D8: the call comes INSIDE content, and `input` is already an object.
		// `json.RawMessage` and not `map[string]any` because an `input` that is
		// not an object (the degenerate case) has to reach the model as raw
		// text, and not vanish into a decoding error.
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// ── assembly ────────────────────────────────────────────────────────────────

func (a *Anthropic) Render(t agent.Turn, model string, effort agent.Effort) ([]byte, []string, error) {
	return a.render(t, model, effort, false)
}

// render assembles the request. THE CREDENTIAL DOES NOT ENTER HERE: it goes in a
// header, through the authorization closure — which makes `render` a surface
// safe to log and for the contract suite to inspect.
//
// `operatorInline` is D3's FALLBACK, and it is internal on purpose: whoever
// calls the port does not choose the operator channel, it is a consequence of
// the model.
func (a *Anthropic) render(t agent.Turn, model string, effort agent.Effort,
	operatorInline bool) ([]byte, []string, error) {

	var warnings []string
	var userContent []antTextBlock
	var messages []antMessage

	for _, m := range t.Messages {
		switch {
		case m.Role == agent.RoleOperator && !operatorInline:
			// A channel of its own: `role:"system"` AFTER the history preserves
			// the cached prefix (D3). NEVER `role:"user"`.
			messages = append(messages, antMessage{Role: "system", Content: m.Text})
		case m.Role == agent.RoleOperator:
			userContent = append(userContent, antTextBlock{
				Type: "text",
				Text: "<operator-intervention>\n" + m.Text + "\n</operator-intervention>",
			})
			warnings = append(warnings,
				"model with no native operator channel: the instruction was marked inside "+
					"the user's turn (D3)")
		case m.Role == agent.RoleToolResult:
			// D9: the results go as `tool_result` blocks in a `role:"user"`
			// message — SEVERAL in the same one, which is this provider's
			// format. A message of its OWN and not mixed into the user's turn:
			// a tool's output is untrusted content (substrate spec §6) and must
			// not be confused with the question of whoever asked for the work.
			messages = append(messages, antMessage{
				Role: "user", Content: antResults(m.ToolResults),
			})
		case m.Role == agent.RoleAssistant:
			// D11: the assistant's utterance RESENDS the calls with it. Without
			// them, the next result's `tool_use_id` is orphaned and the provider
			// returns a 400.
			messages = append(messages, antMessage{
				Role: "assistant", Content: antAssistantContent(m),
			})
		default:
			userContent = append(userContent, antTextBlock{Type: "text", Text: m.Text})
		}
	}

	if len(userContent) > 0 {
		// The user's turn goes in BEFORE any operator message already queued:
		// the `system` message in the middle has to follow a user turn, and it
		// is the last entry of `messages`.
		messages = append([]antMessage{{Role: "user", Content: userContent}}, messages...)
	}

	request := antRequest{
		Model:     model,
		MaxTokens: t.MaxOutputTokens,
		Tools:     antTools(t.Tools),
		// THE PREFIX, with the breakpoint at ITS END — and not at the end of the
		// prompt.
		System: []antTextBlock{{
			Type:         "text",
			Text:         t.StablePrefix,
			CacheControl: &antCacheCtl{Type: "ephemeral"},
		}},
		Messages:     messages,
		Thinking:     antThinking{Type: "adaptive"},
		OutputConfig: antOutputConfig{Effort: string(effort)},
	}
	if t.OutputSchema != nil {
		request.OutputConfig.Format = &antFormat{Type: "json_schema", Schema: t.OutputSchema}
	}

	// json.Marshal orders MAP keys alphabetically and keeps STRUCT fields in
	// declaration order — both are deterministic, which is guarantee 4. That is
	// why the schema can be a map without costing the cache.
	body, err := json.Marshal(request)
	if err != nil {
		return nil, nil, agent.Unavailability(NameAnthropic, agent.ReasonProviderError,
			"unreadable request: "+err.Error())
	}
	return body, warnings, nil
}

// antTools translates the domain's declaration into this provider's shape (D7).
// An empty list returns nil, and the request's `omitempty` does the rest: an
// ABSENT field, not an empty array (guarantee 16).
func antTools(specs []agent.ToolSpec) []antTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]antTool, 0, len(specs))
	for _, s := range specs {
		out = append(out, antTool{
			Name: s.Name, Description: s.Description, InputSchema: s.InputSchema,
		})
	}
	return out
}

// antAssistantContent assembles the assistant's utterance with the calls
// alongside (D11).
//
// The text comes BEFORE the calls because it is the order the model produced
// them in, and reassembling the history out of order makes the model read its
// own reasoning backwards.
func antAssistantContent(m agent.Message) []antBlock {
	blocks := make([]antBlock, 0, len(m.ToolCalls)+1)
	if strings.TrimSpace(m.Text) != "" {
		blocks = append(blocks, antBlock{Type: "text", Text: m.Text})
	}
	for _, c := range m.ToolCalls {
		input := c.Input
		if input == nil {
			// A call that arrived unreadable (D8) is resent with an EMPTY
			// object, and not omitted: the `tool_use_id` has to exist for the
			// corresponding error result to have a pair (D11). Omitting the call
			// and sending the result is the classic 400.
			input = map[string]any{}
		}
		blocks = append(blocks, antBlock{
			Type: "tool_use", ID: c.ID, Name: c.Name, Input: input,
		})
	}
	return blocks
}

// antResults assembles the `tool_result` blocks (D9). `is_error` is NATIVE here
// — it is the provider that has the boolean, and using it is guarantee 21's easy
// half.
func antResults(rs []agent.ToolResult) []antBlock {
	blocks := make([]antBlock, 0, len(rs))
	for _, r := range rs {
		blocks = append(blocks, antBlock{
			Type: "tool_result", ToolUseID: r.CallID, Content: r.Content, IsError: r.IsError,
		})
	}
	return blocks
}

// ── sending ─────────────────────────────────────────────────────────────────

func (a *Anthropic) Send(ctx context.Context, t agent.Turn, model string,
	effort agent.Effort) (*agent.Reply, error) {

	if !a.c.hasCredential {
		// With no key we do not spend a round trip: the error talks about
		// configuration, which is what it is.
		return nil, agent.Unavailability(NameAnthropic, agent.ReasonMissingCredential, "")
	}

	body, warnings, err := a.render(t, model, effort, false)
	if err != nil {
		return nil, err
	}
	status, resp, err := a.c.post(ctx, routeAnthropic, body)
	if err != nil {
		return nil, err
	}

	if status == http.StatusBadRequest && isOperatorChannel(resp) {
		// The documented fallback (D3): this model does not accept
		// `role:"system"` in the middle. Redo it with the instruction marked in
		// the user's turn. ONCE — a second 400 is a real 400.
		body, warnings, err = a.render(t, model, effort, true)
		if err != nil {
			return nil, err
		}
		status, resp, err = a.c.post(ctx, routeAnthropic, body)
		if err != nil {
			return nil, err
		}
	}
	if status >= 400 {
		return nil, a.c.failure(status, resp)
	}

	var out antResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		// An unreadable response is an UNAVAILABILITY, not a defect of ours:
		// almost always it is a proxy or an authentication portal answering HTML
		// in the provider's place.
		return nil, agent.Unavailability(NameAnthropic, agent.ReasonProviderError,
			"unreadable response: "+a.c.redact(err.Error()))
	}

	var text strings.Builder
	var calls []agent.ToolCall
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			// D8: here `input` is already an object — the decoding is one line,
			// and not the trap it is in the other adapter. The degenerate case
			// exists anyway (an `input` that is not an object), and the answer
			// is the same on both sides: the call GOES UP with a nil Input and a
			// warning, so the loop returns the error to the model instead of
			// killing the turn.
			c := agent.ToolCall{ID: b.ID, Name: b.Name, RawInput: string(b.Input)}
			if err := json.Unmarshal(b.Input, &c.Input); err != nil || c.Input == nil {
				c.Input = nil
				warnings = append(warnings,
					"the model sent unreadable arguments for the '"+b.Name+
						"' tool: the call was returned to it as an error (D8)")
			}
			calls = append(calls, c)
		}
	}

	stop, ok := stopAnthropic[out.StopReason]
	if !ok {
		// A new reason from the provider does NOT become an error: stopping work
		// because of an unknown string would be worse than recording that it
		// appeared.
		stop = agent.StopUnknown
	}

	return &agent.Reply{
		Text: text.String(),
		// The three input parts are DISJOINT in this provider (D2):
		// `input_tokens` already EXCLUDES what came from cache. Nothing to
		// subtract here — and it is precisely the OpenAI adapter that has to
		// subtract.
		Usage: agent.Usage{
			InputTokens:         out.Usage.InputTokens,
			OutputTokens:        out.Usage.OutputTokens,
			CacheReadTokens:     out.Usage.CacheReadInputTokens,
			CacheCreationTokens: out.Usage.CacheCreationInputTokens,
		},
		Model:      out.Model,
		Provider:   NameAnthropic,
		StopReason: stop,
		Data:       decodeText(text.String()),
		ToolCalls:  calls,
		// The five levels exist here: the effort requested is the one applied
		// (D4).
		EffortApplied: effort,
		Capabilities:  a.Info().Capabilities,
		Warnings:      warnings,
	}, nil
}

// isOperatorChannel recognizes the specific 400 of "this model does not accept a
// system message in the middle".
//
// It is a BET on a text Anthropic does not publish, and it is written here so
// that where the bet lives is explicit. The cost of erring on the LOW side is a
// turn that fails with a 400 instead of falling back; the cost of erring on the
// HIGH side is one unnecessary second call that probably fails the same way.
// Neither corrupts anything — and that is why the heuristic is acceptable here
// and would not be, for example, for deciding whether a merge conflicted.
func isOperatorChannel(body []byte) bool {
	t := strings.ToLower(string(body))
	return strings.Contains(t, "role") && strings.Contains(t, "system")
}
