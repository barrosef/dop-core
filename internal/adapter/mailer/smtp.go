// A ports.Mailer adapter over SMTP.
//
// It is the self-hosted path — the same GCP/OKD pairing as the other ports — and
// it is what FORCES local template resolution. With SendGrid alone, nothing
// would stop the port from leaking `template_id`: the field would be there, the
// domain would end up filling it in, and the "port that speaks intent" would
// become a port that speaks SendGrid. Since there is no provider template here,
// the adapter renders from the files versioned in templates/smtp/ — and the port
// is obliged to speak in KINDS.
//
// The price, accepted by ADR-0025: the vendor's visual editor is lost.
//
// The Subject strings in the index below stay in Portuguese for the same reason
// as SendGrid's: they are the notification's CONTENT. Email localization is a
// pending item.
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The files are EMBEDDED in the binary, not read from disk at runtime. A
// filesystem path would make the same binary send a different email depending on
// where it was mounted — and the day the volume was not there, the notice would
// fail in production for a reason invisible at build time.
//
// The rendered bodies. NOT smtp-only despite the directory name: OneSignal
// renders locally too and uses these same files, because the brand is one and a
// second copy of the same HTML drifts from the first the week after it is made.
// What stays separate per adapter is the INDEX (kind → file), which is the
// thing the contract suite exists to catch a hole in.
//
//go:embed templates/smtp/*.html
var htmlBodies embed.FS

// smtpTemplate is one line of THIS provider's INDEX. An index INDEPENDENT of
// SendGrid's on purpose — see the package header.
type smtpTemplate struct {
	// Subject is a text template, and not a literal, because the subject is the
	// one place in the email where a number changes the decision to open it:
	// "3 items waiting for you" gets read; "You have items waiting" gets
	// archived.
	Subject string
	File    string
}

var smtpIndex = map[string]smtpTemplate{
	string(notification.KindInvite): {
		Subject: "Você foi convidado para uma conta no DOP",
		File:    "templates/smtp/invite.html",
	},
	string(notification.KindAttentionDigest): {
		Subject: "{{.total}} pendência(s) esperando você no DOP",
		File:    "templates/smtp/attention_digest.html",
	},
	string(notification.KindSecondFactorCode): {
		// The code goes in the SUBJECT as well, on purpose: it is what makes it
		// readable from the notification, without opening the message — which
		// is where a person actually reads it.
		Subject: "{{.code}} é o seu código de verificação do DOP",
		File:    "templates/smtp/second_factor_code.html",
	},
	string(notification.KindEmailVerification): {
		Subject: "Confirme seu e-mail para entrar no DOP",
		File:    "templates/smtp/email_verification.html",
	},
}

type SMTPConfig struct {
	// Addr is host:port. EMPTY turns on the LOCAL DRY RUN: it prints instead of
	// sending. It is the same gesture as SendGrid's empty key, and it is what
	// makes the development environment need no mail server at all.
	Addr     string
	Username string
	// Password is the credential's ALREADY RESOLVED value. It does NOT become a
	// field of this adapter — see the constructor.
	Password string
	From     string
	FromName string
	// StartTLS asks to promote the connection before authenticating. It is not
	// automatic: an internal lab server usually does not offer it, and always
	// trying would turn "no TLS" into "no email".
	StartTLS  bool
	TLSConfig *tls.Config
	Timeout   time.Duration
	// Dial exists so the contract suite can talk to a local test server,
	// without exposing the transport to the composition root — the same choice
	// as `Client` in the HTTP adapters.
	Dial func(ctx context.Context) (net.Conn, error)
}

type SMTP struct {
	addr string
	// authenticate carries the password in a CLOSURE. There is no `password`
	// field in this struct, and that absence is guarantee 4: `%+v` has nothing
	// to print.
	authenticate func(*smtp.Client) error
	redact       func(string) string
	dryRunMode   bool
	from         string
	fromName     string
	startTLS     bool
	tlsCfg       *tls.Config
	timeout      time.Duration
	dial         func(ctx context.Context) (net.Conn, error)
	bodies       *template.Template
	subjects     *texttemplate.Template
}

func NewSMTP(cfg SMTPConfig) *SMTP {
	password := cfg.Password
	username := cfg.Username

	// missingkey=zero: a missing key becomes empty, not "<no value>" and not an
	// error. It is the port's guarantee 8 — dropping an invite because a
	// cosmetic field did not arrive would trade an appearance problem for an
	// access block.
	bodies := template.Must(template.New("smtp").
		Option("missingkey=zero").
		ParseFS(htmlBodies, "templates/smtp/*.html"))

	subjects := texttemplate.New("subjects").Option("missingkey=zero")
	for kind, spec := range smtpIndex {
		texttemplate.Must(subjects.New(kind).Parse(spec.Subject))
	}

	t := cfg.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	s := &SMTP{
		addr:       cfg.Addr,
		redact:     redactor(password),
		dryRunMode: strings.TrimSpace(cfg.Addr) == "",
		from:       orDefault(cfg.From, DefaultFrom),
		fromName:   orDefault(cfg.FromName, DefaultFromName),
		startTLS:   cfg.StartTLS,
		tlsCfg:     cfg.TLSConfig,
		timeout:    t,
		dial:       cfg.Dial,
		bodies:     bodies,
		subjects:   subjects,
	}
	s.authenticate = func(c *smtp.Client) error {
		if username == "" || password == "" {
			// An internal relay server with no authentication is a legitimate
			// case, not an error. Demanding a credential here would rule out the
			// cluster's Postfix.
			return nil
		}
		host, _, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			host = cfg.Addr
		}
		return c.Auth(smtp.PlainAuth("", username, password, host))
	}
	return s
}

var _ ports.Mailer = (*SMTP)(nil)

// String: a VALUE receiver, so it also applies to `%+v` of a value.
func (s SMTP) String() string { return "mailer.SMTP{" + s.addr + "}" }

// Resolve is guarantee 2: it answers with no I/O and without sending.
func (s *SMTP) Resolve(_ context.Context, kind string) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return errs.Invalid("a notice with no kind: the channel has nothing to resolve")
	}
	spec, ok := smtpIndex[kind]
	if !ok {
		return unknownKind("SMTP", kind, sortedKeys(smtpIndex))
	}
	// An index with no file is the same silence, one step further on: the line
	// exists, `go:embed` did not bring the file, and the send would only fail in
	// production.
	if s.bodies.Lookup(baseName(spec.File)) == nil {
		return errs.Precondition(
			"the %q notice is in SMTP's index but the file %q was not embedded",
			kind, spec.File)
	}
	return nil
}

func (s *SMTP) Send(ctx context.Context, m ports.Mail) (*ports.MailReceipt, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	// RESOLVE FIRST — in the dry run too (guarantee 3).
	if err := s.Resolve(ctx, m.Kind); err != nil {
		return nil, err
	}
	spec := smtpIndex[m.Kind]

	subject, err := s.renderSubject(m.Kind, m.Data)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	if err := s.bodies.ExecuteTemplate(&body, baseName(spec.File), m.Data); err != nil {
		// A rendering failure is OUR error, not the provider's and not the
		// caller's: the template is in the binary.
		return nil, errs.Wrap(errs.KindInternal, err,
			"failed to render the %q notice", m.Kind)
	}

	if s.dryRunMode {
		return dryRun(ctx, "smtp", m, subject, body.String()), nil
	}
	if err := s.deliver(ctx, m, subject, body.Bytes()); err != nil {
		return nil, err
	}
	// No Reference: SMTP returns no identifier at all, and inventing one here
	// would make the record assert a traceability that does not exist (the
	// port's guarantee: an empty Reference is NORMAL).
	return &ports.MailReceipt{State: ports.MailSent, Provider: "smtp"}, nil
}

func (s *SMTP) renderSubject(kind string, data map[string]any) (string, error) {
	var b bytes.Buffer
	if err := s.subjects.ExecuteTemplate(&b, kind, data); err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "failed to build the subject of %q", kind)
	}
	return strings.TrimSpace(b.String()), nil
}

// deliver speaks SMTP by hand, instead of smtp.SendMail, for two reasons:
// SendMail takes no context (and a hung server would hold the worker until the
// operating system's timeout) and it decides about STARTTLS on its own.
func (s *SMTP) deliver(ctx context.Context, m ports.Mail, subject string, body []byte) error {
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	// The deadline goes on the CONNECTION: it is the only point where the
	// context reaches net/smtp, which knows no context.Context.
	deadline := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	host, _, errSplit := net.SplitHostPort(s.addr)
	if errSplit != nil {
		host = s.addr
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return s.unavailable("handshake", err)
	}
	defer func() { _ = c.Close() }()

	if s.startTLS {
		cfg := s.tlsCfg
		if cfg == nil {
			cfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		}
		if err := c.StartTLS(cfg); err != nil {
			return s.unavailable("STARTTLS", err)
		}
	}
	if err := s.authenticate(c); err != nil {
		// A refused credential is KindUnauthorized, not Unavailable: sending the
		// team hunting for a network problem when the problem is a password
		// costs hours.
		return errs.New(errs.KindUnauthorized,
			"the SMTP server refused this installation's credential: %s", s.redact(err.Error()))
	}
	if err := c.Mail(s.from); err != nil {
		return s.refusal("sender", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return s.refusal("recipient", err)
	}
	w, err := c.Data()
	if err != nil {
		return s.unavailable("opening the body", err)
	}
	if _, err := w.Write(s.message(m, subject, body)); err != nil {
		return s.unavailable("writing the body", err)
	}
	if err := w.Close(); err != nil {
		return s.refusal("body", err)
	}
	if err := c.Quit(); err != nil {
		// A failed QUIT after the body was accepted is NOT a delivery failure:
		// the server has already taken the message. Treating it as an error
		// would make the record say "it did not send" for an email that
		// arrived — and the resumption would send a second one.
		return nil
	}
	return nil
}

func (s *SMTP) connect(ctx context.Context) (net.Conn, error) {
	if s.dial != nil {
		conn, err := s.dial(ctx)
		if err != nil {
			return nil, s.unavailable("connection", err)
		}
		return conn, nil
	}
	d := net.Dialer{Timeout: s.timeout}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, s.unavailable("connection", err)
	}
	return conn, nil
}

func (s *SMTP) unavailable(stage string, err error) error {
	return errs.New(errs.KindUnavailable, "SMTP server unavailable at %s: %s",
		stage, s.redact(err.Error()))
}

// refusal tells 5xx (permanent: resending gives the same result) from 4xx
// (temporary: worth trying again) apart. Confusing the two either resends
// forever to an address that does not exist — which burns the sender's
// reputation — or gives up on a server that was merely busy.
func (s *SMTP) refusal(what string, err error) error {
	msg := s.redact(err.Error())
	var proto *textproto.Error
	if errors.As(err, &proto) && proto.Code >= 400 && proto.Code < 500 {
		return errs.New(errs.KindUnavailable, "the SMTP server deferred the %s: %s", what, msg)
	}
	return errs.Invalid("the SMTP server refused the %s: %s", what, msg)
}

// message assembles the RFC 5322. A minimal header on purpose: every extra
// header is one more chance for an antispam filter to find fault.
func (s *SMTP) message(m ports.Mail, subject string, body []byte) []byte {
	var b bytes.Buffer
	// mime.QEncoding: a subject in Portuguese has accents, and an unencoded
	// subject arrives with mangled characters in older clients.
	fmt.Fprintf(&b, "From: %s <%s>\r\n", mime.QEncoding.Encode("utf-8", s.fromName), s.from)
	if m.ToName != "" {
		fmt.Fprintf(&b, "To: %s <%s>\r\n", mime.QEncoding.Encode("utf-8", m.ToName), m.To)
	} else {
		fmt.Fprintf(&b, "To: %s\r\n", m.To)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/html; charset=utf-8\r\n")
	fmt.Fprintf(&b, "Content-Transfer-Encoding: 8bit\r\n")
	// X-DOP-Kind exists for operations: "why did I get this?" and "which notices
	// of this kind went out?" become one search on the mail server.
	fmt.Fprintf(&b, "X-DOP-Kind: %s\r\n", m.Kind)
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}

// baseName returns the name under which ParseFS registered the template: it uses
// the path's BASE, not the whole path.
func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// SMTPTemplateSource returns the embedded SOURCE of an SMTP template, by the
// file's base name.
//
// Exported for tests only, and it is worth it: it is what allows comparing which
// fields each of the two templates of the same notice consumes. Without it, the
// duplication ADR-0025 accepts as a cost ("two places for the same notice's
// template") would have nobody watching it — and the divergence between the two
// is silent: the email goes out in both providers, only one of them without the
// information that matters.
func SMTPTemplateSource(file string) (string, error) {
	b, err := htmlBodies.ReadFile("templates/smtp/" + baseName(file))
	if err != nil {
		return "", errs.NotFound("SMTP template %q was not embedded", file)
	}
	return string(b), nil
}

// SMTPTemplateFile returns the file SMTP's INDEX associates with a kind.
//
// Exported for tests, and for the same reason the index is exercised: the suite
// proves the adapter resolves the kind, but "resolving" and "resolving to the
// RIGHT artifact" are different things. Swapping two kinds' files in the index
// would send the wrong message with the right label — and that is worse than a
// missing template, because somebody RECEIVES something.
func SMTPTemplateFile(kind string) (string, error) {
	spec, ok := smtpIndex[kind]
	if !ok {
		return "", unknownKind("SMTP", kind, sortedKeys(smtpIndex))
	}
	return spec.File, nil
}
