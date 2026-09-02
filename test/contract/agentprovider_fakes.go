package contract

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ════════════════════════════════════════════════════════════════════════════
// THE AGENT PROVIDERS' TEST DOUBLES.
//
// They sit on the other side of the WIRE: an `httptest.Server` speaking the
// provider's protocol. The adapter under test is the REAL one — its `net/http`,
// its headers, its decoding. A mock of the adapter would pass even with the
// adapter wrong; a double of the provider would not.
//
// WHAT THESE DOUBLES PROVE: that both adapters agree with the SAME reading of
// each provider's documentation. WHAT THEY DO NOT PROVE: that the reading was
// right. This code's two bets — the exact text of the 400 when the model does
// not accept a system message in the middle, and each provider's cache
// accounting shape — are marked as such in the comments, because that is where
// whoever checks against the documentation will look. There is no automated
// path against the real API, and that is deliberate: a test that spends tokens
// per run is a test somebody turns off.
//
// Translating `ScriptedResponse` into the provider's shape is the double's
// heart: the suite speaks in DISJOINT parts and each double writes it in its own
// dialect — including OpenAI's INCLUSIVE dialect, which is D2's trap.
// ════════════════════════════════════════════════════════════════════════════

// fakeAgent is the state shared by both doubles.
type fakeAgent struct {
	mu                  sync.Mutex
	srv                 *httptest.Server
	token               string
	response            ScriptedResponse
	lastBody            []byte
	calls               int
	modelWithoutChannel string
}

func (f *fakeAgent) URL() string { return f.srv.URL }

func (f *fakeAgent) Script(t *testing.T, r ScriptedResponse) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.response = r
}

func (f *fakeAgent) LastBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.lastBody...)
}

func (f *fakeAgent) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// record keeps what arrived and returns the scripted response.
func (f *fakeAgent) record(body []byte) ScriptedResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastBody = body
	return f.response
}

// scriptedError emits the raw body of a scripted error, if there is one.
func scriptedError(w http.ResponseWriter, r ScriptedResponse) bool {
	if r.Status < 400 {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.Status)
	_, _ = io.WriteString(w, r.Body)
	return true
}

// ── Anthropic's double ──────────────────────────────────────────────────────

// NewAnthropicFake brings up the double of Anthropic's API.
//
// `modelWithoutChannel` is the model that refuses `role:"system"` in the middle
// of `messages` — it is what exercises D3's FALLBACK, the one part of the
// adapter that only shows up after a provider error.
func NewAnthropicFake(t *testing.T, token, modelWithoutChannel string) *fakeAgent {
	t.Helper()
	f := &fakeAgent{token: token, modelWithoutChannel: modelWithoutChannel}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		scripted := f.record(body)

		// The credential goes in `x-api-key` at this provider. A wrong key is a
		// 401, and the body ECHOES what arrived — which is the real behaviour
		// and is exactly where a credential leaks into the log.
		if r.Header.Get("x-api-key") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid x-api-key: `+
				r.Header.Get("x-api-key")+`"}}`)
			return
		}
		if scriptedError(w, scripted) {
			return
		}

		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &request)
		if f.modelWithoutChannel != "" && request.Model == f.modelWithoutChannel {
			for _, m := range request.Messages {
				// The 400 Anthropic returns on the models with no operator
				// channel. The text is a bet — see the header.
				if m.Role == "system" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":`+
						`"invalid_request_error","message":"messages: Unexpected role `+
						`\"system\". Allowed roles are user and assistant"}}`)
					return
				}
			}
		}

		// ── D8, THIS provider's dialect ────────────────────────────────────
		// The call is a block INSIDE `content`, next to the text, and `input`
		// is an OBJECT. The other provider's double writes the same
		// `ScriptedResponse` with the arguments as a STRING — it is that
		// difference that lets guarantee 17 ask whether the adapter
		// normalized.
		content := []any{map[string]any{"type": "text", "text": scripted.Text}}
		for _, c := range scripted.Tools {
			block := map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name}
			if scripted.UnreadableArgument {
				// Here the degenerate case is an `input` that is not an object.
				// There is no way to send "a string that is not JSON" — `input`
				// already arrives decoded — so the double sends a string where
				// the adapter expects an object. From the port's point of view
				// it is the SAME fact: the provider sent something that does
				// not become a map.
				block["input"] = "this-is-not-an-object"
			} else {
				in := c.Input
				if in == nil {
					in = map[string]any{}
				}
				block["input"] = in
			}
			content = append(content, block)
		}

		// The parts are DISJOINT at this provider, and the double writes them
		// as such: `input_tokens` EXCLUDES what came from the cache.
		resp := map[string]any{
			"model":       request.Model,
			"stop_reason": scripted.NativeStop,
			"content":     content,
			"usage": map[string]any{
				"input_tokens":                scripted.Usage.InputTokens,
				"output_tokens":               scripted.Usage.OutputTokens,
				"cache_read_input_tokens":     scripted.Usage.CacheReadTokens,
				"cache_creation_input_tokens": scripted.Usage.CacheCreationTokens,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// ── OpenAI's double ─────────────────────────────────────────────────────────

// NewOpenAIFake brings up the double of OpenAI's API.
func NewOpenAIFake(t *testing.T, token string) *fakeAgent {
	t.Helper()
	f := &fakeAgent{token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		scripted := f.record(body)

		authorization := r.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") ||
			strings.TrimPrefix(authorization, "Bearer ") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"incorrect api key: `+authorization+`"}}`)
			return
		}
		if scriptedError(w, scripted) {
			return
		}

		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &request)

		// ── D2'S TRAP, written on purpose ──────────────────────────────────
		// At this provider `prompt_tokens` INCLUDES the cached ones, and
		// `cached_tokens` is a subset of it. The double rewrites the suite's
		// disjoint parts into that inclusive shape — it is what lets guarantee
		// 6 ask whether the adapter subtracted.
		//
		// And cache creation simply DOES NOT EXIST here: there is no field to
		// write it in. That is divergence D1, and the double makes it concrete
		// instead of merely documented.
		prompt := scripted.Usage.InputTokens + scripted.Usage.CacheReadTokens

		// ── D8, THIS provider's dialect ────────────────────────────────────
		// The call comes in `tool_calls`, OUTSIDE `content`, and the arguments
		// are a STRING that still needs parsing. It is D8's trap written on
		// purpose: an adapter that does not decode delivers a null `Input`, and
		// guarantee 17 sees the difference.
		message := map[string]any{"content": scripted.Text}
		if len(scripted.Tools) > 0 {
			calls := make([]any, 0, len(scripted.Tools))
			for _, c := range scripted.Tools {
				args := "{}"
				if scripted.UnreadableArgument {
					// The real case: the model emitted a string that does not
					// close.
					args = `{"target": "cut`
				} else if c.Input != nil {
					b, _ := json.Marshal(c.Input)
					args = string(b)
				}
				calls = append(calls, map[string]any{
					"id": c.ID, "type": "function",
					"function": map[string]any{"name": c.Name, "arguments": args},
				})
			}
			message["tool_calls"] = calls
		}

		resp := map[string]any{
			"model": request.Model,
			"choices": []any{map[string]any{
				"message":       message,
				"finish_reason": scripted.NativeStop,
			}},
			"usage": map[string]any{
				"prompt_tokens":         prompt,
				"completion_tokens":     scripted.Usage.OutputTokens,
				"prompt_tokens_details": map[string]any{"cached_tokens": scripted.Usage.CacheReadTokens},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}
