// Package mailer implements the ports.Mailer port over SendGrid and over SMTP.
//
// The house pattern, for the same reasons as the gitprovider, secretstore and
// sandbox adapters: plain `net/http` and `net/smtp`, no vendor SDK. The SDK
// would bring the provider's vocabulary back into the process and drag a
// dependency tree along to make ONE HTTP call.
//
// ── What lives HERE and not in the domain ────────────────────────────────────
//
// The template INDEX, the RESOLUTION and the SENDING (ADR-0025). The port speaks
// INTENT — "invite created, to this address, with this data" — and each adapter
// decides what that becomes:
//
//	SendGrid  maps kind → template_id and sends dynamic_template_data
//	SMTP      renders locally, from the files versioned in this package
//
// It is the same separation the cost router already makes: `routingTable` is
// POLICY, `ModelCatalog` is CATALOG. Here the policy ("which notification exists
// and when") is in internal/domain/notification; the catalog ("which template of
// it in this provider") is in each file of this package. Changing provider
// changes the catalog, not the policy.
//
// It is SMTP that FORCES that separation to be real: with SendGrid alone,
// nothing would stop the port from leaking `template_id`. Since SMTP has no
// provider template at all, it renders from the files here — and the port is
// obliged to speak in kinds, not vendor identifiers.
//
// ── The two indexes are INDEPENDENT on purpose ──────────────────────────────
//
// An index shared by both adapters would look less repetitive and would destroy
// the one guarantee that matters: the contract suite exists to catch "I added a
// kind and forgot SendGrid's template", and with a single index that oversight
// would be impossible to make — while OneSignal's, on the day it exists, would
// still be possible. The duplication is the price; the suite is what stops one
// of them from being left behind.
//
// ── Where the secret lives ──────────────────────────────────────────────────
//
// SendGrid's key and SMTP's password are a CREDENTIAL: they live in the vault
// and arrive here already resolved by the composition root. This package does
// not import ports.SecretStore and does not know a vault exists.
//
// And, once inside, the secret does NOT LEAVE: it is captured in a CLOSURE,
// never kept in a struct field. A field — even an unexported one — is printed by
// `%+v`, because fmt reads unexported fields through reflection and cannot call
// their String(). A closure prints as an address. It is the difference between
// "we promise not to log the key" and "there is no key to log".
package mailer

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// DefaultTimeout covers the single call to the provider. Sending an email is a
// short request; a send that takes 30s is broken, not slow.
const DefaultTimeout = 30 * time.Second

// DefaultFrom is the sender of last resort. It exists so the local dry run and
// the contract suite need no configuration; in production the composition root
// provides the installation's sender.
const DefaultFrom = "noreply@dop.local"

// DefaultFromName likewise.
const DefaultFromName = "DOP"

// httpDoer allows injecting the client in the contract suite without exposing
// the transport to the composition root — the same choice as the other
// adapters.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// redactor returns the function that erases the secret from any text about to
// go up. An empty secret returns the identity — without that,
// `strings.ReplaceAll(s, "", x)` would spread the marker between ALL the
// message's characters.
func redactor(secrets ...string) func(string) string {
	var real []string
	for _, s := range secrets {
		if s != "" {
			real = append(real, s)
		}
	}
	if len(real) == 0 {
		return func(s string) string { return s }
	}
	return func(s string) string {
		for _, sec := range real {
			s = strings.ReplaceAll(s, sec, "***")
		}
		return s
	}
}

// validate is guarantee 5: an empty address, one with no "@", or an empty kind
// are KindInvalid decided WITHOUT I/O. Calling with no address must not cost a
// round trip to the provider.
func validate(m ports.Mail) error {
	if strings.TrimSpace(m.Kind) == "" {
		return errs.Invalid("a notice with no kind: the channel has nothing to resolve")
	}
	to := strings.TrimSpace(m.To)
	i := strings.LastIndex(to, "@")
	if i <= 0 || i >= len(to)-1 || strings.ContainsAny(to, " \t\r\n") {
		// The message does NOT echo the whole address: an error goes up to the
		// log, and a log is where personal data becomes data at rest.
		return errs.Invalid("invalid recipient for the %q notice", m.Kind)
	}
	return nil
}

// unknownKind is guarantee 1, written once for both adapters.
//
// The message says what to DO, and not only what was missing, because this is
// the error the contract suite exists to provoke: whoever sees it has just added
// a notification kind and has not yet given it a template in some provider.
func unknownKind(who, kind string, known []string) error {
	return errs.NotFound(
		"channel %s has no template for the %q notice (it knows: %s) — a kind that exists "+
			"in the policy and has no template in the provider fails in SILENCE: the event "+
			"happens, the consumer runs and nobody receives anything (ADR-0025)",
		who, kind, strings.Join(known, ", "))
}

// dryRun is the sibling project's development mode: with no credential, it
// prints instead of sending.
//
// It happens AFTER the template resolution, never before (guarantee 3). A dry
// run that skipped the resolution would hide exactly guarantee 1's defect in
// every environment with no key — which is where the suite runs, and where the
// developer works.
//
// It goes out through the logger, and not through `fmt.Println`, because this
// house's output format is canonical JSON: a loose print would be the one line
// the log collection could not read.
func dryRun(ctx context.Context, who string, m ports.Mail, subject string, body string) *ports.MailReceipt {
	log := logging.From(ctx)
	log.Info("email DRY RUN: no credential, nothing was sent",
		"channel", who,
		"kind", m.Kind,
		"to", m.To,
		"subject", subject,
		"data", m.Data,
		"body_bytes", len(body),
	)
	return &ports.MailReceipt{State: ports.MailSentLocal, Provider: who}
}

// sortedKeys returns an index's kinds, in stable order. A Go map iterates
// randomly, and an error message that changes order between runs is impossible
// to match in a test and annoying to read in a log.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
