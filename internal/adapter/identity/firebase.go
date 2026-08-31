// Adaptador de IdentityProvider sobre o Firebase Authentication.
//
// A fronteira que a ADR-0001 protege: claims de Firebase NÃO cruzam para o
// domínio. Verifica-se o token aqui e devolve-se um Principal normalizado — é o
// que permite trocar por Keycloak, Zitadel ou Ory sem tocar no domínio.
//
// A verificação usa as MESMAS primitivas do adaptador OIDC (parseJWT, keySet,
// registeredClaims.validate, ver oidc.go). O que muda é só de onde vem a chave
// pública e o formato dela: o Google publica CERTIFICADOS X.509 por `kid` em
// vez de JWKS, para os tokens do Secure Token Service.
//
// Sobre o EMULADOR (FIREBASE_AUTH_EMULATOR_HOST): ele emite tokens com
// "alg":"none" — sem assinatura nenhuma. Isso é comportamento do emulador, não
// do Firebase real. Neste modo a verificação de assinatura é PULADA, e só ela:
// emissor, audiência, expiração, nbf e sujeito continuam sendo checados, e a
// normalização é idêntica, de modo que o domínio vê exatamente a mesma coisa nos
// dois ambientes. O modo é ligado por variável de ambiente e nunca por conteúdo
// do token — token que pede para não ser verificado é exatamente o que um
// atacante mandaria.
package identity

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// googleSecureTokenCerts é onde o Google publica as chaves que assinam ID token
// do Firebase. É endpoint de CERTIFICADO X.509 (kid → PEM), não JWKS: o
// documento JWKS do Google (`/oauth2/v3/certs`) serve os tokens do Google
// Sign-In, que são outros. Apontar para o errado dá "chave desconhecida" em
// todo login — falha silenciosa e cara de achar.
const googleSecureTokenCerts = "https://www.googleapis.com/robots/v1/metadata/x509/securetoken@system.gserviceaccount.com"

// firebaseIssuerPrefix + projectID é o `iss` que todo ID token do Firebase
// carrega, emulador incluído.
const firebaseIssuerPrefix = "https://securetoken.google.com/"

type FirebaseConfig struct {
	ProjectID string
	// CertsURL existe para a suíte de contrato poder servir certificados locais
	// e exercitar a verificação de assinatura DE VERDADE, sem internet e sem
	// depender do emulador (que não assina nada). Vazio = o endpoint do Google.
	CertsURL string
	// EmulatorHost vazio faz o adaptador ler FIREBASE_AUTH_EMULATOR_HOST, que é
	// o que o composition root já configura.
	EmulatorHost   string
	HTTPClient     *http.Client
	ClockSkew      time.Duration
	KeysMinRefresh time.Duration
	Now            func() time.Time
}

type Firebase struct {
	projectID   string
	emulatorURL string
	client      *http.Client
	certsURL    string
	skew        time.Duration
	now         func() time.Time
	keys        *keySet
}

// NewFirebase é a forma que o composition root usa (internal/app/wire.go).
func NewFirebase(projectID string) *Firebase {
	return NewFirebaseFrom(FirebaseConfig{ProjectID: projectID})
}

func NewFirebaseFrom(cfg FirebaseConfig) *Firebase {
	f := &Firebase{
		projectID:   cfg.ProjectID,
		emulatorURL: cfg.EmulatorHost,
		client:      cfg.HTTPClient,
		certsURL:    cfg.CertsURL,
		skew:        cfg.ClockSkew,
		now:         cfg.Now,
	}
	if f.emulatorURL == "" {
		f.emulatorURL = os.Getenv("FIREBASE_AUTH_EMULATOR_HOST")
	}
	if f.client == nil {
		f.client = defaultHTTPClient()
	}
	if f.certsURL == "" {
		f.certsURL = googleSecureTokenCerts
	}
	if f.skew == 0 {
		f.skew = defaultClockSkew
	}
	if f.now == nil {
		f.now = time.Now
	}
	minRefresh := cfg.KeysMinRefresh
	if minRefresh == 0 {
		minRefresh = defaultKeysMinRefresh
	}
	f.keys = &keySet{fetch: f.fetchCerts, minRefresh: minRefresh, now: f.now}
	return f
}

func (f *Firebase) UsingEmulator() bool { return f.emulatorURL != "" }

// fetchCerts lê o mapa kid → certificado PEM e extrai a chave pública RSA.
func (f *Firebase) fetchCerts(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	var raw map[string]string
	if err := getJSON(ctx, f.client, f.certsURL, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]*rsa.PublicKey, len(raw))
	for kid, crt := range raw {
		blk, _ := pem.Decode([]byte(crt))
		if blk == nil {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			continue
		}
		// A validade do certificado NÃO é checada aqui de propósito: o que
		// interessa é a chave pública que o Google publica AGORA, e o Google
		// retira do endpoint a chave que aposentou. Rejeitar por data só
		// adicionaria uma segunda fonte de "ninguém entra mais".
		if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok && pub.N.BitLen() >= 2048 {
			out[kid] = pub
		}
	}
	if len(out) == 0 {
		return nil, errs.New(errs.KindUnavailable, "nenhum certificado de assinatura utilizável no endpoint do Google")
	}
	return out, nil
}

type firebaseClaims struct {
	registeredClaims
	// UserID é o nome antigo do sujeito nos tokens do Firebase; o emulador manda
	// os dois. Continua aqui como reserva, e nunca substitui a checagem de
	// sujeito da garantia 3.
	UserID        string   `json:"user_id"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	Picture       string   `json:"picture"`
	Firebase      struct {
		SignInProvider string              `json:"sign_in_provider"`
		Identities     map[string][]string `json:"identities"`
	} `json:"firebase"`
}

func (f *Firebase) VerifyToken(ctx context.Context, raw string) (*ports.Principal, error) {
	// Sem projeto configurado não há emissor nem audiência para comparar, e
	// aceitar o token assim mesmo seria aceitar token de QUALQUER projeto do
	// Firebase — que é conta de terceiro entrando como usuário nosso. Falha
	// fechada, e como indisponibilidade: o defeito é da instalação, não de quem
	// está chamando (garantia 7).
	if f.projectID == "" {
		return nil, errs.New(errs.KindUnavailable, "adaptador de identidade sem projeto configurado")
	}
	tok, err := parseJWT(raw)
	if err != nil {
		return nil, err
	}
	if !f.UsingEmulator() {
		key, err := f.keys.publicKey(ctx, tok.header.Kid)
		if err != nil {
			return nil, err
		}
		if err := verifyRSASignature(key, tok); err != nil {
			return nil, err
		}
	}
	var c firebaseClaims
	if err := json.Unmarshal(tok.payload, &c); err != nil {
		return nil, errs.New(errs.KindUnauthorized, "claims do token ilegíveis")
	}
	if c.Sub == "" {
		c.Sub = c.UserID
	}
	if err := c.validate(f.now(), f.skew, firebaseIssuerPrefix+f.projectID, f.projectID); err != nil {
		return nil, err
	}
	// firebase.identities traz os provedores VINCULADOS e sign_in_provider o que
	// foi usado agora. Os dois viram a mesma lista normalizada: para o domínio a
	// pergunta é "por onde esta pessoa entra", e a diferença entre as duas
	// coisas não existe do outro lado da porta (garantia 6).
	// A ordem é fixada com sort porque a iteração de mapa em Go é aleatória, e a
	// garantia 9 promete o MESMO resultado para o mesmo token.
	names := make([]string, 0, len(c.Firebase.Identities)+1)
	for k := range c.Firebase.Identities {
		names = append(names, k)
	}
	sort.Strings(names)
	names = append(names, c.Firebase.SignInProvider)
	return &ports.Principal{
		Subject:       c.Sub,
		Email:         c.Email,
		EmailVerified: bool(c.EmailVerified),
		Name:          c.Name,
		AvatarURL:     c.Picture,
		Providers:     normalizeProviders(names),
	}, nil
}

var _ ports.IdentityProvider = (*Firebase)(nil)
