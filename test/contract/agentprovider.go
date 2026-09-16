package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/agent"
)

// ════════════════════════════════════════════════════════════════════════════
// The contract suite of the agent.AgentProvider port.
//
// ADR-0001's discipline: a port with a single adapter is a guess. Anthropic and
// OpenAI do not have ONE line in common — explicit cache against automatic,
// disjoint accounting against inclusive, five effort levels against three,
// `role:"system"` against `role:"developer"` — and it is only by running both
// past this same ruler that "switching provider is wiring" stops being a
// promise.
//
// The provider's API is NEVER really called here: the double is on the other
// side of the WIRE (httptest), and the adapter under test is the REAL one, with
// its own `net/http`, its own headers and its own decoding. It is the difference
// between testing the adapter and testing a mock of the adapter — the second
// passes even when the adapter is wrong.
//
// An empty function field in Env means "this environment cannot produce that
// case": the subtest is SKIPPED on the record, never in silence.
// ════════════════════════════════════════════════════════════════════════════

// ScriptedResponse is what the double should answer, described in the DOMAIN's
// vocabulary — each double translates it into its provider's shape.
//
// `Usage` is DISJOINT here, always. It is that choice that makes guarantee 6
// verifiable from the outside: OpenAI's double receives disjoint parts and
// rewrites them in that provider's INCLUSIVE shape; if the adapter does not
// subtract, the suite sees the inflated input and fails. A double speaking a
// provider's vocabulary could not ask that question.
type ScriptedResponse struct {
	Text       string
	Usage      agent.Usage
	NativeStop string // stop reason in the PROVIDER's vocabulary
	Status     int    // 0 = 200
	Body       string // raw body when Status >= 400

	// Tools is what the model ASKS FOR in this response, described in the
	// domain's vocabulary. Each double writes it in its own dialect — an object
	// inside `content` in one, a string in `tool_calls` in the other (D8) — and
	// it is that difference that makes guarantee 17 verifiable from the
	// outside: an adapter that does not normalize delivers a null `Input` where
	// the other delivers the map.
	Tools []agent.ToolCall
	// UnreadableArgument makes the double write, in place of the arguments,
	// something that does NOT decode into an object. It is D8's degenerate
	// case, and what is required of it is that the turn SURVIVES: the one who
	// fixes the argument is the model, and it only fixes it if it gets the
	// error back.
	UnreadableArgument bool
}

// AgentProviderEnv is what THIS provider offers for the suite to work with.
type AgentProviderEnv struct {
	// Connect builds the REAL adapter, pointed at the double, with the sentinel
	// credential.
	Connect func(t *testing.T) agent.AgentProvider
	// WithoutCredential builds the adapter with no key at all — the
	// MISSING_CREDENTIAL reason's case, which has to be decided WITHOUT
	// touching the network.
	WithoutCredential func(t *testing.T) agent.AgentProvider
	// Unreachable builds the adapter pointed at an address that does not
	// answer.
	Unreachable func(t *testing.T) agent.AgentProvider

	// SentinelToken is the EXACT key `Connect` carries. The suite sweeps every
	// output looking for it (guarantee 10). Empty = the sweep is skipped with a
	// SHOUTED warning: it is the guarantee whose failure costs the whole bill.
	SentinelToken string

	// Script tells the double what to answer on the NEXT call.
	Script func(t *testing.T, r ScriptedResponse)
	// LastBody returns the body the adapter REALLY sent. It is what allows
	// checking that `Send` sends the same thing `Render` shows — without it,
	// `Render` could be a pretty shop window next to a different send.
	LastBody func() []byte
	// Calls counts the requests the double received.
	Calls func() int

	// Stops maps the provider's native reason → what the domain should see
	// (D5).
	Stops map[string]agent.StopReason

	// EffortApplied maps each of the core's FIVE levels to what THIS provider
	// actually applies (D4). Whoever has all five maps each onto itself;
	// whoever has three downgrades — and the suite requires the warning.
	EffortApplied map[agent.Effort]agent.Effort

	// ModelWithoutOperatorChannel is a model the provider REFUSES (400) when it
	// receives the operator instruction through the dedicated channel. Empty =
	// this provider has no such case, and the fallback's subtest is skipped.
	ModelWithoutOperatorChannel string

	// ModelWithPrice is a name THIS adapter has in its price table. Empty = the
	// adapter publishes no prices, and the subtest inverts: `PriceFor` has to
	// say it does NOT KNOW, instead of returning zero.
	ModelWithPrice string

	// CacheMarker is the fragment that, in THIS provider's body, marks the
	// cached prefix's breakpoint — "cache_control" at Anthropic. Empty when the
	// cache is automatic (OpenAI), and then the suite only requires coherence
	// with the declared capability.
	//
	// This field was born from a probe: deleting Anthropic's breakpoint passed
	// the entire suite. The prefix stayed in the right place, the order stayed
	// right, and the bill would start arriving ~10× larger without a single red
	// test — which is exactly the silent failure ADR-0012 §1 describes.
	CacheMarker string

	// ToolMarker is the fragment that, in THIS provider's body, marks a tool's
	// DECLARATION: "input_schema" at Anthropic, "parameters" at OpenAI (D7).
	// Empty = this adapter does not implement tools, and the suite requires
	// that it also does NOT announce CapToolUse — a capability is data the
	// telemetry reads, and declaring what you do not do is worse than not
	// declaring.
	ToolMarker string
}

const (
	sentinelPrefix   = "SENTINEL-STABLE-PREFIX-MUST-NOT-LEAVE-THE-TOP"
	sentinelTurn     = "SENTINEL-VOLATILE-TEXT-OF-THE-TURN"
	sentinelOperator = "SENTINEL-OPERATOR-INSTRUCTION"
	// sentinelSchema is a field planted in the output schema: it is how the
	// suite asks "did the schema reach the WIRE?" without knowing any
	// provider's shape. Without it, an adapter that stopped sending the schema
	// would pass — the decoding happens on our side and would keep working.
	sentinelSchema = "SENTINEL_SCHEMA_FIELD"
	// testOutputCap is a number unlikely to appear in the body by chance.
	testOutputCap = 4097
	// The tool loop's sentinels. Each answers a question the suite could not
	// ask knowing only one provider's shape: did the declaration arrive? did
	// the result come back bound to the call? did the error mark survive the
	// provider that has no field for it?
	sentinelTool       = "contract_sentinel_tool"
	sentinelToolSchema = "SENTINEL_TOOL_SCHEMA_FIELD"
	sentinelCallID     = "SENTINEL-CALL-ID-0001"
	sentinelResult     = "SENTINEL-RESULT-CONTENT"
)

// testTool is the declaration the suite sends down the wire.
func testTool() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        sentinelTool,
		Description: "contract tool, does not exist outside here",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				sentinelToolSchema: map[string]any{"type": "string"},
			},
			"additionalProperties": false,
		},
	}
}

// testTurn builds a turn with sentinels in each slice, so the suite can ask
// "where did this end up?" without knowing any provider's shape.
func testTurn(withOperator bool) agent.Turn {
	msgs := []agent.Message{{Role: agent.RoleUser, Text: sentinelTurn}}
	if withOperator {
		msgs = append(msgs, agent.Message{Role: agent.RoleOperator, Text: sentinelOperator})
	}
	schema := agent.OutputSchema()
	if props, ok := schema["properties"].(map[string]any); ok {
		props[sentinelSchema] = map[string]any{"type": "string"}
	}
	return agent.Turn{
		StablePrefix:    "contract\n" + sentinelPrefix + "\ncontext",
		Messages:        msgs,
		OutputSchema:    schema,
		MaxOutputTokens: testOutputCap,
	}
}

// AgentProviderSuite verifies the ten guarantees documented on the port.
func AgentProviderSuite(t *testing.T, name string, env func(t *testing.T) AgentProviderEnv) {
	t.Run(name, func(t *testing.T) {
		e := env(t)
		ctx := context.Background()

		// failures collects EVERY error message the suite produces, for
		// guarantee 10's final sweep. A credential leak almost never shows up
		// on the happy path: it shows up in the 401, which is precisely the
		// path where the provider echoes what it received.
		var failures []string
		note := func(err error) error {
			if err != nil {
				failures = append(failures, err.Error())
				var u *agent.Unavailable
				if errors.As(err, &u) {
					failures = append(failures, u.Detail())
				}
			}
			return err
		}

		t.Run("1_info_is_stable_and_never_hits_the_network", func(t *testing.T) {
			// Info is consulted on every turn's hot path (catalog, price,
			// capabilities). If it cost a network round trip, every turn would
			// pay one extra call — and the adapter pointed at nothing would not
			// even answer. This subtest proves both at once.
			p := e.Unreachable(t)
			a, b := p.Info(), p.Info()
			if a.Name == "" {
				t.Fatal("info with no provider name")
			}
			if a.Name != b.Name || len(a.Catalog) != len(b.Catalog) ||
				len(a.Capabilities) != len(b.Capabilities) {
				t.Fatalf("info unstable between calls: %+v != %+v", a, b)
			}
		})

		t.Run("2_resolve_model_is_pure_and_total", func(t *testing.T) {
			info := e.Connect(t).Info()
			for _, c := range []agent.ModelClass{agent.ClassCheap, agent.ClassMedium, agent.ClassStrong} {
				name := info.ResolveModel(c)
				if strings.TrimSpace(name) == "" {
					t.Fatalf("class %q resolved to an empty name — the provider would refuse "+
						"the call for a reason that is not the real one", c)
				}
				if name != info.ResolveModel(c) {
					t.Fatalf("class %q is not deterministic", c)
				}
			}
		})

		t.Run("3_the_stable_prefix_comes_before_the_messages", func(t *testing.T) {
			body, _, err := e.Connect(t).Render(testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			iPrefix := bytes.Index(body, []byte(sentinelPrefix))
			iTurn := bytes.Index(body, []byte(sentinelTurn))
			if iPrefix < 0 {
				t.Fatal("the stable prefix did not go into the request")
			}
			if iTurn < 0 {
				t.Fatal("the turn's text did not go into the request")
			}
			if iPrefix > iTurn {
				t.Fatalf("PREFIX AFTER THE VOLATILE PART (%d > %d): ADR-0012 §1's saving "+
					"breaks SILENTLY — nothing goes wrong, it just costs ~10× and shows up "+
					"on the invoice", iPrefix, iTurn)
			}
		})

		t.Run("4_render_is_deterministic", func(t *testing.T) {
			p := e.Connect(t)
			turn := testTurn(true)
			first, _, err := p.Render(turn, "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			// Several times: a map iterated in random order only gives itself
			// away after a few attempts, and that is exactly the defect that
			// invalidates the cached prefix on every turn.
			for i := 0; i < 20; i++ {
				other, _, err := p.Render(turn, "model-x", agent.EffortHigh)
				if err != nil {
					t.Fatalf("Render: %v", note(err))
				}
				if !bytes.Equal(first, other) {
					t.Fatalf("Render is NOT deterministic (attempt %d): different bytes for "+
						"the same Turn invalidate the prefix cache on every turn", i)
				}
			}
			// And the same prefix with different messages has to give the SAME
			// fingerprint: that is what "the prefix is stable" means.
			otherTurn := turn
			otherTurn.Messages = []agent.Message{{Role: agent.RoleUser, Text: "another question"}}
			if turn.Fingerprint() != otherTurn.Fingerprint() {
				t.Fatal("Fingerprint changed with the conversation: it is the PREFIX's, and only its")
			}
		})

		t.Run("5_the_operator_is_never_the_user", func(t *testing.T) {
			body, warnings, err := e.Connect(t).Render(testTurn(true), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			if !bytes.Contains(body, []byte(sentinelOperator)) {
				t.Fatal("the operator instruction vanished from the request")
			}
			if attributedToUser(t, body, sentinelOperator) && len(warnings) == 0 {
				t.Fatal("OPERATOR INSTRUCTION SERIALIZED AS USER SPEECH, with no warning: " +
					"it is the one that authorizes, and flattening the two roles opens the " +
					"door to prompt injection (D3)")
			}
			// And the user's text is still the user's — the guarantee's
			// converse, which would go unnoticed without this line.
			if !attributedToUser(t, body, sentinelTurn) {
				t.Fatal("the turn's text was not attributed to the user")
			}
		})

		t.Run("6_usage_is_disjoint", func(t *testing.T) {
			if e.Script == nil {
				t.Skip("this environment does not script responses")
			}
			p := e.Connect(t)
			// Numbers chosen so that the inclusive sum (1000) and the disjoint
			// one (700) are unmistakable: an adapter that forgets to subtract
			// returns 1000 and the difference leaps out.
			scripted := agent.Usage{
				InputTokens: 700, OutputTokens: 55,
				CacheReadTokens: 300, CacheCreationTokens: 120,
			}
			e.Script(t, ScriptedResponse{Text: `{"reply":"ok"}`, Usage: scripted,
				NativeStop: completedStop(e)})

			r, err := p.Send(ctx, testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Send: %v", note(err))
			}
			want := scripted
			if !p.Info().Supports(agent.CapCacheCreationAccounting) {
				// D1: this provider does not report cache creation. Zero here
				// is "there is no way to know", and the ABSENT capability is
				// what says so.
				want.CacheCreationTokens = 0
			}
			if r.Usage != want {
				t.Fatalf("DOUBLE COUNTING (D2): expected the disjoint parts %+v, got %+v — "+
					"summing inclusive fields inflates ADR-0011's measurement with nothing "+
					"failing", want, r.Usage)
			}
		})

		t.Run("7_stop_reason_in_the_domain_vocabulary", func(t *testing.T) {
			if e.Script == nil || len(e.Stops) == 0 {
				t.Skip("this environment does not script stop reasons")
			}
			p := e.Connect(t)
			for native, want := range e.Stops {
				e.Script(t, ScriptedResponse{Text: "hi", NativeStop: native})
				r, err := p.Send(ctx, testTurn(false), "model-x", agent.EffortHigh)
				if err != nil {
					t.Fatalf("Send (%s): %v", native, note(err))
				}
				if r.StopReason != want {
					t.Fatalf("stop %q became %q, expected %q", native, r.StopReason, want)
				}
			}
			// A NEW reason from the provider must not become an error: stopping
			// work because of an unknown string is worse than recording it.
			e.Script(t, ScriptedResponse{Text: "hi", NativeStop: "reason_that_does_not_exist_yet"})
			r, err := p.Send(ctx, testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("an unknown reason became an ERROR: %v", note(err))
			}
			if r.StopReason != agent.StopUnknown {
				t.Fatalf("an unknown reason became %q, expected %q", r.StopReason, agent.StopUnknown)
			}
		})

		t.Run("8_the_applied_effort_never_claims_too_much", func(t *testing.T) {
			if e.Script == nil || len(e.EffortApplied) == 0 {
				t.Skip("this environment does not declare the effort mapping")
			}
			p := e.Connect(t)
			for asked, want := range e.EffortApplied {
				e.Script(t, ScriptedResponse{Text: "hi", NativeStop: completedStop(e)})
				r, err := p.Send(ctx, testTurn(false), "model-x", asked)
				if err != nil {
					t.Fatalf("Send (%s): %v", asked, note(err))
				}
				if r.EffortApplied != want {
					t.Fatalf("effort %q: the adapter claimed %q, the provider applies %q — "+
						"pretending it applied is worse than downgrading (D4)", asked, r.EffortApplied, want)
				}
				if want != asked && len(r.Warnings) == 0 {
					t.Fatalf("effort %q was DOWNGRADED to %q with no warning: on critical work "+
						"(ADR-0007) that is a product decision, and whoever routed needs to know",
						asked, want)
				}
			}
		})

		t.Run("9_unavailability_with_the_right_reason", func(t *testing.T) {
			cases := []struct {
				name     string
				provider func(t *testing.T) agent.AgentProvider
				status   int
				body     string
				reason   agent.UnavailableReason
			}{
				{name: "no_credential", provider: e.WithoutCredential, reason: agent.ReasonMissingCredential},
				{name: "network_down", provider: e.Unreachable, reason: agent.ReasonUnreachable},
				{name: "credential_refused", status: 401, reason: agent.ReasonRejectedCredential},
				{name: "no_permission", status: 403, reason: agent.ReasonRejectedCredential},
				{name: "unknown_model", status: 404, reason: agent.ReasonUnknownModel},
				{name: "provider_failed", status: 500, reason: agent.ReasonProviderError},
				{name: "rate_limit", status: 429, reason: agent.ReasonProviderError},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					p := e.Connect(t)
					if c.provider != nil {
						p = c.provider(t)
					} else {
						if e.Script == nil {
							t.Skip("this environment does not script statuses")
						}
						// A body with the sentinel INSIDE: it is the real worst
						// case — the provider echoing what it received in an
						// error body.
						e.Script(t, ScriptedResponse{Status: c.status,
							Body: `{"error":"raw detail with ` + e.SentinelToken + ` inside"}`})
					}
					_, err := p.Send(ctx, testTurn(false), "model-x", agent.EffortHigh)
					note(err)
					if err == nil {
						t.Fatal("expected unavailability, got success")
					}
					var u *agent.Unavailable
					if !errors.As(err, &u) {
						t.Fatalf("a RAW error leaked through the port (%T): a third party's "+
							"unavailability must not arrive as an error of ours (D6): %v", err, err)
					}
					if u.Reason != c.reason {
						t.Fatalf("reason %q, expected %q", u.Reason, c.reason)
					}
				})
			}
		})

		t.Run("9b_a_missing_credential_never_touches_the_network", func(t *testing.T) {
			if e.Calls == nil {
				t.Skip("this environment does not count calls")
			}
			before := e.Calls()
			_, err := e.WithoutCredential(t).Send(ctx, testTurn(false), "model-x", agent.EffortHigh)
			note(err)
			if after := e.Calls(); after != before {
				t.Fatalf("a missing credential cost %d network round trip(s): whoever calls "+
					"with no key must not spend a call to find that out", after-before)
			}
		})

		t.Run("10_the_credential_never_leaks", func(t *testing.T) {
			if e.SentinelToken == "" {
				t.Log("### WARNING: with no SentinelToken, the port's most expensive guarantee " +
					"was NOT verified — an agent key in a log is a key at rest")
				t.Skip("no sentinel")
			}
			p := e.Connect(t)
			body, _, err := p.Render(testTurn(true), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			if bytes.Contains(body, []byte(e.SentinelToken)) {
				t.Fatal("THE CREDENTIAL ENDED UP IN THE REQUEST BODY: it goes in a header, " +
					"and `Render` has to be a surface that is safe to log")
			}
			// %v and %+v: fmt reads UNEXPORTED fields by reflection and cannot
			// call their String(). That is why the key lives in a closure, and
			// this is what checks it is still there.
			for _, formatted := range []string{fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p)} {
				if strings.Contains(formatted, e.SentinelToken) {
					t.Fatalf("THE CREDENTIAL APPEARS IN THE ADAPTER'S FORMATTING: %s", formatted)
				}
			}
			for _, msg := range failures {
				if strings.Contains(msg, e.SentinelToken) {
					t.Fatalf("THE CREDENTIAL APPEARS IN AN ERROR MESSAGE: %s", msg)
				}
			}
			if len(failures) == 0 {
				t.Fatal("no error message was collected: the sweep would pass empty, which is " +
					"worse than not sweeping — it would assert a guarantee it did not test")
			}
		})

		t.Run("11_an_unknown_price_never_becomes_zero", func(t *testing.T) {
			info := e.Connect(t).Info()
			if _, ok := info.PriceFor("model-that-exists-in-no-catalog-at-all"); ok {
				t.Fatal("PriceFor invented a price for an unknown model")
			}
			if e.ModelWithPrice == "" {
				// An adapter with no table: the only honest answer is "I do not
				// know" for EVERYTHING, including its own catalog. Zero would
				// assert the call was free (ADR-0011 §2).
				for _, c := range []agent.ModelClass{agent.ClassCheap, agent.ClassMedium, agent.ClassStrong} {
					if _, ok := info.PriceFor(info.ResolveModel(c)); ok {
						t.Fatalf("the environment says it has no price table, but %q has a price", c)
					}
				}
				return
			}
			price, ok := info.PriceFor(e.ModelWithPrice)
			if !ok {
				t.Fatalf("model %q should have a price", e.ModelWithPrice)
			}
			if price.Currency == "" {
				t.Fatal("a price with no currency: micros with no unit is a number that adds " +
					"dollars to reais")
			}
			// INTEGER arithmetic from start to finish, and per 1,000 tokens.
			cost := price.CostMicros(agent.Usage{InputTokens: 1000})
			if cost != price.InputPer1k {
				t.Fatalf("1,000 input tokens cost %d, expected %d", cost, price.InputPer1k)
			}
		})

		t.Run("12_structured_output_arrives_decoded", func(t *testing.T) {
			if e.Script == nil {
				t.Skip("this environment does not script responses")
			}
			p := e.Connect(t)
			if !p.Info().Supports(agent.CapStructuredOutput) {
				t.Skip("this provider does not announce structured output")
			}
			e.Script(t, ScriptedResponse{
				Text:       `{"reply":"answer to the human","concluded":true,"finding_title":"t","finding_summary":"s","finding_evidence":["e1"]}`,
				NativeStop: completedStop(e),
			})
			r, err := p.Send(ctx, testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Send: %v", note(err))
			}
			if r.Data == nil {
				t.Fatal("the structured output was not decoded: the domain would have to " +
					"re-parse text, which is what ADR-0012 §2 avoids")
			}
			if r.Data["reply"] != "answer to the human" {
				t.Fatalf("the `reply` field came back as %v", r.Data["reply"])
			}
			// Loose text must NOT become an error: the schema may fail and the
			// raw speech is still worth something to the human reading the
			// thread.
			e.Script(t, ScriptedResponse{Text: "loose text, no JSON", NativeStop: completedStop(e)})
			r2, err := p.Send(ctx, testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("loose text became an error: %v", note(err))
			}
			if r2.Text != "loose text, no JSON" {
				t.Fatalf("the raw text was lost: %q", r2.Text)
			}
		})

		t.Run("13_send_sends_what_render_shows", func(t *testing.T) {
			if e.Script == nil || e.LastBody == nil {
				t.Skip("this environment does not expose the sent body")
			}
			p := e.Connect(t)
			turn := testTurn(true)
			want, _, err := p.Render(turn, "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			e.Script(t, ScriptedResponse{Text: "hi", NativeStop: completedStop(e)})
			if _, err := p.Send(ctx, turn, "model-x", agent.EffortHigh); err != nil {
				t.Fatalf("Send: %v", note(err))
			}
			if !bytes.Equal(want, e.LastBody()) {
				t.Fatalf("Send sent something DIFFERENT from what Render shows — guarantee 3's "+
					"audit would be looking at a shop window:\nrender: %s\nsent: %s",
					want, e.LastBody())
			}
		})

		t.Run("15_the_cache_breakpoint_marks_the_end_of_the_prefix", func(t *testing.T) {
			p := e.Connect(t)
			explicit := p.Info().Supports(agent.CapExplicitPrefixCache)
			if explicit == (e.CacheMarker == "") {
				t.Fatalf("incoherence between the declared capability (%v) and the "+
					"environment's marker (%q): a capability is DATA the telemetry reads, and "+
					"declaring what you do not do is worse than not declaring", explicit, e.CacheMarker)
			}
			if !explicit {
				t.Skip("automatic cache at this provider: there is no breakpoint to mark (D1)")
			}
			body, _, err := p.Render(testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			iMark := bytes.Index(body, []byte(e.CacheMarker))
			if iMark < 0 {
				t.Fatal("THE CACHE BREAKPOINT VANISHED: the adapter announces an explicit " +
					"cache and does not mark the prefix. Nothing fails — the whole prefix " +
					"starts being charged as new input on every turn (~10×), and only the " +
					"invoice tells (ADR-0012 §1)")
			}
			if iTurn := bytes.Index(body, []byte(sentinelTurn)); iMark > iTurn {
				t.Fatalf("the breakpoint ended up AFTER the volatile text (%d > %d): marking "+
					"at the end of the prompt writes a new cache entry every turn and reads "+
					"none", iMark, iTurn)
			}
		})

		t.Run("16_the_output_schema_goes_to_the_wire", func(t *testing.T) {
			p := e.Connect(t)
			if !p.Info().Supports(agent.CapStructuredOutput) {
				t.Skip("this provider does not announce structured output")
			}
			body, _, err := p.Render(testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			if !bytes.Contains(body, []byte(sentinelSchema)) {
				t.Fatal("THE OUTPUT SCHEMA DID NOT GO INTO THE REQUEST: the adapter announces " +
					"structured output and does not ask for it. The decoding on our side keeps " +
					"working while the model cooperates, and the provider's validation " +
					"(ADR-0012 §2) becomes luck — with no red test")
			}
		})

		t.Run("17_the_output_cap_goes_to_the_wire", func(t *testing.T) {
			body, _, err := e.Connect(t).Render(testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			if !bytes.Contains(body, []byte(fmt.Sprint(testOutputCap))) {
				t.Fatal("THE TURN'S OUTPUT CAP DID NOT REACH THE PROVIDER: the adapter is " +
					"using a limit nobody asked for. A truncated answer would start coming out " +
					"as StopReason=max_tokens with nothing in the system explaining why — and " +
					"the cap is a cost lever (ADR-0011)")
			}
		})

		// ── tools: guarantees 15 to 21 (D7–D11) ─────────────────────────────
		//
		// The `guarantee` prefix in the name because the subtests above were
		// numbered by the ORDER in which they were born, not by the guarantee
		// they prove (the "15_" above is guarantee 11). Reusing the numbers
		// here would give two subtests called "15" proving different things,
		// and whoever reads the `-v` output would have no way to tell which is
		// which.

		t.Run("guarantee15_the_tool_declaration_goes_before_the_volatile_part", func(t *testing.T) {
			p := e.Connect(t)
			hasTools := p.Info().Supports(agent.CapToolUse)
			if hasTools == (e.ToolMarker == "") {
				t.Fatalf("incoherence between the declared capability (%v) and the "+
					"environment's marker (%q): a capability is DATA the telemetry reads, and "+
					"declaring what you do not do is worse than not declaring", hasTools, e.ToolMarker)
			}
			if !hasTools {
				t.Skip("this adapter does not implement tools")
			}

			turn := testTurn(false)
			turn.Tools = []agent.ToolSpec{testTool()}
			body, _, err := p.Render(turn, "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			if !bytes.Contains(body, []byte(e.ToolMarker)) {
				t.Fatalf("the tool DECLARATION did not go into the request (expected %q in "+
					"the body): the adapter announces tools and does not declare them, and the "+
					"model will never ask for what it does not know exists", e.ToolMarker)
			}
			if !bytes.Contains(body, []byte(sentinelToolSchema)) {
				t.Fatal("the tool's SCHEMA did not go into the request: without it the " +
					"provider validates nothing and every argument becomes luck")
			}
			iTool := bytes.Index(body, []byte(sentinelTool))
			iTurn := bytes.Index(body, []byte(sentinelTurn))
			if iTool > iTurn {
				t.Fatalf("the declaration ended up AFTER the volatile text (%d > %d): a "+
					"declaration is stable per thread and leaves the cacheable stretch when it "+
					"goes to the end — nothing goes wrong, it just costs (ADR-0012 §1)", iTool, iTurn)
			}
		})

		t.Run("guarantee16_a_turn_with_no_tool_does_not_send_the_field", func(t *testing.T) {
			p := e.Connect(t)
			if !p.Info().Supports(agent.CapToolUse) || e.ToolMarker == "" {
				t.Skip("this adapter does not implement tools")
			}
			body, _, err := p.Render(testTurn(false), "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			// An empty array is not the same thing as an absent field: it takes
			// up room in the prompt and invites the model to call what does not
			// exist.
			if bytes.Contains(body, []byte(`"tools"`)) {
				t.Fatalf("a turn WITHOUT tools sent the `tools` field anyway:\n%s", body)
			}
		})

		t.Run("guarantee17and19_the_call_is_normalized_and_the_stop_is_tool_use", func(t *testing.T) {
			p := e.Connect(t)
			if e.Script == nil || !p.Info().Supports(agent.CapToolUse) {
				t.Skip("this environment does not script responses, or the adapter has no tools")
			}
			asked := []agent.ToolCall{
				{ID: sentinelCallID, Name: sentinelTool,
					Input: map[string]any{"target": "first"}},
				{ID: sentinelCallID + "-b", Name: sentinelTool,
					Input: map[string]any{"target": "second"}},
			}
			e.Script(t, ScriptedResponse{
				Text: "let me look", Tools: asked, NativeStop: toolStop(e),
			})

			turn := testTurn(false)
			turn.Tools = []agent.ToolSpec{testTool()}
			r, err := p.Send(ctx, turn, "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Send: %v", note(err))
			}
			if r.StopReason != agent.StopToolUse {
				t.Fatalf("the provider asked for a tool and the stop came as %q, expected %q (D5)",
					r.StopReason, agent.StopToolUse)
			}
			if len(r.ToolCalls) != 2 {
				t.Fatalf("expected 2 calls, got %d: %+v", len(r.ToolCalls), r.ToolCalls)
			}
			// The ORDER is the one the provider emitted (D10). Reordering turns
			// "I ran the test and then read the log" into "I read the log and
			// then ran the test" in the model's reading.
			for i, want := range asked {
				got := r.ToolCalls[i]
				if got.ID != want.ID || got.Name != want.Name {
					t.Fatalf("call %d came as %+v, expected id=%q name=%q",
						i, got, want.ID, want.Name)
				}
				if got.Input == nil {
					t.Fatalf("call %d arrived with a NULL Input: one of the providers sends the "+
						"arguments as a STRING (D8), and not decoding them leaves the loop with "+
						"nothing to execute", i)
				}
				if got.Input["target"] != want.Input["target"] {
					t.Fatalf("call %d lost its argument: %+v", i, got.Input)
				}
			}
		})

		t.Run("guarantee18_an_unreadable_argument_does_not_bring_the_turn_down", func(t *testing.T) {
			p := e.Connect(t)
			if e.Script == nil || !p.Info().Supports(agent.CapToolUse) {
				t.Skip("this environment does not script responses, or the adapter has no tools")
			}
			e.Script(t, ScriptedResponse{
				Text: "let me look",
				Tools: []agent.ToolCall{
					{ID: sentinelCallID, Name: sentinelTool},
				},
				UnreadableArgument: true,
				NativeStop:         toolStop(e),
			})
			turn := testTurn(false)
			turn.Tools = []agent.ToolSpec{testTool()}

			r, err := p.Send(ctx, turn, "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("AN UNREADABLE ARGUMENT BROUGHT THE TURN DOWN: the one who fixes the "+
					"argument is the MODEL, and it only fixes it if it gets the error back "+
					"(D8): %v", note(err))
			}
			if len(r.ToolCalls) != 1 {
				t.Fatalf("the call with the unreadable argument VANISHED: %+v", r.ToolCalls)
			}
			c := r.ToolCalls[0]
			if c.Input != nil {
				t.Fatalf("the unreadable argument became an object: %+v — null is what says "+
					"'it could not be read', and an empty map would assert 'no arguments'", c.Input)
			}
			if strings.TrimSpace(c.RawInput) == "" {
				t.Fatal("the raw argument was lost: 'your argument is invalid' without saying " +
					"WHICH argument is a message that fixes nothing")
			}
			if len(r.Warnings) == 0 {
				t.Fatal("an unreadable argument with no warning: whoever reads the telemetry " +
					"has no way to tell this from a model that simply did not call a tool")
			}
			// And the id has to survive: without it the loop's error result is
			// orphaned and BOTH providers refuse the next turn (D11).
			if c.ID != sentinelCallID {
				t.Fatalf("the call's id was lost (%q): a result with no pair is a 400 at both", c.ID)
			}
		})

		t.Run("guarantee20and21_the_result_is_bound_to_the_call_and_carries_the_error_mark", func(t *testing.T) {
			p := e.Connect(t)
			if !p.Info().Supports(agent.CapToolUse) {
				t.Skip("this adapter does not implement tools")
			}
			turn := testTurn(false)
			turn.Tools = []agent.ToolSpec{testTool()}
			// The story of a second round: the model's speech WITH the call,
			// and the result right after. Both are mandatory and in this order
			// — both providers refuse a result with no call (D11).
			turn.Messages = append(turn.Messages,
				agent.Message{Role: agent.RoleAssistant, Text: "let me look",
					ToolCalls: []agent.ToolCall{{
						ID: sentinelCallID, Name: sentinelTool,
						Input: map[string]any{"target": "x"},
					}}},
				agent.Message{Role: agent.RoleToolResult,
					ToolResults: []agent.ToolResult{{
						CallID: sentinelCallID, Name: sentinelTool,
						Content: sentinelResult, IsError: true,
					}}},
			)

			body, _, err := p.Render(turn, "model-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", note(err))
			}
			if !bytes.Contains(body, []byte(sentinelResult)) {
				t.Fatal("the tool's result did not go into the request")
			}
			// The id has to appear TWICE: once in the resent call (on the
			// assistant's side) and once binding the result to it. Counting the
			// occurrences, and not just looking for the id, is what separates
			// "the pair exists" from "the id is somewhere in the body" — and
			// the difference is not academic: this subtest's first version
			// looked only for presence, and deleting the resending of the calls
			// PASSED it. An orphaned result is a 400 at both providers (D11),
			// and it is the suite that has to catch that, not the API in
			// production.
			if n := bytes.Count(body, []byte(sentinelCallID)); n < 2 {
				t.Fatalf("the call's id appears %d time(s) in the body, expected at least 2 "+
					"(the resent call and the result referencing it). A result whose id has no "+
					"pair is a 400 at both providers (D11):\n%s", n, body)
			}
			// And the assistant's SPEECH is resent too: without it the model
			// reads the result without remembering why it asked for it.
			if !bytes.Contains(body, []byte("let me look")) {
				t.Fatal("the assistant's speech carrying the call was not resent: the " +
					"following result is orphaned (D11)")
			}
			// The error mark has to REACH the model somehow: one provider has
			// the native boolean, the other has no field at all and writes it
			// in the text (D9). The suite does not know which is which — it
			// requires the information to exist in SOME form.
			if !bytes.Contains(body, []byte("is_error")) &&
				!bytes.Contains(bytes.ToUpper(body), []byte("ERROR")) {
				t.Fatalf("the result's ERROR MARK was lost: the model will read a failure as "+
					"normal output and go on asserting the opposite of what happened "+
					"(guarantee 21):\n%s", body)
			}
			// And the result's content must NOT be attributed to the user: it
			// is untrusted content (the execution spec §6), and a malicious
			// `README` read by `cat` must not arrive with the authority of
			// whoever asked for the work.
			if roleOfText(t, body, sentinelResult) == "user" &&
				!bytes.Contains(body, []byte("tool_result")) {
				t.Fatal("the tool's result was serialized as loose USER SPEECH: a command's " +
					"output is untrusted content and must not become an instruction")
			}
		})

		t.Run("14_the_operator_channel_fallback", func(t *testing.T) {
			if e.ModelWithoutOperatorChannel == "" || e.Script == nil {
				t.Skip("this provider has no model without an operator channel")
			}
			p := e.Connect(t)
			e.Script(t, ScriptedResponse{Text: "hi", NativeStop: completedStop(e)})
			r, err := p.Send(ctx, testTurn(true), e.ModelWithoutOperatorChannel, agent.EffortHigh)
			if err != nil {
				t.Fatalf("the fallback did not happen: %v", note(err))
			}
			if len(r.Warnings) == 0 {
				t.Fatal("FALLBACK WITH NO WARNING: the operator instruction was delivered " +
					"inside the user's turn and nobody found out (D3)")
			}
			body := e.LastBody()
			if !bytes.Contains(body, []byte(sentinelOperator)) {
				t.Fatal("the operator instruction vanished in the fallback")
			}
			// Even in the fallback it has to be MARKED — delivered as loose
			// user text, it would become data indistinguishable from an
			// injection.
			if !bytes.Contains(body, []byte("operator-intervention")) {
				t.Fatal("in the fallback, the instruction entered the user's turn UNMARKED")
			}
		})
	})
}

// completedStop returns a native stop reason meaning "it finished", for the
// subtests that are not measuring the stop.
func completedStop(e AgentProviderEnv) string {
	for native, domain := range e.Stops {
		if domain == agent.StopCompleted {
			return native
		}
	}
	return ""
}

// toolStop returns the NATIVE reason meaning "I asked for a tool".
func toolStop(e AgentProviderEnv) string {
	for native, domain := range e.Stops {
		if domain == agent.StopToolUse {
			return native
		}
	}
	return ""
}

// roleOfText returns the role of the object containing `target`, or "" if it
// does not find it.
//
// Generic for the same reason as `attributedToUser`: the suite cannot know any
// provider's shape. What it does know is that both mark who speaks in a `role`
// field.
func roleOfText(t *testing.T, body []byte, target string) string {
	t.Helper()
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unreadable body: %v", err)
	}
	for _, role := range []string{"user", "assistant", "tool", "developer", "system"} {
		if findUnderRole(root, role, target) {
			return role
		}
	}
	return ""
}

// attributedToUser looks for `target` INSIDE some object with `role: "user"`.
//
// Generic on purpose: the suite cannot know any provider's shape, or it becomes
// two tests with a single name. What it does know is that both shapes mark the
// speaker's role in a `role` field, and that is enough to ask "was this sentence
// attributed to the user?".
func attributedToUser(t *testing.T, body []byte, target string) bool {
	t.Helper()
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unreadable body: %v", err)
	}
	return findUnderRole(root, "user", target)
}

func findUnderRole(v any, role, target string) bool {
	switch n := v.(type) {
	case map[string]any:
		if p, ok := n["role"].(string); ok && p == role && containsText(n, target) {
			return true
		}
		for _, e := range n {
			if findUnderRole(e, role, target) {
				return true
			}
		}
	case []any:
		for _, e := range n {
			if findUnderRole(e, role, target) {
				return true
			}
		}
	}
	return false
}

func containsText(v any, target string) bool {
	switch n := v.(type) {
	case string:
		return strings.Contains(n, target)
	case map[string]any:
		for _, e := range n {
			if containsText(e, target) {
				return true
			}
		}
	case []any:
		for _, e := range n {
			if containsText(e, target) {
				return true
			}
		}
	}
	return false
}
