package contract_test

// The Mailer's contract suite against the REAL SendGrid adapter, with an
// httptest.Server on the other side of the wire.
//
//	go test ./test/contract/ -run Mailer -v
//
// It ALWAYS runs — no tag, no infrastructure, no key. It is a condition for the
// suite to really exist: this house's lesson is that a suite that does not run
// verifies nothing (SecretStore's k8s adapter spent months never having been
// exercised).
//
// SendGrid's API is never really called. See mailer_fakes.go's header for what
// the double proves and what it does not.

import (
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/mailer"
	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/test/contract"
)

// sendGridKey is the secret the doubles echo back. A value that looks like a
// real key on purpose: a secret of "abc" would match any text by accident and
// guarantee 4 would start reporting false positives.
const sendGridKey = "SG.4nZk-test-DO-NOT-USE.9xQv7hJp2LmR0tWc"

// testIDs is kind → `template_id`, built from `notification.Kinds()`.
//
// Derived, and not a literal: a literal here would need updating with every new
// kind, and whoever forgot would see the suite fail for a MISSING CONFIGURATION,
// not for a MISSING TEMPLATE — which is the failure that matters. With the
// derived map, the only way subtest 1 can fail is the adapter's index being
// incomplete.
func testIDs() map[string]string {
	out := map[string]string{}
	for _, k := range notification.KindNames() {
		out[k] = "d-" + k
	}
	return out
}

func TestMailerContractSendGrid(t *testing.T) {
	ids := testIDs()
	newMailer := func(t *testing.T, f contract.Failure, withKey bool) (ports.Mailer, *contract.Inbox) {
		url, inbox := contract.NewSendGridDouble(t, ids, f, sendGridKey)
		key := sendGridKey
		if !withKey {
			key = "" // ensaio local
		}
		return mailer.NewSendGrid(mailer.SendGridConfig{
			APIKey:    key,
			BaseURL:   url,
			From:      "avisos@dop.test",
			FromName:  "DOP",
			Templates: ids,
		}), inbox
	}

	contract.MailerSuite(t, "sendgrid", contract.MailerHarness{
		Secret: sendGridKey,
		New: func(t *testing.T) (ports.Mailer, *contract.Inbox) {
			return newMailer(t, "", true)
		},
		NewFailing: func(t *testing.T, f contract.Failure) (ports.Mailer, *contract.Inbox) {
			return newMailer(t, f, true)
		},
		NewDryRun: func(t *testing.T) (ports.Mailer, *contract.Inbox) {
			return newMailer(t, "", false)
		},
	})
}

// A template that is configured but ABSENT at the provider is ADR-0025's other
// half of silence: the kind is in the index, the id is in the configuration, and
// the `d-…` points at nothing. SendGrid answers 400; the adapter has to say it
// was a content refusal, and not an unavailability — otherwise the worker keeps
// resending forever a template that does not exist.
func TestMailerSendGridAMissingTemplateDoesNotBecomeAnEternalRetry(t *testing.T) {
	ids := testIDs()
	url, _ := contract.NewSendGridDouble(t, ids, contract.FailureContent, sendGridKey)
	m := mailer.NewSendGrid(mailer.SendGridConfig{
		APIKey: sendGridKey, BaseURL: url, Templates: ids,
	})
	_, err := m.Send(t.Context(), ports.Mail{
		AccountID: "acct-1", Kind: notification.KindNames()[0], To: "someone@example.test",
	})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if got := errsKindOf(err); got != "invalid_argument" {
		t.Fatalf("expected invalid_argument (permanent), got %v: %v", got, err)
	}
}

// WITHOUT a configured id, and WITH a key, the adapter has to refuse with a
// PRECONDITION — an assembly error, not a policy one. The distinction exists so
// whoever reads the error knows whether a line of code or a deploy variable is
// missing.
func TestMailerSendGridWithNoConfiguredIDIsAnAssemblyError(t *testing.T) {
	url, inbox := contract.NewSendGridDouble(t, testIDs(), "", sendGridKey)
	m := mailer.NewSendGrid(mailer.SendGridConfig{
		APIKey: sendGridKey, BaseURL: url, // Templates left empty
	})
	kind := notification.KindNames()[0]
	if got := errsKindOf(m.Resolve(t.Context(), kind)); got != "failed_precondition" {
		t.Fatalf("expected failed_precondition, got %v", got)
	}
	if _, err := m.Send(t.Context(), ports.Mail{
		AccountID: "c", Kind: kind, To: "a@b.test",
	}); errsKindOf(err) != "failed_precondition" {
		t.Fatalf("Send with no id: expected failed_precondition, got %v", err)
	}
	if inbox.Calls() != 0 {
		t.Fatalf("an assembly error cost %d round trip(s) to the provider", inbox.Calls())
	}
}
