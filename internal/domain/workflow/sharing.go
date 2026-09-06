// This file opens the SHARING vocabulary for the workflow domain (flow sharing
// spec §3.2): what happens to a derivation that already exists when the flow it
// was made from gets revoked.
package workflow

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// RevocationPolicy is what a revocation DOES to derivations that already exist.
//
// The three names are borrowed, not invented: `prospective` is the legal and
// API-deprecation sense of "applies going forward" (it is crates.io's `yank`);
// `drain` is the load balancer's and `kubectl drain`'s "stop taking new work,
// let current work end"; `terminate` is k8s/systemd's "end now".
type RevocationPolicy string

const (
	// PolicyProspective revokes the GRANT only: no new derivation is possible
	// and every existing one goes on working, untouched.
	PolicyProspective RevocationPolicy = "prospective"
	// PolicyDrain revokes the derivations, but a demand already running finishes
	// under the version it froze on start (ADR-0014 §4).
	PolicyDrain RevocationPolicy = "drain"
	// PolicyTerminate revokes at once: a running demand stops at its current gate
	// and raises an attention item.
	PolicyTerminate RevocationPolicy = "terminate"
)

// DefaultRevocationPolicy is the permissive one, and deliberately so: the
// destructive behaviour has to be chosen, never inherited by omission.
const DefaultRevocationPolicy = PolicyProspective

// ValidRevocationPolicy accepts only the three predefined policies. An unknown
// value is a contract error, not user data — whoever sends it is speaking a
// vocabulary this platform does not have.
func ValidRevocationPolicy(p RevocationPolicy) bool {
	switch p {
	case PolicyProspective, PolicyDrain, PolicyTerminate:
		return true
	}
	return false
}

// PublicationRef addresses a published flow: `@handle/slug` for the latest
// published version, `@handle/slug@v3` to pin one.
//
// It is typed and read by people, which is why the refusals below name what is
// wrong instead of returning a generic parse error — and why the vocabulary is
// lowercase: two references differing only in case would look identical in a
// chat message and resolve to different flows.
type PublicationRef struct {
	Handle  string
	Slug    string
	Version int32 // 0 = the latest published
}

func (r PublicationRef) Pinned() bool { return r.Version > 0 }

func (r PublicationRef) String() string {
	s := "@" + r.Handle + "/" + r.Slug
	if r.Pinned() {
		s += "@v" + strconv.Itoa(int(r.Version))
	}
	return s
}

// refPart is the shape both a handle and a slug have to fit: lowercase letters,
// digits and hyphens, starting and ending with an alphanumeric.
var refPart = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func ParseRef(raw string) (PublicationRef, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "@") {
		return PublicationRef{}, errs.Invalid("a flow reference starts with @: %q", raw)
	}
	s = s[1:]

	var version int32
	if at := strings.LastIndex(s, "@"); at >= 0 {
		v := s[at+1:]
		if !strings.HasPrefix(v, "v") {
			return PublicationRef{}, errs.Invalid("the version in a reference is written @v3, not @%s", v)
		}
		n, err := strconv.Atoi(v[1:])
		if err != nil || n <= 0 {
			return PublicationRef{}, errs.Invalid("%q is not a version number", v)
		}
		version = int32(n)
		s = s[:at]
	}

	handle, slug, found := strings.Cut(s, "/")
	if !found {
		return PublicationRef{}, errs.Invalid("a reference is @handle/name: %q", raw)
	}
	if !refPart.MatchString(handle) {
		return PublicationRef{}, errs.Invalid("%q is not a usable handle: lowercase letters, digits and hyphens", handle)
	}
	if !refPart.MatchString(slug) {
		return PublicationRef{}, errs.Invalid("%q is not a usable name: lowercase letters, digits and hyphens", slug)
	}
	return PublicationRef{Handle: handle, Slug: slug, Version: version}, nil
}
