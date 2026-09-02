// A ports.Mailer adapter over SendGrid.
//
// ── What this adapter solves, and what it does NOT do ────────────────────────
//
// It maps KIND → `template_id` and sends `dynamic_template_data`. It does NOT
// render: the HTML lives at SendGrid, published from the files versioned in
// templates/sendgrid/ by the idempotent script (publish_templates.go). It is
// that choice that preserves the vendor's visual editor, versioning and
// localization — things the platform would lose if the domain rendered and the
// port carried a blob of HTML.
//
// ── Why the index is compiled and the IDs are configuration ──────────────────
//
// They are two questions on different clocks, exactly like `routingTable` and
// `ModelCatalog` in the cost router:
//
//   - WHICH KINDS this adapter serves is a fact of the CODE, and changes along
//     with the policy. That is why `sendgridIndex` is a literal, and it is what
//     the contract suite exercises: adding a kind in `notification` without
//     adding a line here fails;
//   - WHAT THE ID of each template is is a fact of the INSTALLATION — the
//     client's SendGrid `d-…` is not ours. That is why it comes from
//     configuration, and its absence is KindPrecondition (an assembly error),
//     not KindNotFound (a policy error).
//
// The Name/Subject strings below stay in Portuguese: they are the notification's
// CONTENT, not code, and translating them one by one would replace one hardcoded
// locale with another. Email localization is a pending item — a template per
// locale at the provider, chosen by the recipient's language.
package mailer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// sgTemplate is one line of the INDEX: what this provider knows how to build.
type sgTemplate struct {
	// Name is the template's name INSIDE SendGrid. It is the identity the
	// publishing script uses to decide between creating and updating — changing
	// it creates a new template instead of versioning the existing one.
	Name string
	// Subject goes into the published version's `subject`. It lives here, and
	// not in the HTML file, because SendGrid keeps the subject separate from the
	// body.
	Subject string
	// File is the file versioned in this repository that the script publishes.
	File string
}

// sendgridIndex — THE INDEX. Keyed by the domain's vocabulary, not by a loose
// string: renaming a Kind breaks compilation here, which is where it needs to
// break.
var sendgridIndex = map[string]sgTemplate{
	string(notification.KindInvite): {
		Name:    "DOP — Convite para conta",
		Subject: "Você foi convidado para uma conta no DOP",
		File:    "invite.html",
	},
	string(notification.KindAttentionDigest): {
		Name:    "DOP — Resumo da caixa de atenção",
		Subject: "Você tem pendências esperando",
		File:    "attention_digest.html",
	},
	string(notification.KindSecondFactorCode): {
		Name:    "DOP — Código de verificação",
		Subject: "{{code}} é o seu código de verificação do DOP",
		File:    "second_factor_code.html",
	},
}

type SendGridConfig struct {
	// APIKey is the credential's ALREADY RESOLVED value. This package does not
	// know ports.SecretStore. EMPTY turns on the LOCAL DRY RUN: it prints
	// instead of sending.
	APIKey string
	// BaseURL is https://api.sendgrid.com on the public service. It exists so
	// the contract suite can point at an httptest.Server — SendGrid's API is
	// never really called in a test.
	BaseURL  string
	From     string
	FromName string
	// Templates is kind → `template_id`, coming from the installation's configuration.
	Templates map[string]string
	Timeout   time.Duration
	Client    httpDoer
}

type SendGrid struct {
	base string
	http httpDoer
	// authorize carries the key in a CLOSURE — see the package header. There is
	// no `apiKey` field in this struct, and that absence is guarantee 4.
	authorize func(*http.Request)
	redact    func(string) string
	// dryRunMode is a BOOLEAN, derived from the key in the constructor. Keeping
	// the boolean instead of the key is what allows deciding the mode without
	// holding on to the secret.
	dryRunMode bool
	from       string
	fromName   string
	templates  map[string]string
}

func NewSendGrid(cfg SendGridConfig) *SendGrid {
	base := orDefault(cfg.BaseURL, "https://api.sendgrid.com")
	key := cfg.APIKey
	doer := cfg.Client
	if doer == nil {
		t := cfg.Timeout
		if t <= 0 {
			t = DefaultTimeout
		}
		doer = &http.Client{Timeout: t}
	}
	ids := map[string]string{}
	for k, v := range cfg.Templates {
		if strings.TrimSpace(v) != "" {
			ids[k] = v
		}
	}
	return &SendGrid{
		base: strings.TrimRight(base, "/"),
		http: doer,
		authorize: func(r *http.Request) {
			if key != "" {
				r.Header.Set("Authorization", "Bearer "+key)
			}
		},
		redact:     redactor(key),
		dryRunMode: key == "",
		from:       orDefault(cfg.From, DefaultFrom),
		fromName:   orDefault(cfg.FromName, DefaultFromName),
		templates:  ids,
	}
}

var _ ports.Mailer = (*SendGrid)(nil)

// String: a VALUE receiver, so it also applies to `%+v` of a value. Without it,
// fmt would walk the fields through reflection — including the unexported ones,
// whose String() it cannot call.
func (s SendGrid) String() string { return "mailer.SendGrid{}" }

// Resolve is guarantee 2: it answers with no I/O and without sending.
func (s *SendGrid) Resolve(_ context.Context, kind string) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return errs.Invalid("a notice with no kind: the channel has nothing to resolve")
	}
	if _, ok := sendgridIndex[kind]; !ok {
		return unknownKind("SendGrid", kind, sortedKeys(sendgridIndex))
	}
	// In a dry run the id is not needed: there is no call to the provider to
	// make. Demanding the id here would make the contract suite — and every
	// developer's machine — require production configuration to prove a policy
	// guarantee.
	if s.dryRunMode {
		return nil
	}
	if s.templates[kind] == "" {
		return errs.Precondition(
			"the template for the %q notice is not configured in this SendGrid: publish "+
				"with publish_templates.go and provide the resulting id", kind)
	}
	return nil
}

// sgMail is `/v3/mail/send`'s shape — only what the port uses.
type sgMail struct {
	From             sgAddr              `json:"from"`
	Personalizations []sgPersonalization `json:"personalizations"`
	TemplateID       string              `json:"template_id"`
}

type sgAddr struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type sgPersonalization struct {
	To []sgAddr `json:"to"`
	// DynamicTemplateData is what the provider's template consumes. It is where
	// the port's `Data` flows into, untranslated: the port speaks intent and
	// data, and SendGrid consumes data.
	DynamicTemplateData map[string]any `json:"dynamic_template_data,omitempty"`
}

type sgError struct {
	Errors []struct {
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"errors"`
}

func (s *SendGrid) Send(ctx context.Context, m ports.Mail) (*ports.MailReceipt, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	// RESOLVE FIRST — in the dry run too (guarantee 3).
	if err := s.Resolve(ctx, m.Kind); err != nil {
		return nil, err
	}
	spec := sendgridIndex[m.Kind]

	if s.dryRunMode {
		return dryRun(ctx, "sendgrid", m, spec.Subject, ""), nil
	}

	payload := sgMail{
		From:       sgAddr{Email: s.from, Name: s.fromName},
		TemplateID: s.templates[m.Kind],
		Personalizations: []sgPersonalization{{
			To:                  []sgAddr{{Email: m.To, Name: m.ToName}},
			DynamicTemplateData: withSubject(m.Data, spec.Subject),
		}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "notice unreadable for SendGrid")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v3/mail/send",
		strings.NewReader(string(raw)))
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "invalid request for SendGrid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dop-core")
	s.authorize(req)

	resp, err := s.http.Do(req)
	if err != nil {
		// net/http's message carries the URL; the URL does not carry the key
		// (authorization goes in a header), but the redactor passes over it
		// anyway — it is cheap and it covers the day somebody changes that.
		return nil, errs.New(errs.KindUnavailable, "failed to talk to SendGrid: %s",
			s.redact(err.Error()))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &ports.MailReceipt{
			State:    ports.MailSent,
			Provider: "sendgrid",
			// SendGrid returns the id in the header, not in the body — a 202
			// with no body is its normal response.
			Reference: resp.Header.Get("X-Message-Id"),
		}, nil
	}
	return nil, s.failure(resp.StatusCode, body)
}

// failure translates the provider's status into the port's Kind (guarantee 7).
func (s *SendGrid) failure(code int, body []byte) error {
	msg := s.redact(explainSG(body))
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		// No provider detail, on purpose: a 401/403 is always the same decision
		// for whoever operates — the credential does not work — and a 401's body
		// is the likeliest place for a provider to echo what it received.
		return errs.New(errs.KindUnauthorized,
			"SendGrid refused this installation's credential (HTTP %d)", code)
	case code == http.StatusTooManyRequests || code >= 500:
		return errs.New(errs.KindUnavailable,
			"SendGrid did not accept the notice (HTTP %d): %s", code, msg)
	case code >= 400:
		// A 400 from SendGrid is content or an address refused: it is the
		// caller's error, and resending the same gives the same result.
		return errs.Invalid("SendGrid refused the notice (HTTP %d): %s", code, msg)
	}
	return errs.Internal("unexpected response from SendGrid (HTTP %d): %s", code, msg)
}

// explainSG flattens SendGrid's error body into one sentence.
func explainSG(body []byte) string {
	var e sgError
	if err := json.Unmarshal(body, &e); err != nil || len(e.Errors) == 0 {
		s := strings.TrimSpace(string(body))
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		return s
	}
	parts := make([]string, 0, len(e.Errors))
	for _, it := range e.Errors {
		p := strings.TrimSpace(it.Message)
		if it.Field != "" {
			p = it.Field + ": " + p
		}
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "; ")
}

// withSubject injects the subject into the data, as the sibling project does:
// the published template uses `{{subject}}`, which allows changing the subject
// without republishing a new version of the HTML.
//
// A copy, and not a write into the received map: the caller reuses the same
// `Data` for every recipient of a digest, and mutating that inside the adapter
// is the kind of side effect that only shows up on the second recipient.
func withSubject(d map[string]any, subject string) map[string]any {
	out := make(map[string]any, len(d)+1)
	for k, v := range d {
		out[k] = v
	}
	out["subject"] = subject
	return out
}
