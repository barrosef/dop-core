// Package callauth is how the core stops believing a header and starts
// verifying a signature (ADR-0022).
//
// ── The two shapes, and why there are two ───────────────────────────────────
//
// A call to the core is one of two things, and they can prove different things:
//
//   - a call WITH A PERSON behind it carries the user's own token. Its signature
//     is the identity provider's — an authority neither the edge nor the core
//     controls — so it is the strongest proof available, and it costs nothing to
//     obtain: the edge already holds the token it just verified;
//   - a call WITH NO PERSON — the edge resolving who a subject is before an actor
//     exists, a component acting on its own — has no token to forward. What it
//     can offer is an ASSERTION signed by the platform: "I am this component,
//     and I am acting for this actor, in this account, until this instant".
//
// This package is the second one. The first needs no code here: it is the
// `IdentityProvider` port the core already has.
//
// ── Why an assertion and not just a shared secret ───────────────────────────
//
// A secret in a header proves only that the caller knows the secret — it says
// nothing about WHAT is being claimed, so whoever holds it declares any actor of
// any account. The signature here covers the CLAIM: the actor, the account and
// the expiry are inside what was signed. A stolen assertion is worth exactly one
// actor, in one account, until it expires.
package callauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Assertion is what a platform component signs about the call it is making.
type Assertion struct {
	// Caller names the component — "bff", "launcher". The core keeps a key per
	// caller, so a compromised component forges only its own calls.
	Caller string
	// ActorID may be EMPTY, and that is a legitimate state: the edge resolving
	// a subject into a user does not yet know the actor. What it never is, is
	// invented.
	ActorID   string
	ActorKind string
	AccountID string
	SessionID string
	ExpiresAt time.Time
}

// Sign produces the value of the `x-dop-assertion` header.
//
// The fields travel separated by `|`, so a field containing one is refused
// rather than escaped: an id with a pipe in it is a bug at the source, and
// escaping would hide it until the day the two ends disagreed about the rule.
func Sign(key []byte, a Assertion) (string, error) {
	if len(key) == 0 {
		return "", errs.New(errs.KindInternal, "signing a call with no key")
	}
	if strings.TrimSpace(a.Caller) == "" {
		return "", errs.Invalid("an assertion with no caller")
	}
	if a.ExpiresAt.IsZero() {
		return "", errs.Invalid("an assertion with no expiry")
	}
	fields := []string{a.Caller, a.ActorID, a.ActorKind, a.AccountID, a.SessionID,
		strconv(a.ExpiresAt.Unix())}
	for _, f := range fields {
		if strings.ContainsAny(f, "|.") {
			return "", errs.Invalid("assertion field with a separator: %q", f)
		}
	}
	payload := strings.Join(fields, "|")
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + mac(key, payload), nil
}

// Verify returns the assertion when the signature holds and it has not expired.
//
// It reports the SAME error for a bad signature and an expired one on purpose:
// telling them apart tells whoever is probing which half to work on.
func Verify(key []byte, raw string, now time.Time) (*Assertion, error) {
	refused := errs.New(errs.KindUnauthorized, "assertion not accepted")
	if len(key) == 0 {
		return nil, errs.New(errs.KindInternal, "verifying a call with no key")
	}
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 2 {
		return nil, refused
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, refused
	}
	if !hmac.Equal([]byte(mac(key, string(payload))), []byte(parts[1])) {
		return nil, refused
	}
	fields := strings.Split(string(payload), "|")
	if len(fields) != 6 {
		return nil, refused
	}
	exp, err := atoi(fields[5])
	if err != nil || now.Unix() >= exp {
		return nil, refused
	}
	return &Assertion{
		Caller: fields[0], ActorID: fields[1], ActorKind: fields[2],
		AccountID: fields[3], SessionID: fields[4],
		ExpiresAt: time.Unix(exp, 0),
	}, nil
}

func mac(key []byte, payload string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}

func strconv(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func atoi(s string) (int64, error) {
	if s == "" {
		return 0, errs.Invalid("empty number")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errs.Invalid("not a number: %q", s)
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}
