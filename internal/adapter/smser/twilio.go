// Package smser holds the ports.SMSer adapters.
//
// Two of them, from day one, by ADR-0001's discipline: the second exists to
// prove the port does not leak the first one's vocabulary. Here the risk was
// concrete — Twilio's API has `MessagingServiceSid`, status callbacks and
// message parts, and a port designed against Twilio alone would have carried
// all three into the domain.
//
// Neither adapter renders anything: SMS is one line, the same in every
// provider, and the text arrives ready (see ports.SMS).
package smser

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// TwilioConfig is what the INSTALLATION provides. The credential is not a field
// of the adapter: it is captured in a closure (guarantee 3) so a `%+v` of the
// adapter cannot print it.
type TwilioConfig struct {
	// BaseURL exists for the test double. Empty uses Twilio's.
	BaseURL    string
	AccountSID string
	AuthToken  string
	// From is the sender — a long code, a short code or a Messaging Service.
	// It is installation configuration, per provider and per country, and that
	// is why it is NOT in the port.
	From string
}

type Twilio struct {
	base   string
	from   string
	auth   func() (string, string) // the closure that holds the credential
	hasKey bool
	client *http.Client
}

func NewTwilio(cfg TwilioConfig) *Twilio {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.twilio.com"
	}
	sid, token := cfg.AccountSID, cfg.AuthToken
	return &Twilio{
		base:   base + "/2010-04-01/Accounts/" + url.PathEscape(sid) + "/Messages.json",
		from:   cfg.From,
		auth:   func() (string, string) { return sid, token },
		hasKey: sid != "" && token != "",
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *Twilio) Send(ctx context.Context, m ports.SMS) (*ports.SMSReceipt, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	if !t.hasKey {
		return dryRun(ctx, "twilio", m)
	}

	form := url.Values{}
	form.Set("To", m.To)
	form.Set("From", t.from)
	form.Set("Body", m.Text)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "assembling the request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(t.auth())

	resp, err := t.client.Do(req)
	if err != nil {
		// Network, DNS, timeout: it is the provider that is unreachable, not the
		// message that is wrong. Confusing the two sends the team hunting the
		// defect in the wrong place.
		return nil, errs.Wrap(errs.KindUnavailable, err, "twilio is unreachable")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	if resp.StatusCode >= 300 {
		return nil, translateHTTP("twilio", resp.StatusCode, body)
	}
	var out struct {
		SID string `json:"sid"`
	}
	_ = json.Unmarshal(body, &out)
	return &ports.SMSReceipt{State: ports.SMSSent, Provider: "twilio", Reference: out.SID}, nil
}
