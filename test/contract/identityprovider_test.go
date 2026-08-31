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

	"github.com/Digital-Business-One/dop-core/internal/adapter/identity"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// Os DOIS adaptadores de IdentityProvider passam pela mesma suíte, e sem
// infraestrutura externa: cada um roda contra um emissor de mentira que assina
// DE VERDADE, com um par RSA gerado no próprio teste.
//
// Isso é deliberado, e é a resposta à armadilha do emulador do Firebase: ele
// emite tokens com "alg":"none", então uma suíte que só o exercitasse não
// provaria nada sobre verificação de assinatura — a parte cuja falha entrega a
// conta de qualquer usuário. Aqui o adaptador do Firebase roda no modo de
// PRODUÇÃO (sem emulador), buscando certificado X.509 de um endpoint local no
// mesmo formato do Google. O emulador de verdade é exercitado à parte, sob a tag
// `integration`, e lá os casos de assinatura aparecem como SKIP com aviso.
func TestIdentityProviderContract(t *testing.T) {
	contract.IdentityProviderSuite(t, "oidc", func(t *testing.T) contract.IdentityEnv {
		return novoAmbienteOIDC(t)
	})
	contract.IdentityProviderSuite(t, "firebase_producao", func(t *testing.T) contract.IdentityEnv {
		return novoAmbienteFirebaseLocal(t)
	})
}

// minRefreshDeTeste é o freio de rebusca de chave nos adaptadores sob teste.
// Curto porque o teste de rotação precisa esperá-lo; longo o bastante para que
// as asserções de "não rebuscou" caibam dentro dele com folga.
const minRefreshDeTeste = 300 * time.Millisecond

// ───────────────────────── chaves de teste ─────────────────────────

// Gerar RSA de 2048 bits custa dezenas de milissegundos, e a suíte cria um
// ambiente por subteste. Três chaves geradas uma vez cobrem tudo: a corrente, a
// da rotação e a intrusa.
var (
	chavesUmaVez sync.Once
	chaveA       *rsa.PrivateKey // corrente
	chaveB       *rsa.PrivateKey // depois da rotação
	chaveIntrusa *rsa.PrivateKey // nunca publicada pelo emissor
)

func chavesDeTeste(t *testing.T) {
	t.Helper()
	chavesUmaVez.Do(func() {
		var err error
		if chaveA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if chaveB, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if chaveIntrusa, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
}

// ───────────────────────── assinatura de tokens de teste ─────────────────────────

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func objetoB64(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("serializando %T: %v", v, err)
	}
	return b64(b)
}

// assina monta o token conforme a spec. É aqui que moram os ataques que a suíte
// manda contra os adaptadores.
func assina(t *testing.T, s contract.TokenSpec, claims map[string]any) string {
	t.Helper()
	chavesDeTeste(t)

	switch {
	case s.Unsigned:
		// Exatamente o que o emulador do Firebase emite.
		h := objetoB64(t, map[string]any{"alg": "none", "typ": "JWT"})
		return h + "." + objetoB64(t, claims) + "."

	case s.Symmetric:
		// Confusão de algoritmo: o atacante troca RS256 por HS256 e usa como
		// "segredo" a chave PÚBLICA, que ele também tem.
		h := objetoB64(t, map[string]any{"alg": "HS256", "typ": "JWT", "kid": "chave-a"})
		assinado := h + "." + objetoB64(t, claims)
		pub, err := x509.MarshalPKIXPublicKey(&chaveA.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, pub)
		mac.Write([]byte(assinado))
		return assinado + "." + b64(mac.Sum(nil))
	}

	chave, kid := chaveA, "chave-a"
	if s.WrongKey {
		chave, kid = chaveIntrusa, "chave-intrusa"
	}
	h := objetoB64(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	assinado := h + "." + objetoB64(t, claims)
	soma := sha256.Sum256([]byte(assinado))
	sig, err := rsa.SignPKCS1v15(rand.Reader, chave, crypto.SHA256, soma[:])
	if err != nil {
		t.Fatalf("assinando token de teste: %v", err)
	}
	return assinado + "." + b64(sig)
}

// claimsBase preenche o que é igual em qualquer emissor.
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

// ───────────────────────── emissor OIDC de mentira ─────────────────────────

// emissorOIDC serve /.well-known/openid-configuration e um JWKS, contando as
// buscas de chave. É o mínimo para exercitar descoberta, cache e rotação sem
// subir um Keycloak.
type emissorOIDC struct {
	srv     *httptest.Server
	mu      sync.Mutex
	chave   *rsa.PrivateKey
	kid     string
	buscas  int
	fechado bool
}

const audienciaOIDC = "dop-core"

func novoAmbienteOIDC(t *testing.T) contract.IdentityEnv {
	t.Helper()
	chavesDeTeste(t)
	e := &emissorOIDC{chave: chaveA, kid: "chave-a"}

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
		pub := e.chave.PublicKey
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
		KeysMinRefresh: minRefreshDeTeste,
	})

	return contract.IdentityEnv{
		Provider: idp,
		Mint: func(t *testing.T, s contract.TokenSpec) (string, bool) {
			c := claimsBase(s, e.srv.URL, audienciaOIDC)
			// O vocabulário do OIDC para "como entrou" é `amr`.
			if len(s.Providers) > 0 {
				c["amr"] = s.Providers
			}
			// A rotação troca a chave do emissor; o token novo tem de sair
			// assinado por ela.
			e.mu.Lock()
			rotacionada := e.chave != chaveA
			e.mu.Unlock()
			if rotacionada && !s.WrongKey && !s.Unsigned && !s.Symmetric {
				return assinaCom(t, chaveB, "chave-b", c), true
			}
			return assina(t, s, c), true
		},
		Fetches: func() int {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.buscas
		},
		Rotate: func(t *testing.T) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.chave, e.kid = chaveB, "chave-b"
		},
		MinRefresh: minRefreshDeTeste,
		IssuerDown: func(t *testing.T) { e.srv.Close() },
	}
}

func assinaCom(t *testing.T, chave *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h := objetoB64(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	assinado := h + "." + objetoB64(t, claims)
	soma := sha256.Sum256([]byte(assinado))
	sig, err := rsa.SignPKCS1v15(rand.Reader, chave, crypto.SHA256, soma[:])
	if err != nil {
		t.Fatalf("assinando token de teste: %v", err)
	}
	return assinado + "." + b64(sig)
}

// ───────────────────────── Firebase em modo de produção ─────────────────────────

const projetoDeTeste = "dop-teste"

// novoAmbienteFirebaseLocal roda o adaptador do Firebase com verificação de
// assinatura LIGADA, servindo os certificados de um endpoint local no mesmo
// formato do Google (kid → certificado X.509 em PEM).
func novoAmbienteFirebaseLocal(t *testing.T) contract.IdentityEnv {
	t.Helper()
	chavesDeTeste(t)

	// O adaptador lê FIREBASE_AUTH_EMULATOR_HOST do ambiente. Se a máquina de
	// quem roda os testes tiver a variável exportada, o adaptador entraria em
	// modo emulador e PULARIA a verificação de assinatura — e este teste
	// passaria verde sem verificar nada. Zerar a variável aqui é o que impede
	// que o ambiente do desenvolvedor desligue a garantia mais importante.
	t.Setenv("FIREBASE_AUTH_EMULATOR_HOST", "")

	e := &emissorOIDC{chave: chaveA, kid: "chave-a"}
	mux := http.NewServeMux()
	mux.HandleFunc("/certs", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.buscas++
		chave, kid := e.chave, e.kid
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{kid: certificadoPEM(t, chave)})
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	idp := identity.NewFirebaseFrom(identity.FirebaseConfig{
		ProjectID:      projetoDeTeste,
		CertsURL:       e.srv.URL + "/certs",
		KeysMinRefresh: minRefreshDeTeste,
	})
	if idp.UsingEmulator() {
		t.Fatal("o adaptador entrou em modo emulador: a verificação de assinatura estaria desligada")
	}

	return contract.IdentityEnv{
		Provider: idp,
		Mint: func(t *testing.T, s contract.TokenSpec) (string, bool) {
			c := claimsBase(s, "https://securetoken.google.com/"+projetoDeTeste, projetoDeTeste)
			if s.Subject != "" {
				c["user_id"] = s.Subject
			}
			// O vocabulário do Firebase para "como entrou".
			if len(s.Providers) > 0 {
				c["firebase"] = map[string]any{"sign_in_provider": s.Providers[0]}
			}
			e.mu.Lock()
			rotacionada := e.chave != chaveA
			e.mu.Unlock()
			if rotacionada && !s.WrongKey && !s.Unsigned && !s.Symmetric {
				return assinaCom(t, chaveB, "chave-b", c), true
			}
			return assina(t, s, c), true
		},
		Fetches: func() int {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.buscas
		},
		Rotate: func(t *testing.T) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.chave, e.kid = chaveB, "chave-b"
		},
		MinRefresh: minRefreshDeTeste,
		IssuerDown: func(t *testing.T) { e.srv.Close() },
	}
}

// certificadoPEM embrulha a chave pública num certificado autoassinado, porque é
// nesse formato que o Google publica as chaves do Secure Token Service — e é o
// formato que o adaptador precisa saber ler.
func certificadoPEM(t *testing.T, chave *rsa.PrivateKey) string {
	t.Helper()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "securetoken-de-teste"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &chave.PublicKey, chave)
	if err != nil {
		t.Fatalf("gerando certificado de teste: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
