// OneSignal as a Mailer.
//
// The third adapter behind the same port, and the reason the port speaks in
// KINDS instead of vendor identifiers: adding a provider changes a catalogue,
// not a policy. The package header calls this one out by name as the case the
// independent indexes exist for — this file is that day.
//
// ── What is different from SendGrid ─────────────────────────────────────────
//
// The body is rendered HERE, from the repository's templates, and not published
// to the provider. It is the SMTP shape, not SendGrid's, and it is deliberate:
// D-7 wants the message in the house's brand, and a template living in a
// provider's console is a piece of the product nobody reviews, nobody versions
// and nobody can diff.
//
// A note on the templates, learned the hard way: `html/template` STRIPS HTML
// comments. The `<!-- dop-template: NAME -->` marker at the top of each file
// does not survive rendering, so nothing downstream can read the kind off the
// body. It travels in the payload's `data`, the way SMTP puts it in an
// X-DOP-Kind header.
//
// ── The trap this adapter has and the others do not ─────────────────────────
//
// OneSignal answers **200 with an `errors` array** for a message that reached
// nobody — an unsubscribed address, an app with no e-mail channel configured.
// Reading only the status code would report a send that never happened, which
// is the exact silence ADR-0018 orders us to test for. `recipients` and
// `errors` are therefore read on the success path, not only on the failure one.
package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// oneSignalIndex is this provider's catalogue: kind → subject and body file.
//
// Its own map, on purpose, even though the files are shared with SMTP. The
// duplication that matters — and that the contract suite polices — is "a kind
// exists in the policy and this provider cannot resolve it". Sharing an index
// would make that oversight impossible to make and therefore impossible to
// catch on the day a fourth provider does not share it.
var oneSignalIndex = map[string]smtpTemplate{
	string(notification.KindInvite): {
		Subject: "Você foi convidado para uma conta no DOP",
		File:    "templates/smtp/invite.html",
	},
	string(notification.KindAttentionDigest): {
		Subject: "{{.total}} pendência(s) esperando você no DOP",
		File:    "templates/smtp/attention_digest.html",
	},
	string(notification.KindSecondFactorCode): {
		Subject: "{{.code}} é o seu código de verificação do DOP",
		File:    "templates/smtp/second_factor_code.html",
	},
	string(notification.KindEmailVerification): {
		Subject: "Confirme seu e-mail para entrar no DOP",
		File:    "templates/smtp/email_verification.html",
	},
}

type OneSignalConfig struct {
	// AppID identifies the OneSignal application. Empty is as good as no key:
	// there is nothing to send to.
	AppID string
	// APIKey is the REST credential, ALREADY RESOLVED. EMPTY turns on the LOCAL
	// DRY RUN — the same gesture as SendGrid's empty key and SMTP's empty
	// address.
	APIKey string
	// AuthScheme prefixes the Authorization header. OneSignal moved from
	// "Basic" to "Key" as it rotated its credential format, and both are alive
	// in the wild. It is configuration rather than a constant because getting it
	// wrong produces a 401 that reads exactly like a bad key.
	AuthScheme string
	// BaseURL exists so the contract suite can point at an httptest.Server.
	BaseURL  string
	From     string
	FromName string
	// ReplyTo is where a person's answer lands when the sender is a noreply
	// address. Empty sends no reply-to and the provider's default applies.
	ReplyTo string
	Timeout time.Duration
	Client  httpDoer
}

type OneSignal struct {
	base       string
	http       httpDoer
	authorize  func(*http.Request)
	redact     func(string) string
	dryRunMode bool
	appID      string
	from       string
	fromName   string
	replyTo    string
	bodies     *template.Template
	subjects   *texttemplate.Template
}

func NewOneSignal(cfg OneSignalConfig) *OneSignal {
	key := cfg.APIKey
	scheme := orDefault(cfg.AuthScheme, "Key")

	doer := cfg.Client
	if doer == nil {
		t := cfg.Timeout
		if t <= 0 {
			t = DefaultTimeout
		}
		doer = &http.Client{Timeout: t}
	}

	// missingkey=zero for the same reason SMTP uses it: a cosmetic field that
	// did not arrive must not drop the message (the port's guarantee 8).
	bodies := template.Must(template.New("onesignal").
		Option("missingkey=zero").
		ParseFS(htmlBodies, "templates/smtp/*.html"))
	subjects := texttemplate.New("onesignal-subjects").Option("missingkey=zero")
	for kind, spec := range oneSignalIndex {
		texttemplate.Must(subjects.New(kind).Parse(spec.Subject))
	}

	return &OneSignal{
		base: strings.TrimRight(orDefault(cfg.BaseURL, "https://api.onesignal.com"), "/"),
		http: doer,
		// The key lives in a CLOSURE and never in a field — see the package
		// header. A field is printed by %+v through reflection.
		authorize: func(r *http.Request) {
			if key != "" {
				r.Header.Set("Authorization", scheme+" "+key)
			}
		},
		redact:     redactor(key, cfg.AppID),
		dryRunMode: strings.TrimSpace(key) == "" || strings.TrimSpace(cfg.AppID) == "",
		appID:      cfg.AppID,
		from:       orDefault(cfg.From, DefaultFrom),
		fromName:   orDefault(cfg.FromName, DefaultFromName),
		replyTo:    cfg.ReplyTo,
		bodies:     bodies,
		subjects:   subjects,
	}
}

var _ ports.Mailer = (*OneSignal)(nil)

func (o OneSignal) String() string { return "mailer.OneSignal{}" }

// Resolve is guarantee 2: no I/O, nothing sent.
func (o *OneSignal) Resolve(_ context.Context, kind string) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return errs.Invalid("a notice with no kind: the channel has nothing to resolve")
	}
	spec, ok := oneSignalIndex[kind]
	if !ok {
		return unknownKind("OneSignal", kind, sortedKeys(oneSignalIndex))
	}
	// An index line whose file was not embedded is the same silence one step
	// further on: it would only fail in production.
	if o.bodies.Lookup(baseName(spec.File)) == nil {
		return errs.Precondition(
			"the %q notice is in OneSignal's index but the file %q was not embedded",
			kind, spec.File)
	}
	return nil
}

type osNotification struct {
	AppID string `json:"app_id"`
	// TargetChannel is what tells OneSignal this is e-mail and not a push. It is
	// the one field whose absence produces a valid-looking request that
	// delivers a push notification to nobody.
	TargetChannel      string   `json:"target_channel"`
	IncludeEmailTokens []string `json:"include_email_tokens"`
	EmailSubject       string   `json:"email_subject"`
	EmailBody          string   `json:"email_body"`
	EmailFromName      string   `json:"email_from_name,omitempty"`
	EmailFromAddress   string   `json:"email_from_address,omitempty"`
	EmailReplyTo       string   `json:"email_reply_to_address,omitempty"`
	// Data carries the kind to the provider, the same way the SMTP adapter puts
	// it in an X-DOP-Kind header. It is what makes a message findable in the
	// provider's console by what it IS rather than by its subject line — and
	// the subject is the one thing that changes when somebody edits a template.
	Data map[string]string `json:"data,omitempty"`
}

type osResponse struct {
	ID         string `json:"id"`
	Recipients int    `json:"recipients"`
	// Errors is either a list of strings or an object, depending on which way
	// OneSignal failed. It is decoded as raw JSON and only rendered, because
	// branching on its shape would be guessing at a provider's error format in
	// the one code path that runs when things are already wrong.
	Errors json.RawMessage `json:"errors"`
}

func (o *OneSignal) Send(ctx context.Context, m ports.Mail) (*ports.MailReceipt, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	// RESOLVE FIRST — in the dry run too (guarantee 3).
	if err := o.Resolve(ctx, m.Kind); err != nil {
		return nil, err
	}
	spec := oneSignalIndex[m.Kind]

	subject, err := o.render(o.subjects, m.Kind, m.Data)
	if err != nil {
		return nil, err
	}
	body, err := o.renderBody(baseName(spec.File), m.Data)
	if err != nil {
		return nil, err
	}

	if o.dryRunMode {
		return dryRun(ctx, "onesignal", m, subject, body), nil
	}

	raw, err := json.Marshal(osNotification{
		AppID:              o.appID,
		TargetChannel:      "email",
		IncludeEmailTokens: []string{m.To},
		EmailSubject:       subject,
		EmailBody:          body,
		EmailFromName:      o.fromName,
		EmailFromAddress:   o.from,
		EmailReplyTo:       o.replyTo,
		Data:               map[string]string{"dop_kind": m.Kind},
	})
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "notice unreadable for OneSignal")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/notifications",
		bytes.NewReader(raw))
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "invalid request for OneSignal")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dop-core")
	o.authorize(req)

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, errs.New(errs.KindUnavailable, "failed to talk to OneSignal: %s",
			o.redact(err.Error()))
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, o.failure(resp.StatusCode, payload)
	}

	// The 200 is not the answer. OneSignal reports "reached nobody" INSIDE a
	// success: an address that unsubscribed, an application with no e-mail
	// channel set up. Trusting the status code here would record MailSent for a
	// message that does not exist.
	var out osResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, errs.New(errs.KindInternal,
			"unreadable response from OneSignal: %s", o.redact(trim(string(payload))))
	}
	if len(out.Errors) > 0 && string(out.Errors) != "null" && string(out.Errors) != "[]" {
		return nil, errs.Invalid("OneSignal accepted the request and delivered nothing: %s",
			o.redact(trim(string(out.Errors))))
	}
	if out.Recipients == 0 {
		return nil, errs.Invalid(
			"OneSignal reported 0 recipients for %q: the address is not reachable through this application", m.To)
	}
	return &ports.MailReceipt{
		State:     ports.MailSent,
		Provider:  "onesignal",
		Reference: out.ID,
	}, nil
}

func (o *OneSignal) render(t *texttemplate.Template, kind string, data map[string]any) (string, error) {
	var b bytes.Buffer
	if err := t.ExecuteTemplate(&b, kind, data); err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "failed to build the subject of %q", kind)
	}
	return strings.TrimSpace(b.String()), nil
}

func (o *OneSignal) renderBody(name string, data map[string]any) (string, error) {
	var b bytes.Buffer
	if err := o.bodies.ExecuteTemplate(&b, name, data); err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "failed to render the body %q", name)
	}
	return b.String(), nil
}

// failure translates the provider's status into the port's Kind (guarantee 7).
func (o *OneSignal) failure(code int, body []byte) error {
	msg := o.redact(trim(string(body)))
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		// No provider detail: a 401/403 is one decision for whoever operates —
		// the credential does not work — and a 401 body is the likeliest place
		// for a provider to echo back what it received.
		return errs.New(errs.KindUnauthorized,
			"OneSignal refused this installation's credential (HTTP %d)", code)
	case code == http.StatusTooManyRequests || code >= 500:
		return errs.New(errs.KindUnavailable,
			"OneSignal did not accept the notice (HTTP %d): %s", code, msg)
	case code >= 400:
		return errs.Invalid("OneSignal refused the notice (HTTP %d): %s", code, msg)
	}
	return errs.Internal("unexpected response from OneSignal (HTTP %d): %s", code, msg)
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
