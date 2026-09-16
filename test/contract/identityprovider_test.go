package contract_test

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/adapter/identity"
	"github.com/barrosef/dop-core/test/contract"
)

// BOTH IdentityProvider adapters go through the same suite, and with no
// external infrastructure: each runs against a fake issuer that signs FOR REAL,
// with an RSA pair generated in the test itself.
//
// That is deliberate, and it is the answer to the Firebase emulator's trap: it
// issues tokens with "alg":"none", so a suite that only exercised it would prove
// nothing about signature verification — the part whose failure hands over any
// user's account. Here the Firebase adapter runs in PRODUCTION mode (no
// emulator), fetching X.509 certificates from a local endpoint in Google's same
// format. The real emulator is exercised separately, under the `integration`
// tag, and there the signature cases show up as SKIPs with a warning.
func TestIdentityProviderContract(t *testing.T) {
	contract.IdentityProviderSuite(t, "oidc", func(t *testing.T) contract.IdentityEnv {
		return novoAmbienteOIDC(t)
	})
	contract.IdentityProviderSuite(t, "firebase_production", func(t *testing.T) contract.IdentityEnv {
		return newLocalFirebaseEnv(t)
	})
}

// testMinRefresh is the key re-fetch brake in the adapters under test. Short
// because the rotation test has to wait it out; long enough for the "it did not
// re-fetch" assertions to fit inside it comfortably.
const testMinRefresh = 300 * time.Millisecond

// ───────────────────────── test keys ─────────────────────────

// Generating a 2048-bit RSA key costs tens of milliseconds, and the suite
// creates one environment per subtest. Three keys generated once cover
// everything: the current one, the rotation's and the intruder's.
var (
	chavesUmaVez sync.Once
	keyA         *rsa.PrivateKey // corrente
	keyB         *rsa.PrivateKey // after the rotation
	intruderKey  *rsa.PrivateKey // never published by the issuer
)

func chavesDeTeste(t *testing.T) {
	t.Helper()
	chavesUmaVez.Do(func() {
		var err error
		if keyA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if keyB, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if intruderKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
}

// ───────────────────────── assinatura de tokens de teste ─────────────────────────

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func objectB64(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("serializando %T: %v", v, err)
	}
	return b64(b)
}

// sign builds the token according to the spec. It is here that the attacks the
// suite sends against the adapters live.
func sign(t *testing.T, s contract.TokenSpec, claims map[string]any) string {
	t.Helper()
	chavesDeTeste(t)

	switch {
	case s.Unsigned:
		// Exactly what the Firebase emulator issues.
		h := objectB64(t, map[string]any{"alg": "none", "typ": "JWT"})
		return h + "." + objectB64(t, claims) + "."

	case s.Symmetric:
		// Algorithm confusion: the attacker swaps RS256 for HS256 and uses the
		// PUBLIC key, which they also have, as the "secret".
		h := objectB64(t, map[string]any{"alg": "HS256", "typ": "JWT", "kid": "key-a"})
		assinado := h + "." + objectB64(t, claims)
		pub, err := x509.MarshalPKIXPublicKey(&keyA.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, pub)
		mac.Write([]byte(assinado))
		return assinado + "." + b64(mac.Sum(nil))
	}

	key, kid := keyA, "key-a"
	if s.WrongKey {
		key, kid = intruderKey, "key-intrusa"
	}
	h := objectB64(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	assinado := h + "." + objectB64(t, claims)
	sum := sha256.Sum256([]byte(assinado))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("assinando token de teste: %v", err)
	}
	return assinado + "." + b64(sig)
}

// claimsBase fills in what is the same in any issuer.
func claimsBase(s contract.TokenSpec, iss, aud string) map[string]any {
	agora := time.Now()
	c := map[string]any{}
	for k, v := range s.Extra {
		c[k] = v
	}
	if s.Issuer != "" {
		iss = s.Issuer
	}
	if s.Audience != "" {
		aud = s.Audience
	}
	exp := s.Expiry
	if exp.IsZero() {
		exp = agora.Add(time.Hour)
	}
	iat := s.IssuedAt
	if iat.IsZero() {
		iat = agora
	}
	c["iss"] = iss
	c["aud"] = aud
	c["exp"] = exp.Unix()
	c["iat"] = iat.Unix()
	if !s.NotBefore.IsZero() {
		c["nbf"] = s.NotBefore.Unix()
	}
	if s.Subject != "" {
		c["sub"] = s.Subject
	}
	if s.Email != "" {
		c["email"] = s.Email
	}
	if s.EmailVerified != nil {
		c["email_verified"] = *s.EmailVerified
	}
	if s.Name != "" {
		c["name"] = s.Name
	}
	if s.Picture != "" {
		c["picture"] = s.Picture
	}
	return c
}

// firebaseProviderClaims builds the `firebase` claim the way Firebase really
// builds it, and getting that shape right is the whole reason this helper is
// not one line inline.
//
// `sign_in_provider` names the provider used THIS time; `identities` is keyed by
// IDENTIFIER TYPE — "email" for an e-mail/password account, the provider's own
// domain for a social one. So a password login crosses the port as
// ["email","password"] and never as ["password"] alone.
//
// Minting only `sign_in_provider` handed the adapter a list no Firebase token
// ever produces, and every assertion downstream then agreed with that fiction:
// a domain rule that reads the values passed its tests and did nothing in
// production. An environment that lies about the provider's output is worse than
// no environment, because it reports success.
func firebaseProviderClaims(providers []string) map[string]any {
	identities := map[string]any{}
	for _, p := range providers {
		key := p
		if p == "password" {
			key = "email"
		}
		identities[key] = []string{"identifier-of-" + key}
	}
	return map[string]any{
		"sign_in_provider": providers[0],
		"identities":       identities,
	}
}

// ───────────────────────── a make-believe OIDC issuer ──────────────────────

// oidcIssuer serves /.well-known/openid-configuration and a JWKS, counting the
// key fetches. It is the minimum needed to exercise discovery, caching and
// rotation without bringing up a Keycloak.
type oidcIssuer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	key     *rsa.PrivateKey
	kid     string
	buscas  int
	fechado bool
}

const audienciaOIDC = "dop-core"

func novoAmbienteOIDC(t *testing.T) contract.IdentityEnv {
	t.Helper()
	chavesDeTeste(t)
	e := &oidcIssuer{key: keyA, kid: "key-a"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   e.srv.URL,
			"jwks_uri": e.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.buscas++
		pub := e.key.PublicKey
		kid := e.kid
		e.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": kid,
				"n":   b64(pub.N.Bytes()),
				"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	idp := identity.NewOIDC(identity.OIDCConfig{
		Issuer:         e.srv.URL,
		Audience:       audienciaOIDC,
		KeysMinRefresh: testMinRefresh,
	})

	return contract.IdentityEnv{
		Provider: idp,
		Mint: func(t *testing.T, s contract.TokenSpec) (string, bool) {
			c := claimsBase(s, e.srv.URL, audienciaOIDC)
			// OIDC splits the question in two, and routing each request to the
			// claim that really carries it is what keeps this environment from
			// grading the adapter against a fiction: `amr` says HOW the person
			// authenticated — Keycloak sends "pwd", never "password", which is
			// exactly the normalization the adapter has to perform — while `idp`
			// names the brokered external provider.
			var amr []string
			for _, prov := range s.Providers {
				if prov == "password" {
					amr = append(amr, "pwd")
					continue
				}
				c["idp"] = prov
			}
			if len(amr) > 0 {
				c["amr"] = amr
			}
			// The rotation swaps the issuer's key; the new token has to come out
			// signed by it.
			e.mu.Lock()
			rotated := e.key != keyA
			e.mu.Unlock()
			if rotated && !s.WrongKey && !s.Unsigned && !s.Symmetric {
				return signWith(t, keyB, "key-b", c), true
			}
			return sign(t, s, c), true
		},
		Fetches: func() int {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.buscas
		},
		Rotate: func(t *testing.T) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.key, e.kid = keyB, "key-b"
		},
		MinRefresh: testMinRefresh,
		IssuerDown: func(t *testing.T) { e.srv.Close() },
	}
}

func signWith(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h := objectB64(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	assinado := h + "." + objectB64(t, claims)
	sum := sha256.Sum256([]byte(assinado))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("assinando token de teste: %v", err)
	}
	return assinado + "." + b64(sig)
}

// ───────────────────────── Firebase in production mode ──────────────────────────

const testProject = "dop-teste"

// newLocalFirebaseEnv runs the Firebase adapter with signature verification ON,
// serving the certificates from a local endpoint in Google's same format
// (kid → an X.509 certificate in PEM).
func newLocalFirebaseEnv(t *testing.T) contract.IdentityEnv {
	t.Helper()
	chavesDeTeste(t)

	// The adapter reads FIREBASE_AUTH_EMULATOR_HOST from the environment. If the
	// machine running the tests has the variable exported, the adapter would
	// enter emulator mode and SKIP the signature verification — and this test
	// would pass green verifying nothing. Zeroing the variable here is what
	// stops the developer's environment from switching off the most important
	// guarantee.
	t.Setenv("FIREBASE_AUTH_EMULATOR_HOST", "")

	e := &oidcIssuer{key: keyA, kid: "key-a"}
	mux := http.NewServeMux()
	mux.HandleFunc("/certs", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.buscas++
		key, kid := e.key, e.kid
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{kid: pemCertificate(t, key)})
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	idp := identity.NewFirebaseFrom(identity.FirebaseConfig{
		ProjectID:      testProject,
		CertsURL:       e.srv.URL + "/certs",
		KeysMinRefresh: testMinRefresh,
	})
	if idp.UsingEmulator() {
		t.Fatal("the adapter entered emulator mode: the signature verification would be off")
	}

	return contract.IdentityEnv{
		Provider: idp,
		Mint: func(t *testing.T, s contract.TokenSpec) (string, bool) {
			c := claimsBase(s, "https://securetoken.google.com/"+testProject, testProject)
			if s.Subject != "" {
				c["user_id"] = s.Subject
			}
			// Firebase's vocabulary for "how they signed in".
			if len(s.Providers) > 0 {
				c["firebase"] = firebaseProviderClaims(s.Providers)
			}
			e.mu.Lock()
			rotated := e.key != keyA
			e.mu.Unlock()
			if rotated && !s.WrongKey && !s.Unsigned && !s.Symmetric {
				return signWith(t, keyB, "key-b", c), true
			}
			return sign(t, s, c), true
		},
		Fetches: func() int {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.buscas
		},
		Rotate: func(t *testing.T) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.key, e.kid = keyB, "key-b"
		},
		MinRefresh: testMinRefresh,
		IssuerDown: func(t *testing.T) { e.srv.Close() },
	}
}

// pemCertificate wraps the public key in a self-signed certificate, because that
// is the format in which Google publishes the Secure Token Service's keys — and
// it is the format the adapter has to know how to read.
func pemCertificate(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "securetoken-de-teste"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("gerando certificado de teste: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
