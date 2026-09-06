// Package identity is the domain of who the user is, what an account is and how
// one gets into it.
//
// House rule: this package knows nothing of Postgres, gRPC or any SDK. It
// declares what it needs as a PORT (repository.go) and the composition root
// wires it.
package identity

import (
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

type AccountKind string

const (
	AccountPersonal     AccountKind = "personal"
	AccountOrganization AccountKind = "organization"
)

type Role string

const (
	RoleOwner     Role = "owner"
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
	RoleViewer    Role = "viewer"
)

// ValidRole accepts only the four predefined roles. The role is a field on the
// membership: adding one later needs no migration (spec SP-0 §4).
func ValidRole(r Role) bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer:
		return true
	}
	return false
}

// CanManageMembers answers who may invite and change memberships.
func (r Role) CanManageMembers() bool { return r == RoleOwner || r == RoleAdmin }

// HasImplicitManage: owner and admin manage every resource in the account —
// without it you get the scenario where nobody can fix a broken integration.
func (r Role) HasImplicitManage() bool { return r == RoleOwner || r == RoleAdmin }

type User struct {
	ID            string
	Subject       string // from the IdentityProvider, already normalized
	Email         string
	EmailVerified bool
	Name          string
	AvatarURL     string
	Providers     []string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Account struct {
	ID             string
	Kind           AccountKind
	Handle         string
	DisplayName    string
	LegalID        string // company registration number (CNPJ in Brazil)
	LegalName      string
	VerifiedDomain string // empty = not verified
	// DefaultRevocationPolicy is stamped onto a grant when a flow is shared FROM
	// this account (flow sharing spec §3.2). It is a plain string, not
	// workflow.RevocationPolicy: identity does not know the sharing domain, the
	// same way it does not know Postgres — the vocabulary is validated at the
	// service boundary, not by the type.
	DefaultRevocationPolicy string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// IsVerified unlocks domain-based joining, the badge and handle disputes
// (ADR-0004). Everything else works without verification.
func (a Account) IsVerified() bool { return a.VerifiedDomain != "" }

type Membership struct {
	ID        string
	UserID    string
	AccountID string
	Role      Role
	CreatedAt time.Time
	UpdatedAt time.Time
}

type InviteStatus string

const (
	InvitePending  InviteStatus = "pending"
	InviteAccepted InviteStatus = "accepted"
	InviteExpired  InviteStatus = "expired"
	InviteRevoked  InviteStatus = "revoked"
)

// GrantSpec is the grant composed INTO THE INVITE — there is no default
// (ADR-0013).
type GrantSpec struct {
	ResourceID string
	Level      string // use | manage
}

type Invite struct {
	ID        string
	AccountID string
	Email     string
	Role      Role
	Grants    []GrantSpec
	Status    InviteStatus
	InvitedBy string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// InviteTTL: 14 days (spec SP-0 §3).
const InviteTTL = 14 * 24 * time.Hour

// IsUsable says whether the invite can still be accepted. Expiry is checked by
// TIME, not only by status — the status may not have been swept yet.
func (i Invite) IsUsable(now time.Time) bool {
	return i.Status == InvitePending && now.Before(i.ExpiresAt)
}

// ── name rules ───────────────────────────────────────────────────────────────

// NormalizeHandle produces a valid handle from free text.
// Personal and organization accounts share one namespace (ADR-0002).
func NormalizeHandle(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if at := strings.IndexByte(raw, '@'); at > 0 {
		raw = raw[:at] // handle derived from the email
	}
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

const (
	handleMinLen = 2
	handleMaxLen = 39
)

// Translation keys for the refusals a person reads while choosing a handle.
const (
	KeyHandleTooShort = "identity.handle.too_short"
	KeyHandleTooLong  = "identity.handle.too_long"
	KeyHandleCharset  = "identity.handle.charset"
)

func ValidateHandle(h string) error {
	if len(h) < handleMinLen {
		return errs.Invalid("handle needs at least %d characters", handleMinLen).
			WithCode(KeyHandleTooShort, map[string]any{"min": handleMinLen})
	}
	if len(h) > handleMaxLen {
		return errs.Invalid("handle may have at most %d characters", handleMaxLen).
			WithCode(KeyHandleTooLong, map[string]any{"max": handleMaxLen})
	}
	if NormalizeHandle(h) != h {
		return errs.Invalid("handle accepts only lowercase letters, digits and hyphens").
			WithCode(KeyHandleCharset, nil)
	}
	return nil
}
