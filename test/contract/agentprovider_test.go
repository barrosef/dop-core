package contract_test

// The AgentProvider contract suite against BOTH local doubles.
//
//	go test ./test/contract/ -run AgentProvider -v
//
// It always runs, with no infrastructure and no API key — and it is the only way
// for the suite to really exist: neither CI nor the laptop of whoever touches
// the adapter has an Anthropic or OpenAI credential, and a suite that only runs
// with a production credential is a suite that does not run. See the doubles'
// header for the limit of what they prove.
//
// There is NO path here that calls the providers for REAL, and that is a choice,
// not a pending item: a suite that spends tokens when it runs ends up not
// running, and a test that charges per run is a test somebody turns off. What
// the doubles cannot prove — the exact text of the 400 with no operator channel
// and each provider's cache accounting shape — is recorded as a BET in the
// adapter and in the doubles, in the place where whoever checks will look.
// Running with `-tags=integration` runs this same suite: the files with no tag
// compile in both modes, and the result being the same is deliberate.

import (
	"net/http/httptest"
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/agentprovider"
	"github.com/barrosef/dop-core/internal/domain/agent"
	"github.com/barrosef/dop-core/test/contract"
)

// fakeKey is guarantee 10's SENTINEL. It has to be an unlikely, recognizable
// sequence: the suite sweeps every output looking for it.
const fakeKey = "sk-SENTINEL-MUST-NOT-APPEAR-ANYWHERE-0001"

// deadAddress returns a URL nobody answers — it is the UNREACHABLE reason's
// case. A server created and then closed guarantees the port is free; a
// hand-picked number would give a test that fails on the machine of anyone with
// something listening there.
func deadAddress(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(nil)
	url := s.URL
	s.Close()
	return url
}

func TestAgentProviderContractAnthropic(t *testing.T) {
	// Sonnet 5 is the model that refuses `role:"system"` in the middle of
	// `messages` (D3) — it is the one that exercises the fallback.
	const withoutChannel = "claude-sonnet-5"
	f := contract.NewAnthropicFake(t, fakeKey, withoutChannel)

	contract.AgentProviderSuite(t, "anthropic", func(t *testing.T) contract.AgentProviderEnv {
		build := func(base, key string) agent.AgentProvider {
			return agentprovider.NewAnthropic(agentprovider.AnthropicConfig{
				APIBase: base + "/v1",
				APIKey:  key,
			})
		}
		return contract.AgentProviderEnv{
			Connect:           func(t *testing.T) agent.AgentProvider { return build(f.URL(), fakeKey) },
			WithoutCredential: func(t *testing.T) agent.AgentProvider { return build(f.URL(), "") },
			Unreachable:       func(t *testing.T) agent.AgentProvider { return build(deadAddress(t), fakeKey) },
			SentinelToken:     fakeKey,
			Script:            f.Script,
			LastBody:          f.LastBody,
			Calls:             f.Calls,
			Stops: map[string]agent.StopReason{
				"end_turn":                      agent.StopCompleted,
				"stop_sequence":                 agent.StopCompleted,
				"max_tokens":                    agent.StopMaxTokens,
				"model_context_window_exceeded": agent.StopMaxTokens,
				"refusal":                       agent.StopRefused,
				"tool_use":                      agent.StopToolUse,
				"pause_turn":                    agent.StopToolUse,
			},
			// All five levels exist here: nothing is downgraded (D4).
			EffortApplied: map[agent.Effort]agent.Effort{
				agent.EffortLow:    agent.EffortLow,
				agent.EffortMedium: agent.EffortMedium,
				agent.EffortHigh:   agent.EffortHigh,
				agent.EffortXHigh:  agent.EffortXHigh,
				agent.EffortMax:    agent.EffortMax,
			},
			ModelWithoutOperatorChannel: withoutChannel,
			ModelWithPrice:              "claude-opus-5",
			// EXPLICIT cache: the breakpoint is a marker in the body, and the
			// suite checks that it exists and that it sits at the END of the
			// prefix (D1).
			CacheMarker: "cache_control",
			// D7: the schema goes at the TOP of the tool's object, with no
			// wrapper.
			ToolMarker: "input_schema",
		}
	})
}

func TestAgentProviderContractOpenAI(t *testing.T) {
	f := contract.NewOpenAIFake(t, fakeKey)

	contract.AgentProviderSuite(t, "openai", func(t *testing.T) contract.AgentProviderEnv {
		build := func(base, key string) agent.AgentProvider {
			return agentprovider.NewOpenAI(agentprovider.OpenAIConfig{
				APIBase: base + "/v1",
				APIKey:  key,
			})
		}
		return contract.AgentProviderEnv{
			Connect:           func(t *testing.T) agent.AgentProvider { return build(f.URL(), fakeKey) },
			WithoutCredential: func(t *testing.T) agent.AgentProvider { return build(f.URL(), "") },
			Unreachable:       func(t *testing.T) agent.AgentProvider { return build(deadAddress(t), fakeKey) },
			SentinelToken:     fakeKey,
			Script:            f.Script,
			LastBody:          f.LastBody,
			Calls:             f.Calls,
			Stops: map[string]agent.StopReason{
				"stop":           agent.StopCompleted,
				"length":         agent.StopMaxTokens,
				"tool_calls":     agent.StopToolUse,
				"content_filter": agent.StopRefused,
			},
			// Only three levels: `xhigh` and `max` are DOWNGRADED to `high`, and
			// the suite requires the warning alongside (D4).
			EffortApplied: map[agent.Effort]agent.Effort{
				agent.EffortLow:    agent.EffortLow,
				agent.EffortMedium: agent.EffortMedium,
				agent.EffortHigh:   agent.EffortHigh,
				agent.EffortXHigh:  agent.EffortHigh,
				agent.EffortMax:    agent.EffortHigh,
			},
			// No model without an operator channel: `role:"developer"` is
			// accepted in any position at this provider.
			ModelWithoutOperatorChannel: "",
			// No price table, and on purpose: an invented price would feed
			// ADR-0011's budget with convincing fiction. Subtest 11 INVERTS
			// here and requires `PriceFor` to say it does not know.
			ModelWithPrice: "",
			// AUTOMATIC cache: there is no breakpoint to mark (D1). Empty here
			// and `CapExplicitPrefixCache` absent from Info are the SAME
			// statement, and the suite requires the two to agree.
			CacheMarker: "",
			// D7: here the tool comes wrapped in `function`, and the schema is
			// called `parameters`. Same fact, another name — it is what the port
			// normalizes.
			ToolMarker: "parameters",
		}
	})
}
