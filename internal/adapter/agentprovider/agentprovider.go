// Package agentprovider implements the agent.AgentProvider port over
// Anthropic's and OpenAI's APIs.
//
// ── About SDKs ──────────────────────────────────────────────────────────────
//
// Plain `net/http`, no vendor SDK — the same pattern as the gitprovider,
// secretstore, objectstore and sandbox adapters, and for the same two reasons.
// The SDK would bring the vendor's vocabulary back into the process
// (`anthropic.Message`, `openai.ChatCompletion`) and the first time somebody
// passed it along the port would have leaked; and this repository's go.mod is
// deliberately lean, while the official SDKs drag large dependency trees along
// for what here is ONE HTTP route per provider.
//
// This is a CONSCIOUS DIVERGENCE from the Python implementation this package
// ports: there the Anthropic adapter used the official SDK and only OpenAI's
// spoke HTTP by hand. The house rule here is the other one, and the contract
// suite is what guarantees the change of tactic did not change the contract: it
// runs the same against both adapters.
//
// ── Where the credential lives ──────────────────────────────────────────────
//
// The provider's key is a RESOURCE CREDENTIAL (ADR-0009): it lives in the vault,
// behind `ports.SecretStore`, and the composition root resolves it
// (internal/app/agentproviders.go). This package does NOT import
// `ports.SecretStore`, does not receive a vault as a parameter and does not know
// a vault exists — it receives the value ready-made in the constructor. It is
// ADR-0016's entire point: the credential is read and used in the SAME process,
// and crosses no network boundary at all.
//
// And the key, once inside, does not leave: it is captured in an authorization
// CLOSURE instead of kept in a field. A struct field — even an unexported one —
// is printed by `%+v`, because fmt reads unexported fields through reflection and
// cannot call their String(). A closure prints as an address. It is the
// difference between "we promise not to log the key" and "there is no key to
// log".
//
// ── About deadlines and connections ─────────────────────────────────────────
//
// ADR-0016 recorded the change's price: "the core starts making LONG external
// calls". A generation at high effort takes minutes, and a 30s deadline — which
// is right for a git API call — would turn every expensive turn into a failure.
// Hence the generous `DefaultTimeout`. The CONNECTION budget is solved by reusing
// `http.DefaultTransport` (which is what a `&http.Client{Timeout: …}` with no
// Transport does): the pool belongs to the process, and not one per adapter built
// per request — otherwise one account per request would become one pool per
// request.
package agentprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/agent"
)

// DefaultTimeout is ONE generation's deadline. See the header: generous on
// purpose, because the alternative is turning expensive work into a failure.
const DefaultTimeout = 10 * time.Minute

// httpDoer allows injecting the client without exposing the transport to the
// composition root — the same choice as the gitprovider and secretstore
// adapters.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// client is the plumbing shared between the adapters.
//
// `authorize` is the closure that carries the key (see the package header).
// `redact` is the safety net: if one day somebody assembles a URL with the key
// inside, or the provider echoes a prefix of it in the error body, the message
// comes out redacted instead of leaking. Belt AND braces, because the cost of
// getting this wrong is somebody spending on another person's account.
type client struct {
	base      string
	http      httpDoer
	authorize func(*http.Request)
	redact    func(string) string
	who       string // "anthropic" | "openai", for the error message
	// hasCredential is a BOOLEAN, and not the key: the adapter needs to know
	// whether it has anything to talk with, and keeping the key to answer that
	// question would undo the closure.
	hasCredential bool
}

// String stops the instance from revealing anything when somebody writes
// `log.Info("…", "provider", p)`. A VALUE receiver on purpose: with a pointer
// receiver, `%+v` of a value would not call this method.
func (c client) String() string { return "agentprovider.client{" + c.who + "}" }

func newClient(base, who string, doer httpDoer, timeout time.Duration,
	authorize func(*http.Request), token string) *client {
	if doer == nil {
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		// No Transport of our own: the connection pool is the process's. See
		// the header — an adapter is built PER REQUEST (the provider is an
		// account resource), and a Transport per adapter would be a new pool
		// per turn.
		doer = &http.Client{Timeout: timeout}
	}
	return &client{
		base: strings.TrimRight(base, "/"), http: doer, authorize: authorize,
		redact: redactor(token), who: who, hasCredential: token != "",
	}
}

// redactor returns the function that erases the key from any text about to go
// up. An empty key returns the identity — without that,
// `strings.ReplaceAll(s, "", x)` would spread the marker between ALL the
// message's characters.
func redactor(token string) func(string) string {
	if token == "" {
		return func(s string) string { return s }
	}
	return func(s string) string { return strings.ReplaceAll(s, token, "***") }
}

// post makes the call and returns status + body.
//
// A transport error NEVER comes with a body: a transport error has no body, and
// returning both invites the caller to inspect bytes that do not exist.
func (c *client) post(ctx context.Context, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path,
		strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, agent.Unavailability(c.who, agent.ReasonUnreachable,
			"invalid request: "+c.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		// A blown deadline and a cancellation arrive here as transport errors,
		// and both are UNREACHABLE from the domain's point of view: in neither
		// case do we know what the provider did. "I do not know" must never
		// become "it did not happen".
		return 0, nil, agent.Unavailability(c.who, agent.ReasonUnreachable,
			c.redact(err.Error()))
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, agent.Unavailability(c.who, agent.ReasonUnreachable,
			"truncated response: "+c.redact(err.Error()))
	}
	return resp.StatusCode, out, nil
}

// failure translates the HTTP status into the port's reason (D6).
//
// The translation is the SAME in both providers because the guarantee is the
// port's, not the provider's. And the response BODY does not enter the message
// that goes up: it usually echoes headers and, in some errors, a prefix of the
// key. It goes into the detail, which is a log — and it still passes through the
// redactor first.
func (c *client) failure(status int, body []byte) *agent.Unavailable {
	var reason agent.UnavailableReason
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		reason = agent.ReasonRejectedCredential
	case status == http.StatusNotFound:
		// A 404 here is almost always a model name that does not exist in the
		// provider's catalog: the route is fixed and unique.
		reason = agent.ReasonUnknownModel
	default:
		reason = agent.ReasonProviderError
	}
	return agent.Unavailability(c.who, reason,
		"HTTP "+itoa(status)+": "+c.redact(strings.TrimSpace(string(body))))
}

// itoa avoids importing strconv just for this on the error path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// decodeText tries to read the model's structured output.
//
// A decoding failure is NOT a turn error: the provider may have returned loose
// text because the schema did not take, and in that case the raw utterance is
// still worth something to the human reading the thread (see `replyText` in the
// domain). Returning an error here would throw away the only thing the model
// produced — and the tokens already paid for it.
func decodeText(text string) map[string]any {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "{") {
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(t), &data); err != nil {
		return nil
	}
	return data
}
