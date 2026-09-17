// Package totp implements RFC 6238 (a time-based one-time password) over
// RFC 4226 (HOTP).
//
// It is written here, in ~120 lines of the standard library, instead of being
// pulled in as a dependency: the algorithm is an HMAC, a truncation and a
// modulo, it has not changed since 2011, and a dependency in the authentication
// path is a supply chain in the authentication path.
//
// It is also the only one of the three verifiers with NO external service: the
// same code runs identically in the local environment and in production, which
// is why ADR-0020 puts it first in the implementation order.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Step is the window's size. Thirty seconds is what every authenticator app
	// assumes, and it is not configurable on purpose: an app that reads our QR
	// code does not read a period we invented.
	Step = 30 * time.Second
	// Digits is the code's length — six, for the same reason.
	Digits = 6
	// Skew is how many windows BACKWARD and forward are accepted. One window
	// (±30 s) covers the phone's clock drift and the person typing the code as
	// it is about to expire. Two would double the guessing surface for a
	// comfort nobody asked for.
	Skew = 1
	// secretBytes is the seed's size. RFC 4226 requires at least 128 bits and
	// recommends 160 — which is also SHA-1's block, and what Google
	// Authenticator expects.
	secretBytes = 20
)

// b32 is base32 WITHOUT padding: it is what the `otpauth://` URI carries, and a
// "=" at the end breaks readers that do not expect it.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret draws a seed. It comes back already in base32 because that is the
// form that travels — to the vault, to the URI and to the QR code.
func NewSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("drawing the TOTP seed: %w", err)
	}
	return b32.EncodeToString(buf), nil
}

// Code computes the code for an instant. Exported because the tests — and only
// the tests — need to produce a valid code without an app.
func Code(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("the TOTP seed is not valid base32: %w", err)
	}

	counter := uint64(t.UTC().Unix()) / uint64(Step.Seconds())
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation (RFC 4226 §5.3): the last nibble picks where to read
	// four bytes from, and the high bit is dropped so the number is positive on
	// every platform.
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", Digits, value%mod), nil
}

// Validate answers whether the code proves possession of the seed at that
// instant, accepting ±Skew windows.
//
// The comparison is CONSTANT TIME. It is six digits and the difference is
// microseconds, but a comparison that returns early on the first different
// digit is a timing oracle that turns 10^6 guesses into 6×10 — and the cost of
// not making that mistake is one function call.
func Validate(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != Digits {
		return false
	}
	for w := -Skew; w <= Skew; w++ {
		want, err := Code(secret, now.Add(time.Duration(w)*Step))
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// URI builds the `otpauth://` that becomes the QR code.
//
// It is shown ONCE, at enrolment, and never again: the platform stores the seed
// in the vault to VERIFY, not to show. An endpoint that gave the URI back would
// turn every session into an enrolment of a new device.
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", Digits))
	q.Set("period", fmt.Sprintf("%d", int(Step.Seconds())))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
