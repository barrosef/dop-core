//go:build integration

package contract_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/adapter/identity"
)

// The CORE's Firebase adapter used to decode the payload from base64 and trust
// it: no signature, no exp, no iss. This test is the proof that it is over.
func TestCoreRefusesAForgedToken(t *testing.T) {
	b64 := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := b64(map[string]any{"alg": "RS256", "kid": "whatever", "typ": "JWT"})
	body := b64(map[string]any{
		"sub": "victim", "aud": "dop-local",
		"iss": "https://securetoken.google.com/dop-local",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	forged := header + "." + body + "." + base64.RawURLEncoding.EncodeToString([]byte("junk"))

	// PRODUCTION mode: no emulator. It is the path that was open.
	t.Setenv("FIREBASE_AUTH_EMULATOR_HOST", "")
	fb := identity.NewFirebase("dop-local")

	if p, err := fb.VerifyToken(context.Background(), forged); err == nil {
		t.Fatalf("FORGED TOKEN ACCEPTED — the bypass is back. Principal: %+v", p)
	}
}
