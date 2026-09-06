//go:build integration

// The SAME contract suite, now against the real Firebase Auth emulator.
//
//	go test ./test/contract/ -tags=integration -run Identity -v
//
// The local k3d emulator answers at http://auth.localtest.me:8080 through the
// Ingress; FIREBASE_AUTH_EMULATOR_HOST points somewhere else.
//
// WARNING — the trap this file exists in order not to fall into:
//
// the emulator issues tokens with "alg":"none", WITH NO signature. That is the
// emulator's behaviour, not real Firebase's, and it means a suite that only ran
// here would pass green without exercising a single line of signature
// verification — precisely the part whose failure hands over any user's account
// to whoever can assemble a JSON. Hence:
//
//   - the environment declares Unsigned, and the signature cases show up as
//     SKIPs with a warning, instead of passing in silence;
//   - the SAME adapter's signature coverage comes from
//     identityprovider_test.go, which always runs, with the adapter in
//     production mode and X.509 certificates served locally.
//
// What THIS file proves, and the other cannot: that the adapter accepts the
// token the emulator REALLY issues, with the claim format Firebase really uses.
// It was a divergence between "what I think the provider sends" and "what it
// sends" that once cost everybody an afternoon.
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

func exigeEmulador(t *testing.T) (host, project string) {
	t.Helper()
	host, project = emuladorHost(), projetoDoEmulador()
	url := fmt.Sprintf("http://%s/emulator/v1/projects/%s/config", host, project)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Firebase Auth emulator unavailable at %s: %v — bring the local environment up "+
			"(Ingress do k3d) ou aponte FIREBASE_AUTH_EMULATOR_HOST", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		t.Skipf("the emulator at %s answered HTTP %d for project %q — the wrong project "+
			"or another installation's emulator", host, resp.StatusCode, project)
	}
	return host, project
}

func TestIdentityProviderContractFirebaseEmulador(t *testing.T) {
	host, project := exigeEmulador(t)
	t.Logf("emulator at %s, project %q — tokens WITH NO signature (alg:none)", host, project)

	contract.IdentityProviderSuite(t, "firebase_emulador", func(t *testing.T) contract.IdentityEnv {
		idp := identity.NewFirebaseFrom(identity.FirebaseConfig{
			ProjectID:    project,
			EmulatorHost: host,
		})
		if !idp.UsingEmulator() {
			t.Fatal("the adapter did not enter emulator mode: the emulator's token would be refused")
		}
		return contract.IdentityEnv{
			Provider: idp,
			Unsigned: true,
			UnsignedWhy: "the Firebase Auth emulator issues tokens with alg:none; " +
				"the signature is covered in TestIdentityProviderContract/firebase_production",
			Mint: func(t *testing.T, s contract.TokenSpec) (string, bool) {
				// No signature to fabricate: the emulator does not sign, and that
				// is exactly why these cases are not verifiable here.
				if s.WrongKey || s.Symmetric {
					return "", false
				}
				c := claimsBase(s, "https://securetoken.google.com/"+project, project)
				if s.Subject != "" {
					c["user_id"] = s.Subject
				}
				if len(s.Providers) > 0 {
					c["firebase"] = firebaseProviderClaims(s.Providers)
				}
				cab := objectB64(t, map[string]any{"alg": "none", "typ": "JWT"})
				return cab + "." + objectB64(t, c) + ".", true
			},
		}
	})
}

// TestIdentityProviderFirebaseRealEmulatorToken is the suite's complement:
// instead of a token the suite assembled, a token the emulator ISSUED.
//
// It is what closes the distance between the format this adapter expects and
// what the provider actually sends — the detail of the emulator filling in both
// `user_id` and `sub` included, and of `firebase.identities` coming along with
// `sign_in_provider`.
func TestIdentityProviderFirebaseRealEmulatorToken(t *testing.T) {
	host, project := exigeEmulador(t)

	email := fmt.Sprintf("contract-%d@example.com", time.Now().UnixNano())
	corpo, _ := json.Marshal(map[string]any{
		"email":             email,
		"password":          "contract-test-password",
		"returnSecureToken": true,
	})
	url := fmt.Sprintf("http://%s/identitytoolkit.googleapis.com/v1/accounts:signUp?key=fake-emulator-key", host)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(corpo))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("could not create a user in the emulator: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		IDToken string `json:"idToken"`
		LocalID string `json:"localId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.IDToken == "" {
		t.Skipf("the emulator did not return an idToken (HTTP %d): %v", resp.StatusCode, err)
	}

	idp := identity.NewFirebaseFrom(identity.FirebaseConfig{ProjectID: project, EmulatorHost: host})
	p, err := idp.VerifyToken(context.Background(), out.IDToken)
	if err != nil {
		t.Fatalf("a token ISSUED by the emulator was refused by the adapter: %v", err)
	}
	if p.Subject != out.LocalID {
		t.Errorf("Subject %q is not the localId %q the emulator created", p.Subject, out.LocalID)
	}
	if p.Email != email {
		t.Errorf("Email %q divergente do cadastrado %q", p.Email, email)
	}
	if p.EmailVerified {
		t.Error("a user just created by password has no verified e-mail, and the adapter said it does")
	}
	// This is the ONE place a real Firebase token's provider shape is observed
	// rather than assembled, so it asserts the VALUES and not just the length.
	// "not empty" was the assertion here before, and it is what let the domain
	// believe a password login arrives as ["password"]: the emulator signs this
	// person up with an e-mail and a password, and what actually comes back is
	// the identifier type "email" ALONGSIDE the provider "password".
	seen := map[string]bool{}
	for _, v := range p.Providers {
		seen[v] = true
	}
	if !seen["password"] {
		t.Errorf("an e-mail/password sign-up and Providers does not report the password: %v", p.Providers)
	}
	if !seen["email"] {
		t.Errorf("firebase.identities is keyed by identifier type and the adapter dropped it — "+
			"the domain would be reading a list Firebase never sends: %v", p.Providers)
	}
	t.Logf("normalized principal from the real token: %+v", p)
}
