package contract_test

// The Mailer's contract suite against the REAL OneSignal adapter, with an
// httptest.Server on the other side of the wire.
//
//	go test ./test/contract/ -run Mailer -v
//
// It ALWAYS runs — no tag, no infrastructure, no key. Same reason as the other
// two: a suite that does not run verifies nothing.
//
// What this adapter is here to prove that the others cannot: a provider can
// answer 200 and have delivered to nobody. See NewOneSignalDouble.

import (
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/mailer"
	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/test/contract"
)

// A value shaped like a real credential: a secret of "abc" would match text by
// accident and guarantee 4 would start reporting false positives.
const oneSignalKey = "os_v2_app_test_DO_NOT_USE_4nZk9xQv7hJp2LmR0tWc"

const oneSignalAppID = "8f5c1e2a-0000-4c3d-9b7e-testappid0001"

func TestMailerContractOneSignal(t *testing.T) {
	newMailer := func(t *testing.T, f contract.Failure, withKey bool) (ports.Mailer, *contract.Inbox) {
		url, inbox := contract.NewOneSignalDouble(t, f, oneSignalKey)
		key := oneSignalKey
		if !withKey {
			key = "" // ensaio local
		}
		return mailer.NewOneSignal(mailer.OneSignalConfig{
			AppID:    oneSignalAppID,
			APIKey:   key,
			BaseURL:  url,
			From:     "avisos@dop.test",
			FromName: "DOP",
		}), inbox
	}

	contract.MailerSuite(t, "onesignal", contract.MailerHarness{
		Secret: oneSignalKey,
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

// The failure that belongs to this provider and to no other: HTTP 200, and
// nothing delivered.
//
// Reading only the status code would record MailSent for a message that does
// not exist — and the notification would be marked as handled, so nobody would
// ever resend it. It is ADR-0018's silence wearing a success code.
func TestMailerOneSignalASuccessThatReachedNobodyIsNotASuccess(t *testing.T) {
	url, _ := contract.NewOneSignalDouble(t, contract.FailureContent, oneSignalKey)
	m := mailer.NewOneSignal(mailer.OneSignalConfig{
		AppID: oneSignalAppID, APIKey: oneSignalKey, BaseURL: url,
	})

	receipt, err := m.Send(t.Context(), ports.Mail{
		AccountID: "acct-1",
		Kind:      string(notification.KindEmailVerification),
		To:        "someone@example.test",
		Data:      map[string]any{"link": "https://dop-t.com/verify"},
	})

	if err == nil {
		t.Fatalf("a 200 that delivered to nobody was reported as sent: %+v", receipt)
	}
	// Content, not unavailability: resending the same message to the same
	// unsubscribed address gives the same answer forever, and a retry loop
	// would be a worker spinning on it until somebody notices the bill.
	if k := errs.KindOf(err); k != errs.KindInvalid {
		t.Fatalf("expected a content refusal, got %s: %v", k, err)
	}
}

// A noreply sender with nowhere to answer is a support ticket that never
// arrives. The reply-to is installation configuration, like the sender, and it
// has to reach the wire on every message.
func TestMailerOneSignalTheReplyToReachesTheProvider(t *testing.T) {
	url, inbox := contract.NewOneSignalDouble(t, "", oneSignalKey)
	m := mailer.NewOneSignal(mailer.OneSignalConfig{
		AppID: oneSignalAppID, APIKey: oneSignalKey, BaseURL: url,
		From: "noreply@mail.dop.test", ReplyTo: "people@dop.test",
	})

	if _, err := m.Send(t.Context(), ports.Mail{
		AccountID: "acct-1", Kind: string(notification.KindEmailVerification),
		To: "someone@example.test", Data: map[string]any{"link": "https://dop.test/v"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := inbox.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if msgs[0].ReplyTo != "people@dop.test" {
		t.Fatalf("the reply-to did not reach the provider: %q", msgs[0].ReplyTo)
	}
}
