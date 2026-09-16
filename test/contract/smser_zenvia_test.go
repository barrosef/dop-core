package contract_test

// The same suite against the SECOND provider — the one that keeps the port
// honest.
//
// Zenvia is mechanically unlike Twilio in every detail: a JSON body instead of
// a form, a token in a header instead of basic auth, a destination without the
// "+". Everything the two do NOT share is what the port must not carry.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/smser"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/test/contract"
)

const zenviaToken = "zv-api-token-DO-NOT-USE-4Xr9"

func TestSMSerContractZenvia(t *testing.T) {
	newSMSer := func(t *testing.T, f contract.Failure, withServer bool) (ports.SMSer, *contract.Outbox) {
		out := &contract.Outbox{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				To       string `json:"to"`
				Contents []struct {
					Text string `json:"text"`
				} `json:"contents"`
			}
			_ = json.Unmarshal(raw, &body)
			text := ""
			if len(body.Contents) > 0 {
				text = body.Contents[0].Text
			}

			switch f {
			case contract.FailureCredential:
				out.Touched()
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"invalid token ` + zenviaToken + `"}`))
			case contract.FailureUnavailable:
				out.Touched()
				w.WriteHeader(http.StatusBadGateway)
			case contract.FailureContent:
				out.Touched()
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"rejected: ` + text + `"}`))
			default:
				out.Received(contract.SentSMS{
					To: body.To, Text: text, Auth: r.Header.Get("X-API-TOKEN"),
				})
				_, _ = w.Write([]byte(`{"id":"zv-1"}`))
			}
		}))
		t.Cleanup(srv.Close)

		cfg := smser.ZenviaConfig{BaseURL: srv.URL, Token: zenviaToken, From: "dop"}
		if !withServer {
			cfg.Token = ""
		}
		return smser.NewZenvia(cfg), out
	}

	contract.SMSerSuite(t, "zenvia", contract.SMSerHarness{
		Secret: zenviaToken,
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
