//go:build integration

// A MESMA suíte de contrato, agora contra o emulador do Firebase Auth de verdade.
//
//	go test ./test/contract/ -tags=integration -run Identity -v
//
// O emulador local do k3d responde em http://auth.localtest.me:8080 pelo
// Ingress; FIREBASE_AUTH_EMULATOR_HOST aponta para outro.
//
// ATENÇÃO — a armadilha que este arquivo existe para não cair:
//
// o emulador emite tokens com "alg":"none", SEM assinatura. Isso é
// comportamento do emulador, não do Firebase real, e significa que uma suíte que
// só rodasse aqui passaria verde sem exercitar uma única linha de verificação de
// assinatura — justamente a parte cuja falha entrega a conta de qualquer usuário
// a quem souber montar um JSON. Por isso:
//
//   - o ambiente declara Unsigned, e os casos de assinatura aparecem como SKIP
//     com aviso, em vez de passarem em silêncio;
//   - a cobertura de assinatura do MESMO adaptador vem de
//     identityprovider_test.go, que roda sempre, com o adaptador em modo de
//     produção e certificados X.509 servidos localmente.
//
// O que ESTE arquivo prova, e o outro não pode provar: que o adaptador aceita o
// token que o emulador REALMENTE emite, com o formato de claim que o Firebase
// realmente usa. Foi divergência entre "o que eu acho que o provedor manda" e "o
// que ele manda" que já custou uma tarde de todo mundo.
package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/identity"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func emuladorHost() string {
	if v := os.Getenv("FIREBASE_AUTH_EMULATOR_HOST"); v != "" {
		return v
	}
	return "auth.localtest.me:8080"
}

func projetoDoEmulador() string {
	if v := os.Getenv("FIREBASE_PROJECT"); v != "" {
		return v
	}
	return "dop-local"
}

func exigeEmulador(t *testing.T) (host, projeto string) {
	t.Helper()
	host, projeto = emuladorHost(), projetoDoEmulador()
	url := fmt.Sprintf("http://%s/emulator/v1/projects/%s/config", host, projeto)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("emulador do Firebase Auth indisponível em %s: %v — suba o ambiente local "+
			"(Ingress do k3d) ou aponte FIREBASE_AUTH_EMULATOR_HOST", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		t.Skipf("o emulador em %s respondeu HTTP %d para o projeto %q — projeto errado "+
			"ou emulador de outra instalação", host, resp.StatusCode, projeto)
	}
	return host, projeto
}

func TestIdentityProviderContractFirebaseEmulador(t *testing.T) {
	host, projeto := exigeEmulador(t)
	t.Logf("emulador em %s, projeto %q — tokens SEM assinatura (alg:none)", host, projeto)

	contract.IdentityProviderSuite(t, "firebase_emulador", func(t *testing.T) contract.IdentityEnv {
		idp := identity.NewFirebaseFrom(identity.FirebaseConfig{
			ProjectID:    projeto,
			EmulatorHost: host,
		})
		if !idp.UsingEmulator() {
			t.Fatal("o adaptador não entrou em modo emulador: o token do emulador seria recusado")
		}
		return contract.IdentityEnv{
			Provider: idp,
			Unsigned: true,
			UnsignedWhy: "o emulador do Firebase Auth emite tokens com alg:none; " +
				"a assinatura é coberta em TestIdentityProviderContract/firebase_producao",
			Mint: func(t *testing.T, s contract.TokenSpec) (string, bool) {
				// Nada de assinatura para fabricar: o emulador não assina, e é
				// exatamente por isso que estes casos não são verificáveis aqui.
				if s.WrongKey || s.Symmetric {
					return "", false
				}
				c := claimsBase(s, "https://securetoken.google.com/"+projeto, projeto)
				if s.Subject != "" {
					c["user_id"] = s.Subject
				}
				if len(s.Providers) > 0 {
					c["firebase"] = map[string]any{"sign_in_provider": s.Providers[0]}
				}
				cab := objetoB64(t, map[string]any{"alg": "none", "typ": "JWT"})
				return cab + "." + objetoB64(t, c) + ".", true
			},
		}
	})
}

// TestIdentityProviderFirebaseTokenRealDoEmulador é o complemento da suíte: em
// vez de um token que a suíte montou, um token que o emulador EMITIU.
//
// É o que fecha a distância entre o formato que este adaptador espera e o que o
// provedor de fato manda — inclusive o detalhe de o emulador preencher `user_id`
// e `sub`, e de `firebase.identities` vir junto do `sign_in_provider`.
func TestIdentityProviderFirebaseTokenRealDoEmulador(t *testing.T) {
	host, projeto := exigeEmulador(t)

	email := fmt.Sprintf("contrato-%d@example.com", time.Now().UnixNano())
	corpo, _ := json.Marshal(map[string]any{
		"email":             email,
		"password":          "senha-de-teste-do-contrato",
		"returnSecureToken": true,
	})
	url := fmt.Sprintf("http://%s/identitytoolkit.googleapis.com/v1/accounts:signUp?key=chave-falsa-do-emulador", host)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(corpo))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("não foi possível criar usuário no emulador: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		IDToken string `json:"idToken"`
		LocalID string `json:"localId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.IDToken == "" {
		t.Skipf("o emulador não devolveu idToken (HTTP %d): %v", resp.StatusCode, err)
	}

	idp := identity.NewFirebaseFrom(identity.FirebaseConfig{ProjectID: projeto, EmulatorHost: host})
	p, err := idp.VerifyToken(context.Background(), out.IDToken)
	if err != nil {
		t.Fatalf("token EMITIDO pelo emulador foi recusado pelo adaptador: %v", err)
	}
	if p.Subject != out.LocalID {
		t.Errorf("Subject %q não é o localId %q que o emulador criou", p.Subject, out.LocalID)
	}
	if p.Email != email {
		t.Errorf("Email %q divergente do cadastrado %q", p.Email, email)
	}
	if p.EmailVerified {
		t.Error("usuário recém-criado por senha não tem e-mail verificado, e o adaptador disse que tem")
	}
	if len(p.Providers) == 0 {
		t.Error("o token traz firebase.sign_in_provider e Providers saiu vazio")
	}
	t.Logf("principal normalizado do token real: %+v", p)
}
