// Package identity é o domínio de quem é o usuário, o que é uma conta e como
// se entra nela.
//
// Regra da casa: este pacote não conhece Postgres, gRPC nem SDK nenhum. Ele
// declara o que precisa como PORTA (repository.go) e o composition root liga.
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

// ValidRole aceita apenas os quatro papéis pré-definidos. Papel é campo na
// membership: acrescentar depois não exige migração (spec SP-0 §4).
func ValidRole(r Role) bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer:
		return true
	}
	return false
}

// CanManageMembers responde quem pode convidar e alterar vínculos.
func (r Role) CanManageMembers() bool { return r == RoleOwner || r == RoleAdmin }

// HasImplicitManage: owner e admin gerenciam todo recurso da conta — sem isso
// surge o cenário em que ninguém consegue consertar uma integração quebrada.
func (r Role) HasImplicitManage() bool { return r == RoleOwner || r == RoleAdmin }

type User struct {
	ID            string
	Subject       string // do IdentityProvider, já normalizado
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
	LegalID        string // CNPJ
	LegalName      string
	VerifiedDomain string // vazio = não verificada
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// IsVerified destrava entrada por domínio, selo e contestação de handle
// (ADR-0004). Tudo o mais funciona sem verificação.
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

// GrantSpec é a concessão composta NO CONVITE — não há default (ADR-0013).
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

// InviteTTL: 14 dias (spec SP-0 §3).
const InviteTTL = 14 * 24 * time.Hour

// IsUsable diz se o convite ainda pode ser aceito. Expiração é verificada por
// tempo, não só por status — o status pode não ter sido varrido ainda.
func (i Invite) IsUsable(now time.Time) bool {
	return i.Status == InvitePending && now.Before(i.ExpiresAt)
}

// ── regras de nome ───────────────────────────────────────────────────────────

// NormalizeHandle produz um handle válido a partir de um texto livre.
// PF e PJ dividem o mesmo espaço de nomes (ADR-0002).
func NormalizeHandle(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if at := strings.IndexByte(raw, '@'); at > 0 {
		raw = raw[:at] // handle derivado do e-mail
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

func ValidateHandle(h string) error {
	if len(h) < handleMinLen {
		return errs.Invalid("handle precisa de ao menos %d caracteres", handleMinLen)
	}
	if len(h) > handleMaxLen {
		return errs.Invalid("handle pode ter no máximo %d caracteres", handleMaxLen)
	}
	if NormalizeHandle(h) != h {
		return errs.Invalid("handle aceita apenas letras minúsculas, números e hífen")
	}
	return nil
}
