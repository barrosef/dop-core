package contract_test

// The SAME contract suite, against the REAL SMTP adapter and a local test
// server.
//
//	go test ./test/contract/ -run Mailer -v
//
// SMTP is the adapter that FORCES local template resolution: it has no provider
// template at all. If the suite only ran against SendGrid, nothing would stop
// the port from leaking `template_id` — and the "port that speaks intent" would
// become a port that speaks SendGrid without any test noticing.

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/mailer"
	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// smtpPassword is the secret the double echoes in the AUTH refusal.
const smtpPassword = "r3l4y-password-DO-NOT-USE-9xQv7hJp"

func TestMailerContractSMTP(t *testing.T) {
	novo := func(t *testing.T, f contract.Failure, comServidor bool) (ports.Mailer, *contract.Inbox) {
		addr, inbox := contract.NewSMTPDouble(t, f, smtpPassword)
		if !comServidor {
			addr = "" // the local dry run: with no address, it prints instead of sending
		}
		return mailer.NewSMTP(mailer.SMTPConfig{
			Addr:     addr,
			Username: "dop",
			Password: smtpPassword,
			From:     "avisos@dop.test",
			FromName: "DOP",
		}), inbox
	}

	contract.MailerSuite(t, "smtp", contract.MailerHarness{
		Secret: smtpPassword,
		New: func(t *testing.T) (ports.Mailer, *contract.Inbox) {
			return novo(t, "", true)
		},
		NewFailing: func(t *testing.T, f contract.Failure) (ports.Mailer, *contract.Inbox) {
			return novo(t, f, true)
		},
		NewDryRun: func(t *testing.T) (ports.Mailer, *contract.Inbox) {
			return novo(t, "", false)
		},
	})
}

// SMTP really renders, so we can demand from it what SendGrid does not let us
// verify from here: that the DATA reached the body.
//
// Without this, a template that ignored `items` would pass the whole suite — the
// email would go out, with the right subject, saying "2 pending items" and
// listing none.
func TestMailerSMTPRendersTheDataIntoTheBody(t *testing.T) {
	addr, inbox := contract.NewSMTPDouble(t, "", smtpPassword)
	m := mailer.NewSMTP(mailer.SMTPConfig{Addr: addr, From: "avisos@dop.test"})

	_, err := m.Send(t.Context(), ports.Mail{
		AccountID: "acct-1",
		Kind:      string(notification.KindAttentionDigest),
		To:        "dev@exemplo.test",
		Data: map[string]any{
			"total": 2,
			"link":  "https://cockpit.exemplo.test/atencao",
			"items": []map[string]any{
				{"kind": "thread_blocked", "title": "Um agente precisa de resposta", "summary": "thread 7"},
				{"kind": "pr_review", "title": "PR aguardando revisão"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := inbox.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	body := msgs[0].Body
	for _, want := range []string{
		"Um agente precisa de resposta",
		"PR aguardando revisão",
		"thread 7",
		"https://cockpit.exemplo.test/atencao",
	} {
		if !contains(body, want) {
			t.Errorf("the rendered body does NOT contain %q — the email would go out saying "+
				"there are pending items and listing none", want)
		}
	}
	// A MISSING key must not become "<no value>" in the user's face: the second
	// item has no `summary`, and the template has to handle that quietly.
	if contains(body, "<no value>") {
		t.Errorf("the body came out with \"<no value>\": Option(missingkey=zero) is missing")
	}
}

// The digest's subject carries the NUMBER, and that is a product decision, not
// cosmetics: "3 items waiting for you" gets read; "You have items waiting" gets
// archived. A subject that lost the number would pass the whole suite.
func TestMailerSMTPDigestSubjectCarriesTheNumber(t *testing.T) {
	addr, inbox := contract.NewSMTPDouble(t, "", smtpPassword)
	m := mailer.NewSMTP(mailer.SMTPConfig{Addr: addr})
	if _, err := m.Send(t.Context(), ports.Mail{
		AccountID: "acct-1", Kind: string(notification.KindAttentionDigest),
		To: "dev@exemplo.test", Data: map[string]any{"total": 7},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := inbox.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !contains(msgs[0].Subject, "7") {
		t.Fatalf("the digest's subject lost the number: %q", msgs[0].Subject)
	}
}
