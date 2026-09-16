package smser

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ZenviaConfig is the second provider — the one that keeps the port honest.
//
// It is deliberately unlike Twilio in every mechanical detail: a JSON body
// instead of a form, a token in a header instead of basic auth, a nested
// `contents` array instead of a flat `Body`. Everything the two do NOT share is
// what the port must not carry.
type ZenviaConfig struct {
	BaseURL string
	Token   string
	// From is the sender registered at the provider ("dop"), the same reason as
	// Twilio's.
	From string
}

type Zenvia struct {
	base   string
	from   string
	token  func() string
	hasKey bool
	client *http.Client
}

func NewZenvia(cfg ZenviaConfig) *Zenvia {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.zenvia.com"
	}
	token := cfg.Token
	return &Zenvia{
		base:   base + "/v2/channels/sms/messages",
		from:   cfg.From,
		token:  func() string { return token },
		hasKey: token != "",
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (z *Zenvia) Send(ctx context.Context, m ports.SMS) (*ports.SMSReceipt, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	if !z.hasKey {
		return dryRun(ctx, "zenvia", m)
	}

	payload := map[string]any{
		"from": z.from,
		// Zenvia takes the destination WITHOUT the "+". The port speaks E.164
		// because that is the unambiguous form; each adapter adjusts to its
		// provider — which is exactly what an adapter is for.
		"to": strings.TrimPrefix(m.To, "+"),
		"contents": []map[string]any{
			{"type": "text", "text": m.Text},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "assembling the request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, z.base, bytes.NewReader(body))
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "assembling the request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-TOKEN", z.token())

	resp, err := z.client.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "zenvia is unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	if resp.StatusCode >= 300 {
		return nil, translateHTTP("zenvia", resp.StatusCode, raw)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &out)
	return &ports.SMSReceipt{State: ports.SMSSent, Provider: "zenvia", Reference: out.ID}, nil
}
