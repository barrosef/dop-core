// An IdentityProvider adapter over Firebase Authentication.
//
// The frontier ADR-0001 protects: Firebase claims do NOT cross into the domain.
// The token is verified here and a normalized Principal is returned — it is what
// allows swapping in Keycloak, Zitadel or Ory without touching the domain.
//
// The verification uses the SAME primitives as the OIDC adapter (parseJWT,
// keySet, registeredClaims.validate, see oidc.go). All that changes is where the
// public key comes from and its format: Google publishes X.509 CERTIFICATES by
// `kid` instead of a JWKS, for Secure Token Service tokens.
//
// About the EMULATOR (FIREBASE_AUTH_EMULATOR_HOST): it issues tokens with
// "alg":"none" — no signature at all. That is the emulator's behaviour, not real
// Firebase's. In this mode the signature verification is SKIPPED, and only that:
// the issuer, the audience, expiry, nbf and subject are still checked, and the
// normalization is identical, so the domain sees exactly the same thing in both
// environments. The mode is turned on by an environment variable and never by
// the token's content — a token asking not to be verified is exactly what an
// attacker would send.
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

// googleSecureTokenCerts is where Google publishes the keys that sign Firebase
// ID tokens. It is an X.509 CERTIFICATE endpoint (kid → PEM), not a JWKS:
// Google's JWKS document (`/oauth2/v3/certs`) serves Google Sign-In tokens,
// which are different ones. Pointing at the wrong one gives "unknown key" on
// every login — a silent failure and an expensive one to find.
const googleSecureTokenCerts = "https://www.googleapis.com/robots/v1/metadata/x509/securetoken@system.gserviceaccount.com"

// firebaseIssuerPrefix + projectID is the `iss` every Firebase ID token
// carries, the emulator included.
const firebaseIssuerPrefix = "https://securetoken.google.com/"

type FirebaseConfig struct {
	ProjectID string
	// CertsURL exists so the contract suite can serve local certificates and
	// exercise the signature verification FOR REAL, with no internet and with no
	// dependency on the emulator (which signs nothing). Empty = Google's
	// endpoint.
	CertsURL string
	// An empty EmulatorHost makes the adapter read FIREBASE_AUTH_EMULATOR_HOST,
	// which is what the composition root already configures.
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

// NewFirebase is the form the composition root uses (internal/app/wire.go).
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

// fetchCerts reads the kid → PEM certificate map and extracts the RSA public key.
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
		// The certificate's validity is NOT checked here, on purpose: what
		// matters is the public key Google publishes NOW, and Google removes the
		// key it retired from the endpoint. Rejecting by date would only add a
		// second source of "nobody gets in any more".
		if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok && pub.N.BitLen() >= 2048 {
			out[kid] = pub
		}
	}
	if len(out) == 0 {
		return nil, errs.New(errs.KindUnavailable, "no usable signing certificate at Google's endpoint")
	}
	return out, nil
}

type firebaseClaims struct {
	registeredClaims
	// UserID is the old name of the subject in Firebase tokens; the emulator
	// sends both. It stays here as a fallback, and it never replaces guarantee
	// 3's subject check.
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
	// With no project configured there is no issuer and no audience to compare
	// against, and accepting the token anyway would mean accepting a token from
	// ANY Firebase project — which is a third party's account coming in as a
	// user of ours. It fails closed, and as an unavailability: the defect is the
	// installation's, not the caller's (guarantee 7).
	if f.projectID == "" {
		return nil, errs.New(errs.KindUnavailable, "identity adapter with no project configured")
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
		return nil, errs.New(errs.KindUnauthorized, "unreadable token claims")
	}
	if c.Sub == "" {
		c.Sub = c.UserID
	}
	if err := c.validate(f.now(), f.skew, firebaseIssuerPrefix+f.projectID, f.projectID); err != nil {
		return nil, err
	}
	// firebase.identities carries the LINKED providers and sign_in_provider the
	// one used now. Both become the same normalized list: for the domain the
	// question is "where does this person come in from", and the difference
	// between the two does not exist on the other side of the port
	// (guarantee 6).
	// The order is pinned with sort because Go's map iteration is random, and
	// guarantee 9 promises the SAME result for the same token.
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
