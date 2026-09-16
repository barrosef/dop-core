package contract_test

// The SAME contract suite, against the REAL Twilio adapter and an HTTP double.
//
//	go test ./test/contract/ -run SMSer -v

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/smser"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/test/contract"
)

// twilioToken is the secret the double echoes back in the refusal — it is what
// turns guarantee 3 into a test instead of a promise.
const twilioToken = "tw1l10-auth-token-DO-NOT-USE-7Kq2"

func TestSMSerContractTwilio(t *testing.T) {
	newSMSer := func(t *testing.T, f contract.Failure, withServer bool) (ports.SMSer, *contract.Outbox) {
		out := &contract.Outbox{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			switch f {
			case contract.FailureCredential:
				out.Touched()
				w.WriteHeader(http.StatusUnauthorized)
				// The provider echoing the secret back is the realistic case,
				// and it is what the suite hunts for in the error.
				_, _ = w.Write([]byte(`{"message":"authenticate failed for ` + twilioToken + `"}`))
			case contract.FailureUnavailable:
				out.Touched()
				w.WriteHeader(http.StatusServiceUnavailable)
			case contract.FailureContent:
				out.Touched()
				w.WriteHeader(http.StatusBadRequest)
				// It echoes the body back: guarantee 7 requires the code not to
				// come out of here into the error.
				_, _ = w.Write([]byte(`{"message":"invalid body: ` + r.Form.Get("Body") + `"}`))
			default:
				out.Received(contract.SentSMS{
					To: r.Form.Get("To"), Text: r.Form.Get("Body"),
					Auth: r.Header.Get("Authorization"),
				})
				_, _ = w.Write([]byte(`{"sid":"SM123"}`))
			}
		}))
		t.Cleanup(srv.Close)

		cfg := smser.TwilioConfig{BaseURL: srv.URL, AccountSID: "AC123", AuthToken: twilioToken, From: "+15005550006"}
		if !withServer {
			// The dry run: with no credential it prints instead of sending.
			cfg.AccountSID, cfg.AuthToken = "", ""
		}
		return smser.NewTwilio(cfg), out
	}

	contract.SMSerSuite(t, "twilio", contract.SMSerHarness{
		Secret: twilioToken,
		New: func(t *testing.T) (ports.SMSer, *contract.Outbox) {
			return newSMSer(t, "", true)
		},
		NewFailing: func(t *testing.T, f contract.Failure) (ports.SMSer, *contract.Outbox) {
			return newSMSer(t, f, true)
		},
		NewDryRun: func(t *testing.T) (ports.SMSer, *contract.Outbox) {
			return newSMSer(t, "", false)
		},
	})
}
