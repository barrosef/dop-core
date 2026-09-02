package contract

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// TokenSpec describes the token the suite wants THIS issuer to produce.
//
// The suite does not know how to build a token: each issuer has its own claim
// format (Firebase puts the provider in `firebase.sign_in_provider`, OIDC in
// `amr`), and what matters is that the PRINCIPAL on the other side comes out the
// same. That is why the suite describes the INTENT and the environment
// translates.
type TokenSpec struct {
	Subject       string
	Email         string
	EmailVerified *bool // nil = the claim does not go into the token
	Name          string
	Picture       string
	Providers     []string
	// Extra are claims the issuer carries and the domain must NOT see.
	Extra map[string]any

	// Empty = the correct value. Filled in = the token is issued wrong on
	// purpose.
	Issuer   string
	Audience string

	IssuedAt  time.Time
	NotBefore time.Time
	Expiry    time.Time // zero = one hour in the future

	// The signature cases. An issuer that cannot produce them returns ok=false.
	WrongKey  bool // signed by another key, with another kid
	Unsigned  bool // "alg":"none"
	Symmetric bool // HS256 using the public key as the secret
}

// IdentityEnv is what THIS issuer offers for the suite to work with.
type IdentityEnv struct {
	Provider ports.IdentityProvider

	// Mint returns the raw token. ok=false means "this issuer cannot produce
	// that case" — the subtest is SKIPPED with a record, never in silence.
	Mint func(t *testing.T, s TokenSpec) (raw string, ok bool)

	// Unsigned marks an issuer that signs nothing.
	//
	// It exists because of a real trap: the Firebase emulator issues tokens with
	// "alg":"none", and a suite that only ran against it would pass green
	// without proving ONE line of signature verification — which is precisely
	// the part whose failure hands over any user's account. When this is true,
	// the signature cases are skipped with a SHOUTED warning, and the coverage
	// has to come from another environment of the same adapter.
	Unsigned    bool
	UnsignedWhy string

	// Fetches counts round trips to the issuer after a key. nil = not observable
	// (and then guarantee 8 and part of 7 are not verifiable here).
	Fetches func() int
	// Rotate swaps the issuer's signing key, like a real rotation: the new key
	// has a different kid.
	Rotate func(t *testing.T)
	// MinRefresh is the re-fetch brake configured in this adapter.
	MinRefresh time.Duration
	// IssuerDown brings the issuer down.
	IssuerDown func(t *testing.T)
}

// IdentityProviderSuite verifies the eleven guarantees documented on the
// ports.IdentityProvider port.
//
// ADR-0001's discipline: a port with a single adapter is guesswork. Firebase and
// a generic OIDC issuer share neither the public key's format (an X.509
// certificate on one side, a JWKS on the other) nor the claim vocabulary — and
// it is putting both through the SAME eleven checks that turns "changing the
// identity provider is wiring" into a fact.
func IdentityProviderSuite(t *testing.T, name string, newEnv func(t *testing.T) IdentityEnv) {
	t.Run(name, func(t *testing.T) {
		ctx := context.Background()

		// mint aborts the subtest when the issuer cannot produce the case —
		// skipping with a message, which is the opposite of pretending it
		// passed.
		mint := func(t *testing.T, env IdentityEnv, s TokenSpec) string {
			t.Helper()
			raw, ok := env.Mint(t, s)
			if !ok {
				t.Skipf("this issuer cannot produce the requested token — the case is not verifiable here")
			}
			return raw
		}
		t.Run("1_an_implausible_token_is_unauthorized", func(t *testing.T) {
			env := newEnv(t)

			// These the suite builds on its own: no issuer needs to cooperate for
			// junk to be refused.
			lixo := map[string]string{
				"emptyEnv":                 "",
				"so_espaco":                "   ",
				"only_the_bearer_prefix":   "Bearer ",
				"not_a_jwt":                "this-is-not-a-token",
				"duas_partes":              "aaaabbbb.ccccdddd",
				"quatro_partes":            "aaaabbbb.ccccdddd.eeeeffff.gggghhhh",
				"base64_invalido":          "@@@@@@@@.@@@@@@@@.@@@@@@@@",
				"cabecalho_nao_e_json":     "bm90LWpzb24.bm90LWpzb24.YWJjZA",
				"tres_partes_vazias_com_h": "..",
			}
			for nome, raw := range lixo {
				t.Run(nome, func(t *testing.T) {
					p, err := env.Provider.VerifyToken(ctx, raw)
					mustRefuse(t, p, err, raw)
				})
			}

			agora := time.Now()
			casos := map[string]TokenSpec{
				"expirado":          {Subject: "s", Expiry: agora.Add(-2 * time.Hour)},
				"ainda_nao_valido":  {Subject: "s", NotBefore: agora.Add(2 * time.Hour)},
				"emitido_no_futuro": {Subject: "s", IssuedAt: agora.Add(2 * time.Hour)},
				"wrong_issuer":      {Subject: "s", Issuer: "https://issuer-of-another-installation.example"},
				"audiencia_errada":  {Subject: "s", Audience: "outra-aplicacao"},
				"no_subject":        {},
			}
			for nome, spec := range casos {
				t.Run(nome, func(t *testing.T) {
					env := newEnv(t)
					raw := mint(t, env, spec)
					p, err := env.Provider.VerifyToken(ctx, raw)
					mustRefuse(t, p, err, raw)
				})
			}

			// The signature cases. They are what separates "I verified the token"
			// from "I read the token".
			assinatura := map[string]TokenSpec{
				"signed_by_an_intruding_key": {Subject: "s", WrongKey: true},
				"sem_assinatura_alg_none":    {Subject: "s", Unsigned: true},
				"algoritmo_simetrico_hs256":  {Subject: "s", Symmetric: true},
			}
			for nome, spec := range assinatura {
				t.Run(nome, func(t *testing.T) {
					env := newEnv(t)
					if env.Unsigned {
						t.Skipf("WARNING: this issuer does NOT sign tokens (%s). "+
							"Signature verification is left UNCOVERED here, and the "+
							"coverage has to come from another environment of the same "+
							"adapter — a suite that only ran against it would pass green "+
							"proving nothing.",
							env.UnsignedWhy)
					}
					raw := mint(t, env, spec)
					p, err := env.Provider.VerifyToken(ctx, raw)
					mustRefuse(t, p, err, raw)
				})
			}
		})

		t.Run("2_the_error_does_not_leak_the_token", func(t *testing.T) {
			env := newEnv(t)
			const secret = "a-claim-that-must-not-appear-in-a-log"
			raw := mint(t, env, TokenSpec{
				Subject: "s",
				Expiry:  time.Now().Add(-2 * time.Hour),
				Extra:   map[string]any{"token_secret": secret},
			})
			_, err := env.Provider.VerifyToken(ctx, raw)
			if err == nil {
				t.Fatal("an expired token was accepted")
			}
			// mustRefuse already compares against the token's parts; here the
			// target is the decoded claim, which does not travel in base64 and
			// would escape the earlier comparison.
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("LEAK: the error message carries a claim from the token: %v", err)
			}
			doesNotLeak(t, raw, err)
		})

		t.Run("3_the_subject_is_mandatory_and_stable", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "stable-subject-42"})
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("a valid token was refused: %v", err)
			}
			if p == nil || p.Subject == "" {
				t.Fatal("a nil error with an empty Subject: the domain would be left with no identity at all")
			}
			if p.Subject != "stable-subject-42" {
				t.Fatalf("Subject %q is not the token's subject", p.Subject)
			}
			// Guarantee 9, the deterministic half: the same token, the same result.
			p2, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("the second verification of the SAME token failed: %v", err)
			}
			if p2.Subject != p.Subject {
				t.Fatalf("the same token gave different subjects: %q and %q", p.Subject, p2.Subject)
			}
		})

		t.Run("4_email_name_and_avatar_are_optional", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "just-the-subject"})
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("a token with no email was refused — a phone login or SSO with no "+
					"profile scope would have no way in: %v", err)
			}
			if p.Email != "" || p.Name != "" || p.AvatarURL != "" {
				t.Fatalf("fields invented from a token that does not carry them: %+v", p)
			}
		})

		t.Run("5_an_absent_email_verified_is_false", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "s", Email: "someone@example.withPrefix"})
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("an absent claim became an error: %v", err)
			}
			if p.EmailVerified {
				t.Fatal("a silent issuer promoted the email to verified: the absence of " +
					"an assertion is not an assertion")
			}
			yes := true
			env2 := newEnv(t)
			raw2 := mint(t, env2, TokenSpec{Subject: "s", Email: "someone@example.withPrefix", EmailVerified: &yes})
			p2, err := env2.Provider.VerifyToken(ctx, raw2)
			if err != nil {
				t.Fatalf("a token with a verified email was refused: %v", err)
			}
			if !p2.EmailVerified {
				t.Fatal("the issuer asserted a verified email and the adapter swallowed the assertion")
			}
		})

		t.Run("6_providers_is_informative_and_normalized", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "s", Providers: []string{"password"}})
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			vistos := map[string]bool{}
			for _, v := range p.Providers {
				if v == "" {
					t.Error("an empty entry in Providers")
				}
				if v != strings.ToLower(v) || v != strings.TrimSpace(v) {
					t.Errorf("a provider outside the normalized vocabulary: %q", v)
				}
				if vistos[v] {
					t.Errorf("provedor repetido: %q", v)
				}
				vistos[v] = true
			}
			if !vistos["password"] {
				t.Errorf("the token says the sign-in was by password and Providers does not reflect it: %v", p.Providers)
			}

			env2 := newEnv(t)
			raw2 := mint(t, env2, TokenSpec{Subject: "s"})
			p2, err := env2.Provider.VerifyToken(ctx, raw2)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			if p2.Providers == nil {
				t.Error("Providers is nil: 'I do not know' is an EMPTY list, and nil ends up " +
					"meaning something else in the head of whoever reads it later")
			}
			if len(p2.Providers) != 0 {
				t.Errorf("the adapter invented a provider for a token that reports none: %v", p2.Providers)
			}
		})

		t.Run("7_an_issuer_that_is_down_is_unavailable", func(t *testing.T) {
			env := newEnv(t)
			if env.IssuerDown == nil {
				t.Skip("this environment cannot bring the issuer down — the guarantee is not verifiable here")
			}
			raw := mint(t, env, TokenSpec{Subject: "s"})
			env.IssuerDown(t) // the cache is still cold: the next verification needs the issuer

			p, err := env.Provider.VerifyToken(ctx, raw)
			if err == nil {
				t.Fatalf("the issuer is down and the token was accepted: %+v", p)
			}
			if k := errs.KindOf(err); k != errs.KindUnavailable {
				t.Fatalf("an unavailable issuer became %s: %v\n"+
					"that tells the user THEIR credential is wrong, to everybody at "+
					"the same time, and sends the team looking for the "+
					"defeito no lugar errado", k, err)
			}
			doesNotLeak(t, raw, err)
		})

		t.Run("7b_a_cancelled_context_is_unavailable", func(t *testing.T) {
			env := newEnv(t)
			if env.Fetches == nil {
				t.Skip("this adapter does no I/O to verify — nothing to cancel")
			}
			raw := mint(t, env, TokenSpec{Subject: "s"})
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := env.Provider.VerifyToken(cancelled, raw)
			if err == nil {
				t.Fatal("the context was cancelled and the token was verified anyway")
			}
			if k := errs.KindOf(err); k != errs.KindUnavailable {
				t.Fatalf("a cancelled context became %s: %v", k, err)
			}
		})

		t.Run("8_the_key_is_cached_and_revalidated_on_rotation", func(t *testing.T) {
			env := newEnv(t)
			if env.Fetches == nil || env.Rotate == nil {
				t.Skip("this environment neither observes key fetches nor knows how to rotate — " +
					"the guarantee is not verifiable here")
			}
			raw := mint(t, env, TokenSpec{Subject: "s"})
			if _, err := env.Provider.VerifyToken(ctx, raw); err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			after := env.Fetches()
			if after == 0 {
				t.Fatal("no key fetch at all: the adapter is not verifying the signature")
			}

			// Half 1: it caches. Fetching the key on every request is a denial of
			// service against the issuer itself.
			for i := 0; i < 5; i++ {
				if _, err := env.Provider.VerifyToken(ctx, raw); err != nil {
					t.Fatalf("verification %d: %v", i, err)
				}
			}
			if n := env.Fetches(); n != after {
				t.Fatalf("%d key fetches for %d verifications: the cache is not holding", n-after+1, 6)
			}

			// Half 2, the brake: an unknown `kid` right after a fetch is refused
			// WITHOUT going to the issuer. Without that, whoever sends tokens
			// with random kids uses this process as a traffic generator against
			// the issuer.
			if intruso, ok := env.Mint(t, TokenSpec{Subject: "s", WrongKey: true}); ok {
				if _, err := env.Provider.VerifyToken(ctx, intruso); errs.KindOf(err) != errs.KindUnauthorized {
					t.Fatalf("a token from an unknown key should be refused, got: %v", err)
				}
				if n := env.Fetches(); n != after {
					t.Errorf("kid desconhecido disparou busca imediata (%d → %d): "+
						"the re-fetch brake is not in place", after, n)
				}
			}

			// Half 3: the rotation goes through. With no re-fetch, swapping the
			// key at the issuer — routine in Keycloak and in Firebase — would
			// bring every login down.
			env.Rotate(t)
			time.Sleep(env.MinRefresh + 50*time.Millisecond)
			fresh := mint(t, env, TokenSpec{Subject: "s"})
			if _, err := env.Provider.VerifyToken(ctx, fresh); err != nil {
				t.Fatalf("after the key rotation nobody gets in any more: %v", err)
			}
			if n := env.Fetches(); n <= after {
				t.Errorf("the new key was accepted with no re-fetch (%d → %d) — that is "+
					"verification that is not happening", after, n)
			}
		})

		t.Run("9_seguro_para_uso_concorrente", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "concurrent-subject"})
			const n = 24
			var wg sync.WaitGroup
			errors := make([]error, n)
			subs := make([]string, n)
			wg.Add(n)
			for i := 0; i < n; i++ {
				go func(i int) {
					defer wg.Done()
					p, err := env.Provider.VerifyToken(ctx, raw)
					errors[i] = err
					if p != nil {
						subs[i] = p.Subject
					}
				}(i)
			}
			wg.Wait()
			for i := 0; i < n; i++ {
				if errors[i] != nil {
					t.Fatalf("concurrent verification %d failed: %v", i, errors[i])
				}
				if subs[i] != "concurrent-subject" {
					t.Fatalf("concurrent verification %d returned subject %q", i, subs[i])
				}
			}
		})

		t.Run("10_the_bearer_prefix_and_an_empty_token", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "s"})
			withPrefix, err := env.Provider.VerifyToken(ctx, "Bearer "+raw)
			if err != nil {
				t.Fatalf("a token with the HTTP edge's 'Bearer ' prefix was refused: %v", err)
			}
			withoutPrefix, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			if withPrefix.Subject != withoutPrefix.Subject {
				t.Fatalf("with and without the prefix gave different subjects: %q and %q", withPrefix.Subject, withoutPrefix.Subject)
			}

			emptyEnv := newEnv(t)
			before := 0
			if emptyEnv.Fetches != nil {
				before = emptyEnv.Fetches()
			}
			p, err := emptyEnv.Provider.VerifyToken(ctx, "")
			mustRefuse(t, p, err, "")
			if emptyEnv.Fetches != nil && emptyEnv.Fetches() != before {
				t.Error("an empty token cost a round trip to the issuer: calling with no " +
					"credential must not become traffic against the identity provider")
			}
		})

		t.Run("11_a_raw_claim_does_not_cross_the_port", func(t *testing.T) {
			env := newEnv(t)
			const forjado = "administrador-forjado"
			raw, ok := env.Mint(t, TokenSpec{
				Subject: "s",
				Extra: map[string]any{
					"role":         forjado,
					"custom_claim": forjado,
					"scope":        forjado,
				},
			})
			if !ok {
				t.Skip("this issuer cannot carry an extra claim — the case is not verifiable here")
			}
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			campos := append([]string{p.Subject, p.Email, p.Name, p.AvatarURL}, p.Providers...)
			for _, v := range campos {
				if strings.Contains(v, forjado) {
					t.Fatalf("claim de fornecedor atravessou a porta: %q", v)
				}
			}
		})
	})
}

// mustRefuse is guarantee 1 plus 2, together: refusing with the right Kind and
// without carrying the token in the message.
func mustRefuse(t *testing.T, p *ports.Principal, err error, raw string) {
	t.Helper()
	if err == nil {
		t.Fatalf("a token that does not prove itself was ACCEPTED, returning %+v", p)
	}
	if p != nil {
		t.Errorf("an error and a Principal at the same time: %+v", p)
	}
	if k := errs.KindOf(err); k != errs.KindUnauthorized {
		t.Fatalf("expected %s, got %s: %v", errs.KindUnauthorized, k, err)
	}
	doesNotLeak(t, raw, err)
}

// doesNotLeak checks that neither the token nor any of its parts appears in the
// message. A token in a log is a credential at rest: whoever reads the log gets
// in as its owner, and a log is where an error message lives.
func doesNotLeak(t *testing.T, raw string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	pedacos := append([]string{raw}, strings.Split(raw, ".")...)
	for _, p := range pedacos {
		// A short piece matches by accident ("a", ".."); a long piece is real
		// token material.
		if len(p) >= 8 && strings.Contains(msg, p) {
			t.Fatalf("LEAK: the error message carries the token (or part of it): %s", msg)
		}
	}
}
