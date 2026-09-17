// Package gitprovider implements the delivery.GitProvider port over GitHub's and
// GitLab's APIs.
//
// The house pattern, for the same reasons as the secretstore and sandbox
// adapters: plain `net/http`, no vendor SDK. The SDK would bring the provider's
// vocabulary back into the process (`github.PullRequest`, `gitlab.MergeRequest`
// types) and the first time somebody passed them along the port would have
// leaked. Beyond that, both official SDKs drag large dependency trees along to
// make six HTTP calls.
//
// ── Where the token lives ────────────────────────────────────────────────────
//
// The provider's token is a RESOURCE CREDENTIAL (ADR-0009): it lives in the
// vault, behind ports.SecretStore, and the composition root resolves it. This
// package does NOT import ports.SecretStore, does not receive a vault as a
// parameter and does not know a vault exists — it receives the value ready-made
// in the constructor. The reason is ADR-0001's: a git adapter that knew how to
// query the vault would have two responsibilities, and one of them would be
// impossible to test without infrastructure.
//
// And the token, once inside, does not leave: it is captured in an authorization
// CLOSURE instead of kept in a field. A struct field — even an unexported one —
// is printed by `%+v`, because fmt reads unexported fields through reflection and
// cannot call their String(). A closure prints as an address. It is the
// difference between "we promise not to log the token" and "there is no token to
// log".
package gitprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/platform/errs"
)

// DefaultTimeout covers the single call. The rebase, which is asynchronous in
// both providers (the port's guarantee 9), has a deadline of its own — see
// RebaseTimeout.
const DefaultTimeout = 30 * time.Second

// DefaultRebaseTimeout is how long the adapter waits for the provider to FINISH
// reapplying before giving up. It has to be generous: in GitLab the rebase is a
// Sidekiq queue task, and in a large repository at a busy hour it takes a while.
// Giving up early returns KindUnavailable — which is honest — but turns a slow
// queue into a failure, so the default is high on purpose.
const DefaultRebaseTimeout = 3 * time.Minute

// defaultPoll is the interval between two reads of the rebase's state.
const defaultPoll = 2 * time.Second

// httpDoer allows injecting the client in the contract suite without exposing
// the transport to the composition root — the same choice as the secretstore
// adapter, where `Client` exists "for tests".
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// client is the plumbing shared between GitHub and GitLab.
//
// `authorize` is the closure that carries the token (see the package header).
// `redact` is the safety net: if one day somebody assembles a URL with the token
// inside, or the provider returns the token in the error body, the message comes
// out redacted instead of leaking. Belt AND braces, because the cost of getting
// this wrong is somebody opening a PR and merging as the account's owner.
type client struct {
	base      string
	http      httpDoer
	authorize func(*http.Request)
	redact    func(string) string
	who       string // "GitHub" | "GitLab", for the error message
}

// String stops the whole instance from revealing anything when somebody writes
// `log.Info("...", "provider", p)`. A VALUE receiver on purpose: with a pointer
// receiver, `%+v` of a value would not call this method.
func (c client) String() string { return "gitprovider.client{" + c.who + "}" }

func newClient(base, who string, doer httpDoer, timeout time.Duration, authorize func(*http.Request), token string) *client {
	if doer == nil {
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		// No custom CA, unlike the k8s adapters: here the certificate is public
		// (api.github.com) or belongs to a self-hosted installation that already
		// has to be in the system bundle. Inventing a pool here would only
		// create one more place for TLS to break in silence.
		doer = &http.Client{Timeout: timeout}
	}
	return &client{base: strings.TrimRight(base, "/"), http: doer, authorize: authorize,
		redact: redactor(token), who: who}
}

// redactor returns the function that erases the token from any text about to go
// up. An empty token returns the identity — without that,
// `strings.ReplaceAll(s, "", x)` would spread the marker between ALL the
// message's characters.
func redactor(token string) func(string) string {
	if token == "" {
		return func(s string) string { return s }
	}
	return func(s string) string { return strings.ReplaceAll(s, token, "***") }
}

// do makes the call and returns status + body. It never returns the body
// alongside a transport error: a transport error has no body, and returning both
// invites the caller to inspect bytes that do not exist.
func (c *client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, errs.Wrap(errs.KindInternal, err, "request unreadable for %s", c.who)
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, nil, errs.Wrap(errs.KindInternal, err, "invalid request for %s", c.who)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		// net/http's message carries the URL; the URL does not carry the token
		// (authorization goes in a header), but the redactor passes over it
		// anyway — it is cheap and it covers the day somebody changes that.
		return 0, nil, errs.New(errs.KindUnavailable, "failed to talk to %s: %s",
			c.who, c.redact(err.Error()))
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, errs.New(errs.KindUnavailable,
			"truncated response from %s: %s", c.who, c.redact(err.Error()))
	}
	return resp.StatusCode, out, nil
}

// fail translates the HTTP status into the port's Kind (guarantee 11).
//
// The translation is the SAME in both providers because the guarantee is the
// port's, not the vendor's. What changes is where the readable message comes
// from — and each adapter solves that with its own `explain`.
func (c *client) fail(code int, msg, what string) error {
	msg = strings.TrimSpace(c.redact(msg))
	switch code {
	case http.StatusUnauthorized:
		// No provider detail, on purpose: a 401 is always the same decision for
		// whoever operates — the credential does not work — and a 401's body is
		// the likeliest place for a provider to echo what it received.
		return errs.New(errs.KindUnauthorized,
			"%s refused this resource's credential (HTTP 401)", c.who)
	case http.StatusForbidden:
		return errs.Permission("%s denied %s: %s", c.who, what, msg)
	case http.StatusNotFound:
		// See guarantee 11: GitHub answers 404 for a private repository the
		// token cannot see, and there is no way from here to know whether it is
		// gone or invisible. The message states both possibilities for whoever
		// investigates.
		return errs.NotFound("%s at %s — nonexistent or outside this credential's reach", what, c.who)
	}
	if code == http.StatusTooManyRequests || code >= 500 {
		return errs.New(errs.KindUnavailable, "%s did not serve %s (HTTP %d): %s", c.who, what, code, msg)
	}
	return errs.Internal("%s refused %s (HTTP %d): %s", c.who, what, code, msg)
}

// decode deserializes with a useful message. An unreadable response is
// KindUnavailable, and not Internal: almost always it is a proxy or an
// authentication portal answering HTML in the provider's place — a problem of
// the path, not of our code.
func (c *client) decode(body []byte, v any, what string) error {
	if err := json.Unmarshal(body, v); err != nil {
		return errs.New(errs.KindUnavailable,
			"unreadable response from %s at %s: %s", c.who, what, c.redact(err.Error()))
	}
	return nil
}

// wait sleeps while respecting the context. It returns KindUnavailable on
// cancellation, which is guarantee 9: "I do not know" never becomes "it did not
// conflict".
func wait(ctx context.Context, d time.Duration, what string) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return errs.New(errs.KindUnavailable, "%s: %v", what, ctx.Err())
	case <-t.C:
		return nil
	}
}

// checkActor is guarantee 13, written once for both adapters.
//
// Refusing is better than ignoring: one instance speaks for ONE credential, and
// a request on behalf of another actor would open the PR signed by whoever owns
// the token instead. ADR-0003 requires whoever drove to sign — silencing this
// would turn that requirement into a lie nobody would see.
func checkActor(instance, requested, op string) error {
	if requested == "" || instance == "" || requested == instance {
		return nil
	}
	return errs.Permission(
		"this connection to the provider speaks for actor %q; %s was requested on behalf of %q "+
			"(ADR-0003: whoever drove signs) — build the connection with the right actor's credential",
		instance, op, requested)
}

// textOf flattens what the providers return in `message`: GitHub sends a string,
// GitLab sends sometimes a string, sometimes a list of strings, sometimes an
// object mapping a field to a list of errors. Flattening here avoids three
// different decoders.
func textOf(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := textOf(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "; ")
	case map[string]any:
		parts := make([]string, 0, len(t))
		for k, e := range t {
			if s := textOf(e); s != "" {
				parts = append(parts, k+": "+s)
			}
		}
		// A stable order: a Go map iterates randomly, and an error message that
		// changes order between runs is impossible to match in a test and
		// annoying to read in a log.
		sortStrings(parts)
		return strings.Join(parts, "; ")
	default:
		return fmt.Sprint(t)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// tolerantUnmarshal exists only for the ERROR path: when the provider (or a
// proxy in between) returns something that is not the expected JSON, we want
// whatever message can be extracted, not a second error on top of the first.
func tolerantUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// instant converts the providers' ISO-8601 date. An unreadable date becomes the
// ZERO instant instead of an error: the port's guarantee is about identity and
// conflict, and bringing down the opening of a PR over a badly formatted time
// zone would trade a cosmetic problem for a delivery block.
func instant(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func instantPtr(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	return instant(*s)
}

// unixOf returns 0 — and not the zero instant's Unix time, which is
// -62135596800 — for an absent date. It is the difference between "it has not
// merged yet" and "it merged in the year 1".
func unixOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
