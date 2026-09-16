// An IdentityProvider adapter over a generic OIDC issuer.
//
// It is this adapter that makes the platform run in a self-hosted cluster —
// Keycloak, Dex, Authentik, Zitadel — with no Firebase and without touching the
// domain, which is ADR-0001's portability case. Running the SAME guarantees
// against it and against Firebase is what proves the port is a port, and not
// Firebase's interface under another name.
//
// No vendor SDK: net/http to talk to the issuer and crypto/rsa for the
// signature. There is no JOSE library in this module's go.mod (the only
// dependencies are gRPC, pgx and NATS), and bringing one in to verify RS256
// would mean paying a dependency tree, and a CVE surface, for thirty lines of
// crypto/rsa. RS256/384/512 is what OIDC issuers actually use.
//
// This file's primitives (parseJWT, keySet, registered-claim validation) belong
// to the PACKAGE, not to this adapter: the Firebase adapter uses exactly the
// same ones. Two token-verification implementations in the same package would be
// two chances to get the same thing wrong.
package identity

import (
	"context"
	"crypto"
	"crypto/rsa"
	_ "crypto/sha256" // registers SHA-256/384 for crypto.Hash.New() (RS256/RS384)
	_ "crypto/sha512" // likewise for SHA-512 (RS512)
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// defaultClockSkew is the clock tolerance applied to exp, nbf and iat.
//
// It is ONE MINUTE, in both directions, and the choice is deliberate:
//
//   - with no tolerance at all, any drift between the issuer's clock and ours
//     becomes an intermittent "expired token" for the user. It is the worst kind
//     of failure: random, it vanishes when somebody goes to investigate, and it
//     blames the password of whoever typed it correctly. A node with healthy NTP
//     stays within tens of milliseconds, but a VM back from suspension, a
//     freshly scheduled container and a host with no NTP are off by seconds;
//   - with a large tolerance, a stolen token keeps working for the whole
//     tolerated window AFTER expiring. That is an attack window bought with
//     operational comfort.
//
// One minute is the value OIDC Core suggests for bounding iat, it is what the
// reference verifiers use, it covers realistic drift and it keeps the slack far
// smaller than the shortest expiry in use (one hour, in Firebase).
const defaultClockSkew = time.Minute

// defaultKeysMinRefresh limits how often an unknown `kid` can send the process
// off to fetch a key from the issuer. See keySet.publicKey.
const defaultKeysMinRefresh = 30 * time.Second

// maxIssuerResponse caps the issuer's response. The issuer is a third party's
// infrastructure: when it gets sick, it does not return an error — it returns a
// proxy page, or a stream that never ends.
const maxIssuerResponse = 1 << 20 // 1 MiB

func defaultHTTPClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

// ───────────────────────── JWT: parsing and verification ─────────────────────

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// jwtParts is the token already split, and NOTHING beyond that: parsing is not
// verification. signed is the exact material the signature was made over — the
// first two parts with the dot in the middle, AS TEXT, without re-serializing.
// Rebuilding the JSON to sign again is how verification gets broken without
// anyone noticing: one extra space and the legitimate signature does not
// match.
type jwtParts struct {
	header  jwtHeader
	payload []byte
	signed  []byte
	sig     []byte
}

// parseJWT splits the token. Every failure here is KindUnauthorized (guarantee
// 1) and no message carries material from the token (guarantee 2).
func parseJWT(raw string) (*jwtParts, error) {
	// The HTTP edge hands over "Bearer <token>"; the gRPC one hands over the
	// same header. Accepting both shapes here stops each caller from inventing
	// its own (guarantee 10).
	raw = strings.TrimSpace(raw)
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "bearer ") {
		raw = strings.TrimSpace(raw[7:])
	}
	if raw == "" {
		// No I/O: calling with no credential must not cost a round trip to the
		// issuer.
		return nil, errs.New(errs.KindUnauthorized, "token absent")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errs.New(errs.KindUnauthorized, "malformed token: three parts expected")
	}
	hb, err := decodeSegment(parts[0])
	if err != nil {
		return nil, errs.New(errs.KindUnauthorized, "unreadable token header")
	}
	var h jwtHeader
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, errs.New(errs.KindUnauthorized, "the token header is not valid JSON")
	}
	pb, err := decodeSegment(parts[1])
	if err != nil {
		return nil, errs.New(errs.KindUnauthorized, "unreadable token payload")
	}
	sig, err := decodeSegment(parts[2])
	if err != nil {
		return nil, errs.New(errs.KindUnauthorized, "unreadable token signature")
	}
	return &jwtParts{
		header:  h,
		payload: pb,
		signed:  []byte(parts[0] + "." + parts[1]),
		sig:     sig,
	}, nil
}

// decodeSegment tolerates the "=" padding some issuers send, even though JWS
// forbids it. Refusing a legitimate token over that would be rigour with no
// security gain at all.
func decodeSegment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// checkRSAAlg is the line of defence against algorithm confusion.
//
// We accept ONLY RS256/384/512. The two important refusals:
//
//   - "none" waives the signature — it is literally what the Firebase emulator
//     issues, and accepting it here would mean anyone can assemble a token with
//     whatever `sub` they like;
//   - HS* is a MAC with a shared secret. Since the "secret" we would have at
//     hand is the issuer's PUBLIC key, accepting HS* lets the attacker sign with
//     a piece of data they also have. It is the bug that has already taken down
//     a famous JWT library, and it only exists in a verifier that trusts the
//     token's own `alg`.
func checkRSAAlg(alg string) (crypto.Hash, error) {
	switch alg {
	case "RS256":
		return crypto.SHA256, nil
	case "RS384":
		return crypto.SHA384, nil
	case "RS512":
		return crypto.SHA512, nil
	default:
		// The message does not repeat the alg received: it is token content.
		return 0, errs.New(errs.KindUnauthorized, "signature algorithm not accepted")
	}
}

func verifyRSASignature(key *rsa.PublicKey, tok *jwtParts) error {
	h, err := checkRSAAlg(tok.header.Alg)
	if err != nil {
		return err
	}
	sum := h.New()
	sum.Write(tok.signed)
	if err := rsa.VerifyPKCS1v15(key, h, sum.Sum(nil), tok.sig); err != nil {
		return errs.New(errs.KindUnauthorized, "invalid token signature")
	}
	return nil
}

// ───────────────────────── Registered claims ─────────────────────────

// audience accepts a string OR a list, as RFC 7519 §4.1.3 requires. Firebase
// sends a string, Keycloak sends a list; a verifier that understands only one of
// the shapes refuses the other issuer's legitimate token.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("aud claim in an unexpected format")
	}
	*a = many
	return nil
}

func (a audience) has(v string) bool {
	for _, s := range a {
		if s == v {
			return true
		}
	}
	return false
}

// flexBool exists because email_verified arrives as a boolean in most issuers
// and as the STRING "true" in a few (Azure AD, realms with an attribute mapper).
// Treating the string as an invalid format would downgrade an email that really
// is verified — and guarantee 5's insecure default is only honest when the
// issuer really said nothing.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("boolean claim in an unexpected format")
	}
	switch t := v.(type) {
	case bool:
		*f = flexBool(t)
	case string:
		*f = flexBool(strings.EqualFold(t, "true"))
	case nil:
		*f = false
	default:
		return fmt.Errorf("boolean claim in an unexpected format")
	}
	return nil
}

type registeredClaims struct {
	Iss string   `json:"iss"`
	Sub string   `json:"sub"`
	Aud audience `json:"aud"`
	Exp int64    `json:"exp"`
	Nbf int64    `json:"nbf"`
	Iat int64    `json:"iat"`
}

// validate covers guarantee 1 in the part that does not depend on cryptography.
// It is called AFTER the signature, always: validating the claims of an unsigned
// token is asking the attacker whether they are trustworthy.
func (rc registeredClaims) validate(now time.Time, skew time.Duration, issuer, aud string) error {
	if issuer != "" && rc.Iss != issuer {
		return errs.New(errs.KindUnauthorized, "token from an unexpected issuer")
	}
	if aud != "" && !rc.Aud.has(aud) {
		return errs.New(errs.KindUnauthorized, "token issued for another audience")
	}
	// A token with no exp is a permanent credential. No serious issuer emits
	// one, and accepting it would let a stolen token become lifetime access.
	if rc.Exp == 0 {
		return errs.New(errs.KindUnauthorized, "token with no expiry")
	}
	if now.After(time.Unix(rc.Exp, 0).Add(skew)) {
		return errs.New(errs.KindUnauthorized, "expired token")
	}
	if rc.Nbf != 0 && now.Before(time.Unix(rc.Nbf, 0).Add(-skew)) {
		return errs.New(errs.KindUnauthorized, "token not valid yet")
	}
	if rc.Iat != 0 && now.Before(time.Unix(rc.Iat, 0).Add(-skew)) {
		return errs.New(errs.KindUnauthorized, "token issued in the future")
	}
	if rc.Sub == "" {
		return errs.New(errs.KindUnauthorized, "token with no subject")
	}
	return nil
}

// ───────────────────────── Public key cache ─────────────────────────

// keySet is the cache of the issuer's public keys, and both halves of guarantee
// 8 live here.
//
// Fetching the key on every request is a denial of service against the issuer
// itself: at peak traffic, every login becomes an extra call to Keycloak, and
// when it gives way the WHOLE platform goes down with it. Hence the cache.
//
// Not revalidating, on the other hand, turns key rotation — routine in Keycloak
// and in Firebase — into a total login outage: the new tokens come with a `kid`
// the cache does not know and nobody gets in any more. Hence an unknown `kid`
// triggering a new fetch.
//
// And it is exactly that fetch that needs a brake: without one, whoever sends
// tokens with random `kid`s turns this process into a traffic generator against
// the issuer — the attack the cache existed to avoid, coming in through the back
// door. Hence minRefresh.
type keySet struct {
	mu         sync.Mutex
	keys       map[string]*rsa.PublicKey
	fetchedAt  time.Time
	fetch      func(context.Context) (map[string]*rsa.PublicKey, error)
	minRefresh time.Duration
	now        func() time.Time
}

func (s *keySet) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	// The mutex is held DURING the I/O on purpose: with a thousand requests
	// arriving at the instant of the rotation, only one goes to the issuer and
	// the others wait for it. Without that, the rotation becomes a stampede
	// against the issuer.
	s.mu.Lock()
	defer s.mu.Unlock()

	if k, ok := s.lookup(kid); ok {
		return k, nil
	}
	if s.keys != nil && s.now().Sub(s.fetchedAt) < s.minRefresh {
		return nil, errs.New(errs.KindUnauthorized, "token signed by an unknown key")
	}
	keys, err := s.fetch(ctx) // already returns KindUnavailable (guarantee 7)
	if err != nil {
		return nil, err
	}
	s.keys, s.fetchedAt = keys, s.now()
	if k, ok := s.lookup(kid); ok {
		return k, nil
	}
	return nil, errs.New(errs.KindUnauthorized, "token signed by an unknown key")
}

func (s *keySet) lookup(kid string) (*rsa.PublicKey, bool) {
	if len(s.keys) == 0 {
		return nil, false
	}
	if k, ok := s.keys[kid]; ok {
		return k, true
	}
	// `kid` is optional in JWS. When the issuer publishes a SINGLE key there is
	// no ambiguity and refusing would be rigour that breaks a legitimate issuer.
	// With two or more, guessing would mean testing the signature against any
	// key — and then `kid` would stop having a function.
	if kid == "" && len(s.keys) == 1 {
		for _, k := range s.keys {
			return k, true
		}
	}
	return nil, false
}

// getJSON fetches and decodes JSON from the issuer. EVERY failure here is
// KindUnavailable, never KindUnauthorized (guarantee 7): the issuer being down
// says nothing about the caller's token.
func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "invalid address for the identity issuer")
	}
	resp, err := c.Do(req)
	if err != nil {
		// A cancelled context or a blown deadline also lands here, and is also
		// an unavailability — it is not the token's fault.
		return errs.Wrap(errs.KindUnavailable, err, "identity issuer unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errs.New(errs.KindUnavailable, "identity issuer answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIssuerResponse))
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "the issuer's response was interrupted")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errs.New(errs.KindUnavailable, "unreadable response from the identity issuer")
	}
	return nil
}

// ───────────────────────── The adapter ─────────────────────────

type OIDCConfig struct {
	// Issuer is the EXACT `iss` the tokens carry, and the base for discovery.
	Issuer string
	// Audience is the client_id the platform registered with the issuer. Empty
	// turns the check off, and turning it off is the configurer's choice: a
	// valid token issued for ANOTHER application of the same realm would start
	// being accepted here.
	Audience   string
	HTTPClient *http.Client
	// ClockSkew and KeysMinRefresh are adapter TUNING — the port says nothing
	// about them (see the "OUTSIDE the port" block in
	// ports.IdentityProvider).
	ClockSkew      time.Duration
	KeysMinRefresh time.Duration
	// Now exists so the contract test can prove expiry without sleeping. In
	// production it is time.Now.
	Now func() time.Time
}

type OIDC struct {
	issuer   string
	audience string
	client   *http.Client
	skew     time.Duration
	now      func() time.Time
	keys     *keySet

	mu      sync.Mutex
	jwksURI string
}

func NewOIDC(cfg OIDCConfig) *OIDC {
	o := &OIDC{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		client:   cfg.HTTPClient,
		skew:     cfg.ClockSkew,
		now:      cfg.Now,
	}
	if o.client == nil {
		o.client = defaultHTTPClient()
	}
	if o.skew == 0 {
		o.skew = defaultClockSkew
	}
	if o.now == nil {
		o.now = time.Now
	}
	minRefresh := cfg.KeysMinRefresh
	if minRefresh == 0 {
		minRefresh = defaultKeysMinRefresh
	}
	o.keys = &keySet{fetch: o.fetchJWKS, minRefresh: minRefresh, now: o.now}
	return o
}

// jwksEndpoint resolves jwks_uri through discovery, once.
//
// The discovery is LAZY on purpose: doing it at boot would tie the process's
// start-up to Keycloak's, and in a cluster that brings everything up together
// that is a guaranteed CrashLoopBackOff on the first deploy. If it failed, it
// tries again on the next verification — the cost is one request, not the
// process.
func (o *OIDC) jwksEndpoint(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.jwksURI != "" {
		return o.jwksURI, nil
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	url := strings.TrimRight(o.issuer, "/") + "/.well-known/openid-configuration"
	if err := getJSON(ctx, o.client, url, &doc); err != nil {
		return "", err
	}
	// The document has to declare the SAME issuer we configured. Without this
	// check, whoever controls DNS or routing points the discovery at another
	// issuer and starts signing our tokens. It is an unavailability, and not an
	// invalid token: it is the trusted issuer that is not there.
	if !sameIssuer(doc.Issuer, o.issuer) {
		return "", errs.New(errs.KindUnavailable,
			"discovery declares issuer %q, expected %q", doc.Issuer, o.issuer)
	}
	if doc.JWKSURI == "" {
		return "", errs.New(errs.KindUnavailable, "discovery with no jwks_uri")
	}
	o.jwksURI = doc.JWKSURI
	return o.jwksURI, nil
}

// sameIssuer compares ignoring the trailing slash. Some issuers publish `iss`
// with a slash (Auth0) and others without; the difference is not semantic, but
// it breaks the exact comparison for whoever configured it one way and received
// it the other.
func sameIssuer(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (o *OIDC) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	uri, err := o.jwksEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := getJSON(ctx, o.client, uri, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		// An encryption key does not sign, and a non-RSA key this adapter does
		// not verify: ignoring them silently is the right thing here, because a
		// real realm's JWKS has material that is not for us.
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pub, err := rsaFromJWK(k)
		if err != nil {
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errs.New(errs.KindUnavailable, "the issuer published no usable RSA signing key")
	}
	return out, nil
}

func rsaFromJWK(k jwk) (*rsa.PublicKey, error) {
	nb, err := decodeSegment(k.N)
	if err != nil {
		return nil, err
	}
	eb, err := decodeSegment(k.E)
	if err != nil {
		return nil, err
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31 {
		return nil, fmt.Errorf("exponent outside the acceptable range")
	}
	n := new(big.Int).SetBytes(nb)
	// A short modulus is a weak key: 2048 bits is the minimum any production
	// issuer uses, and accepting less would mean accepting a forgeable
	// signature.
	if n.BitLen() < 2048 {
		return nil, fmt.Errorf("RSA modulus too short")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

type oidcClaims struct {
	registeredClaims
	Email             string   `json:"email"`
	EmailVerified     flexBool `json:"email_verified"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Picture           string   `json:"picture"`
	// Amr is how the subject authenticated; idp/identity_provider is the
	// brokered external provider (Keycloak's broker). Neither is mandatory —
	// and that is why guarantee 6 allows an empty list.
	Amr              []string `json:"amr"`
	Idp              string   `json:"idp"`
	IdentityProvider string   `json:"identity_provider"`
}

func (o *OIDC) VerifyToken(ctx context.Context, raw string) (*ports.Principal, error) {
	tok, err := parseJWT(raw)
	if err != nil {
		return nil, err
	}
	if _, err := checkRSAAlg(tok.header.Alg); err != nil {
		return nil, err
	}
	key, err := o.keys.publicKey(ctx, tok.header.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifyRSASignature(key, tok); err != nil {
		return nil, err
	}
	// Only now are the claims worth anything: the signature checks out.
	var c oidcClaims
	if err := json.Unmarshal(tok.payload, &c); err != nil {
		return nil, errs.New(errs.KindUnauthorized, "unreadable token claims")
	}
	if err := c.validate(o.now(), o.skew, o.issuer, o.audience); err != nil {
		return nil, err
	}
	return &ports.Principal{
		Subject:       c.Sub,
		Email:         c.Email,
		EmailVerified: bool(c.EmailVerified),
		// preferred_username is what Keycloak always fills in; `name` only
		// appears with the profile scope. Falling from one to the other gives
		// the domain a usable label instead of an empty one — and it remains an
		// OPTIONAL field, which nobody may use as an identity (guarantee 4).
		Name:      firstNonEmpty(c.Name, c.PreferredUsername),
		AvatarURL: c.Picture,
		Providers: normalizeProviders(append([]string{c.Idp, c.IdentityProvider}, c.Amr...)),
	}, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// normalizeProviders returns lowercase, with no empties and no repetition, and
// ALWAYS a non-nil slice: guarantee 6 says "I do not know" is an EMPTY list, and
// a nil that means something is the next wrong interpretation waiting to
// happen.
func normalizeProviders(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		// "pwd" is the name OIDC gives to what Firebase calls "password".
		// Normalizing here is what makes Providers comparable between the two
		// adapters — without it, the field would be the issuer's vocabulary
		// leaking through the port under another name.
		if v == "pwd" {
			v = "password"
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

var _ ports.IdentityProvider = (*OIDC)(nil)
