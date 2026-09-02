package smser

import (
	"context"
	"net/http"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/secondfactor"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// validate is the port's guarantee 1, shared: a broken number costs nothing
// here, and with SMS "costs nothing" is literal — every send is money.
//
// The E.164 reading is the DOMAIN's (secondfactor.ValidE164), not a copy: two
// readings of the same rule are two rules that diverge on the first fix.
func validate(m ports.SMS) error {
	if m.To == "" || !secondfactor.ValidE164(m.To) {
		return errs.Invalid("invalid destination: it has to be in E.164 format, like +5511999999999")
	}
	if m.Text == "" {
		return errs.Invalid("an SMS with no text")
	}
	return nil
}

// dryRun is guarantee 2: with no credential the adapter does not talk to the
// provider — it prints and returns SMSSentLocal.
//
// It is the local environment's ONLY mode: there is no SMS emulator (P-35).
// And it is the single exception to guarantee 7 — the text carries the code, and
// it only gets printed here, where there is no gateway and nobody to receive.
func dryRun(ctx context.Context, provider string, m ports.SMS) (*ports.SMSReceipt, error) {
	logging.From(ctx).Info("SMS DRY RUN: no credential, nothing was sent",
		"channel", provider, "to", m.To, "text", m.Text)
	return &ports.SMSReceipt{State: ports.SMSSentLocal, Provider: provider}, nil
}

// translateHTTP is guarantee 5, shared by the two adapters.
//
// The distinction is not cosmetic: it decides whether the caller retries the
// code or tells the person to fix the number. And the provider's body does NOT
// go into the message — it echoes back what was sent, and what was sent is the
// code.
func translateHTTP(provider string, status int, _ []byte) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errs.New(errs.KindUnauthorized, "%s refused the credential", provider)
	case status == http.StatusTooManyRequests:
		return errs.New(errs.KindUnavailable, "%s is rate limiting", provider)
	case status >= 500:
		return errs.New(errs.KindUnavailable, "%s answered %d", provider, status)
	default:
		// 4xx that is not authentication: the number or the text was refused.
		return errs.Invalid("%s refused the message (%d)", provider, status)
	}
}
