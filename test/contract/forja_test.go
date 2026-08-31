//go:build integration

package contract_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/identity"
)

// O adaptador Firebase do NÚCLEO decodificava o payload em base64 e confiava
// nele: sem assinatura, sem exp, sem iss. Este teste é a prova de que acabou.
func TestNucleoRecusaTokenForjado(t *testing.T) {
	b64 := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	cab := b64(map[string]any{"alg": "RS256", "kid": "qualquer", "typ": "JWT"})
	corpo := b64(map[string]any{
		"sub": "vitima", "aud": "dop-local",
		"iss": "https://securetoken.google.com/dop-local",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	forjado := cab + "." + corpo + "." + base64.RawURLEncoding.EncodeToString([]byte("lixo"))

	// Modo PRODUÇÃO: sem emulador. É o caminho que estava aberto.
	t.Setenv("FIREBASE_AUTH_EMULATOR_HOST", "")
	fb := identity.NewFirebase("dop-local")

	if p, err := fb.VerifyToken(context.Background(), forjado); err == nil {
		t.Fatalf("TOKEN FORJADO ACEITO — o bypass voltou. Principal: %+v", p)
	}
}
