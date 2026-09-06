package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/callauth"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

var (
	errNoResolver     = errs.New(errs.KindInternal, "no subject resolver wired")
	errUnknownSubject = errs.New(errs.KindNotFound, "subject with no user")
)

// The core's authentication of ITS OWN callers (ADR-0029, P-18 solution F).
//
// Until this existed, the core read `x-actor-id` and believed it. The metadata
// is text: whoever could open a connection to the gRPC port declared themselves
// any actor of any account, and the only thing in the way was a NetworkPolicy —
// a property of the deployment, which fails silently when it is absent.
//
// Now every call has to carry a signature, and WHICH signature depends on who is
// calling:
//
//   - a call with a PERSON behind it carries that person's token, and the core
//     verifies it through the `IdentityProvider` port it already had. The
//     signature is the identity provider's — an authority neither the edge nor
//     the core controls, which makes it the strongest proof available and the
//     cheapest to obtain;
//   - a call with NO person — the edge resolving a subject before an actor
//     exists — carries an assertion signed by the platform (callauth).
//
// The account never comes from the token, because it is not in it: it comes from
// the assertion, which is signed too.
//
// ── The modes, and why the default is not the strict one ────────────────────
//
// `strict` is the destination: no valid signature, no actor. `permissive` warns
// and lets the call through, and it exists because flipping this on in one step
// would break every caller that has not been taught to sign yet — including the
// contract suite, which raises a core with no edge in front of it. `off` is for
// tests that are about something else entirely.
//
// The DEFAULT is permissive and the DEPLOYMENT is strict: the code stays usable
// for whoever clones it, and the cluster we run gets the guarantee.
type callAuthMode string

const (
	authOff        callAuthMode = "off"
	authPermissive callAuthMode = "permissive"
	authStrict     callAuthMode = "strict"
)

// callAuth verifies what arrives, and it is the only thing that may fill in the
// actor.
type callAuth struct {
	mode   callAuthMode
	keys   map[string][]byte // caller → key
	tokens ports.IdentityProvider
	users  subjectResolver
	clock  ports.Clock

	mu    sync.Mutex
	cache map[string]cachedUser
}

// subjectResolver turns a token's subject into the core's user. It is an
// interface and not the identity service because this is the ONLY thing the
// interceptor needs from it, and a narrow port is what keeps the interceptor
// from growing a domain.
type subjectResolver interface {
	UserBySubject(ctx context.Context, subject string) (*identity.User, error)
}

type cachedUser struct {
	id string
	at time.Time
}

// subjectCacheTTL bounds a lookup that would otherwise hit the database on
// EVERY call. Short on purpose: what it caches is "this subject is this user",
// which changes only when a user is created, and a stale miss costs one query.
const subjectCacheTTL = 5 * time.Minute

func newCallAuth(mode string, keys map[string][]byte, tokens ports.IdentityProvider,
	users subjectResolver, clock ports.Clock) *callAuth {
	m := callAuthMode(strings.ToLower(strings.TrimSpace(mode)))
	switch m {
	case authOff, authPermissive, authStrict:
	default:
		m = authPermissive
	}
	return &callAuth{mode: m, keys: keys, tokens: tokens, users: users, clock: clock,
		cache: map[string]cachedUser{}}
}

// authenticate resolves the call from what was PROVEN, and returns what the
// metadata alone claimed only when the mode allows it.
//
// The order matters: the assertion first, because it carries the account and the
// caller; then the token, because it is the stronger proof of WHO and overrides
// the actor the assertion asserted.
func (a *callAuth) authenticate(ctx context.Context, md metadata.MD, claimed ctxutil.Call) ctxutil.Call {
	if a == nil || a.mode == authOff {
		return claimed
	}
	log := logging.From(ctx)
	proven := ctxutil.Call{RequestID: claimed.RequestID}
	var signed bool

	if raw := first(md, "x-dop-assertion"); raw != "" {
		caller, key := a.callerKey(raw)
		if key != nil {
			if as, err := callauth.Verify(key, raw, a.clock.Now()); err == nil {
				signed = true
				proven.Caller = as.Caller
				proven.ActorID = as.ActorID
				proven.ActorKind = ctxutil.ActorKind(as.ActorKind)
				proven.AccountID = as.AccountID
				proven.SessionID = as.SessionID
				proven.ActorName = claimed.ActorName
			} else {
				log.Warn("assertion refused", "caller", caller)
			}
		}
	}

	var verified *ctxutil.VerifiedIdentity
	if tok := bearer(md); tok != "" && a.tokens != nil {
		p, err := a.tokens.VerifyToken(ctx, tok)
		if err != nil {
			log.Warn("token refused")
		} else {
			// Kept regardless of what follows: the token proved the PERSON, and
			// the bootstrap that creates their user runs precisely when the
			// lookup below cannot succeed.
			verified = &ctxutil.VerifiedIdentity{
				Subject: p.Subject, Email: p.Email, EmailVerified: p.EmailVerified,
				Name: p.Name, AvatarURL: p.AvatarURL, Providers: p.Providers,
			}
			if userID, err := a.userOf(ctx, p.Subject); err != nil {
				log.Warn("token verified but the subject has no user", "error", err.Error())
			} else {
				// The token is the stronger proof of WHO: it overrides the actor the
				// assertion asserted. A disagreement between the two is not a
				// preference — it is a bug or an attack, and it is refused below.
				if signed && proven.ActorID != "" && proven.ActorID != userID {
					log.Warn("assertion and token disagree about the actor")
					return a.refuse(claimed)
				}
				signed = true
				proven.ActorID = userID
				proven.ActorKind = ctxutil.ActorUser
				if proven.AccountID == "" {
					proven.AccountID = claimed.AccountID
				}
				if proven.SessionID == "" {
					proven.SessionID = claimed.SessionID
				}
				if proven.ActorName == "" {
					proven.ActorName = claimed.ActorName
				}
			}
		}
	}

	if !signed {
		r := a.refuse(claimed)
		r.Verified = verified
		return r
	}
	if proven.ActorKind == "" {
		proven.ActorKind = ctxutil.ActorUser
	}
	proven.Verified = verified
	return proven
}

// refuse is what happens with nothing proven: in strict, the call goes on with
// NO actor — and every use case that needs one fails where it should, at the
// authorization, instead of here where the error would say nothing useful.
func (a *callAuth) refuse(claimed ctxutil.Call) ctxutil.Call {
	if a.mode == authStrict {
		return ctxutil.Call{RequestID: claimed.RequestID}
	}
	return claimed
}

func (a *callAuth) callerKey(raw string) (string, []byte) {
	// The caller's name is the first field of the payload, and reading it
	// before verifying is safe: it only chooses WHICH key to check against.
	if as, err := callauth.Verify(a.anyKey(), raw, a.clock.Now()); err == nil {
		return as.Caller, a.keys[as.Caller]
	}
	for caller, key := range a.keys {
		if as, err := callauth.Verify(key, raw, a.clock.Now()); err == nil {
			return as.Caller, key
		}
		_ = caller
	}
	return "", a.anyKey()
}

func (a *callAuth) anyKey() []byte {
	for _, k := range a.keys {
		return k
	}
	return nil
}

func (a *callAuth) userOf(ctx context.Context, subject string) (string, error) {
	if a.users == nil {
		return "", errNoResolver
	}
	now := a.clock.Now()
	a.mu.Lock()
	if c, ok := a.cache[subject]; ok && now.Sub(c.at) < subjectCacheTTL {
		a.mu.Unlock()
		return c.id, nil
	}
	a.mu.Unlock()

	u, err := a.users.UserBySubject(ctx, subject)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", errUnknownSubject
	}
	a.mu.Lock()
	a.cache[subject] = cachedUser{id: u.ID, at: now}
	a.mu.Unlock()
	return u.ID, nil
}

func bearer(md metadata.MD) string {
	v := first(md, "authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}
