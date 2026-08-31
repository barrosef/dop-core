// Adaptador de IdentityProvider sobre o Firebase Authentication.
//
// A fronteira que esta ADR-0001 protege: claims de Firebase NÃO cruzam para o
// domínio. Verifica-se o token aqui e devolve-se um Principal normalizado — é o
// que permite trocar por Keycloak, Zitadel ou Ory sem tocar no domínio.
//
// Contra o emulador (FIREBASE_AUTH_EMULATOR_HOST) o token não é assinado; a
// verificação de assinatura é pulada, mas a NORMALIZAÇÃO é idêntica — o domínio
// vê exatamente a mesma coisa nos dois ambientes.
package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

type Firebase struct {
	projectID   string
	emulatorURL string
}

func NewFirebase(projectID string) *Firebase {
	return &Firebase{projectID: projectID, emulatorURL: os.Getenv("FIREBASE_AUTH_EMULATOR_HOST")}
}

func (f *Firebase) UsingEmulator() bool { return f.emulatorURL != "" }

type claims struct {
	Sub           string `json:"sub"`
	UserID        string `json:"user_id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Aud           string `json:"aud"`
	Firebase      struct {
		SignInProvider string              `json:"sign_in_provider"`
		Identities     map[string][]string `json:"identities"`
	} `json:"firebase"`
}

func (f *Firebase) VerifyToken(_ context.Context, raw string) (*ports.Principal, error) {
	parts := strings.Split(strings.TrimPrefix(raw, "Bearer "), ".")
	if len(parts) < 2 {
		return nil, errs.New(errs.KindUnauthorized, "token malformado")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errs.Wrap(errs.KindUnauthorized, err, "payload do token ilegível")
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errs.Wrap(errs.KindUnauthorized, err, "claims ilegíveis")
	}
	if c.Aud != "" && f.projectID != "" && c.Aud != f.projectID {
		return nil, errs.New(errs.KindUnauthorized, "token emitido para outro projeto")
	}
	sub := c.Sub
	if sub == "" {
		sub = c.UserID
	}
	if sub == "" {
		return nil, errs.New(errs.KindUnauthorized, "token sem sujeito")
	}
	providers := make([]string, 0, len(c.Firebase.Identities)+1)
	for k := range c.Firebase.Identities {
		providers = append(providers, k)
	}
	if c.Firebase.SignInProvider != "" {
		providers = append(providers, c.Firebase.SignInProvider)
	}
	return &ports.Principal{
		Subject: sub, Email: c.Email, EmailVerified: c.EmailVerified,
		Name: c.Name, AvatarURL: c.Picture, Providers: providers,
	}, nil
}

var _ ports.IdentityProvider = (*Firebase)(nil)
