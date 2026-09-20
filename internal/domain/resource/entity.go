// Package resource is the domain of an account's resources: integrations,
// skills, workflows and git flows — the unit of ownership and sharing
// (ADR-0009).
//
// House rule: this package knows nothing of Postgres, gRPC or any SDK. It
// declares what it needs as a PORT (repository.go) and the composition root
// wires it.
//
// The distinction that organizes everything here is the resource's NATURE: a
// resource with a credential carries risk; a content resource carries
// knowledge. The two cannot share an access default (ADR-0010 §6).
package resource

import (
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

type Kind string

const (
	KindIntegration Kind = "integration"
	KindSkill       Kind = "skill"
	KindWorkflow    Kind = "workflow"
	KindGitFlow     Kind = "git_flow"
)

// ValidKind accepts only the four types of the database's resource_kind enum —
// a type outside the vocabulary is a contract error, not user data.
func ValidKind(k Kind) bool {
	switch k {
	case KindIntegration, KindSkill, KindWorkflow, KindGitFlow:
		return true
	}
	return false
}

// HasCredential: only an integration has a credential. It is the predicate that
// decides the access default, the versioning and whether SetCredential makes
// sense at all — three different rules hanging off the SAME distinction, which
// is a sign the distinction is the right one.
func (k Kind) HasCredential() bool { return k == KindIntegration }

// IsContent is the other side: skill, workflow and git_flow are written
// knowledge, versionable and shareable within the account.
func (k Kind) IsContent() bool { return ValidKind(k) && !k.HasCredential() }

// Category classifies an integration. There is no category outside these three:
// git (where the code lives), task_manager (where the demand comes from) and
// agent (who executes — Claude, Codex, Google Code Assist).
type Category string

const (
	CategoryGit         Category = "git"
	CategoryTaskManager Category = "task_manager"
	CategoryAgent       Category = "agent"
)

func ValidCategory(c Category) bool {
	switch c {
	case CategoryGit, CategoryTaskManager, CategoryAgent:
		return true
	}
	return false
}

// Level is a grant's level. There are two, on purpose: more levels turn into a
// permission matrix nobody can explain.
type Level string

const (
	LevelNone   Level = "" // absence of access — never stored, only returned
	LevelUse    Level = "use"
	LevelManage Level = "manage"
)

func ValidLevel(l Level) bool { return l == LevelUse || l == LevelManage }

// AtLeast: manage includes use. Whoever manages the integration also uses it —
// the converse does not hold.
func (l Level) AtLeast(want Level) bool {
	switch want {
	case LevelUse:
		return l == LevelUse || l == LevelManage
	case LevelManage:
		return l == LevelManage
	}
	return false
}

type Resource struct {
	ID            string
	AccountID     string
	Kind          Kind
	Name          string
	Version       int32
	Config        map[string]any
	CredentialRef string // an OPAQUE pointer into the SecretStore — never the secret
	Status        string
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (r Resource) HasCredential() bool { return r.Kind.HasCredential() }

// IsVersioned: content is versioned, a credential is not.
//
// A skill and a workflow are text somebody wrote and somebody else will run
// tomorrow — overwriting erases the answer to "which version did that run
// with?". An integration's configuration, by contrast, is the current state of
// the outside world: versioning yesterday's base_url serves nobody.
func (r Resource) IsVersioned() bool { return r.Kind.IsContent() }

type Grant struct {
	ID         string
	ResourceID string
	UserID     string
	Level      Level
	GrantedBy  string
	CreatedAt  time.Time
}

// ── credential ───────────────────────────────────────────────────────────────

// CredentialKind is the Kind of an integration credential's SecretRef. Fixed:
// the domain does not invent vault taxonomy.
const CredentialKind = "integration_credential"

// SecretRefFor builds the secret's LOGICAL reference. The domain knows no path,
// no namespace and no secret name — only the adapter can resolve it (ADR-0001).
func SecretRefFor(accountID, resourceID string) ports.SecretRef {
	return ports.SecretRef{
		AccountID: accountID,
		Kind:      CredentialKind,
		OwnerID:   resourceID,
	}
}

// CredentialRef is what is STORED on the resource's row: an opaque label,
// derivable from the row itself. It is not a vault path and it opens nothing —
// it exists to answer "does this integration already have a credential?" and to
// let the audit trail record WHICH credential authorized an action, without ever
// touching the value.
func CredentialRef(accountID, resourceID string) string {
	return CredentialKind + ":" + accountID + ":" + resourceID
}

// ── the heart: effective access ──────────────────────────────────────────────

// EffectiveLevel resolves the actor's level over a resource. It is the system's
// only function that answers "may this person?" — and that is why it lives here,
// in the domain, testable without a database.
//
// The order of the clauses IS the rule:
//
//  1. owner and admin have IMPLICIT manage over every resource in the account.
//     Without it you get the scenario where the GitHub integration breaks, the
//     person who connected it has left the company, and nobody — not even the
//     account's owner — can fix it.
//  2. an explicit grant comes next, for any nature of resource.
//  3. with no grant, the NATURE decides (ADR-0010 §6): a credential is risk,
//     knowledge is knowledge.
func EffectiveLevel(r Resource, role identity.Role, accountKind identity.AccountKind, explicit *Grant) Level {
	if role.HasImplicitManage() {
		return LevelManage
	}
	if explicit != nil && ValidLevel(explicit.Level) {
		return explicit.Level
	}
	if r.HasCredential() {
		// CLOSED by nature. A credential is the key to a third-party system:
		// whoever was not explicitly handed the key does not have it.
		return LevelNone
	}
	if accountKind == identity.AccountOrganization {
		// OPEN by nature, within the account. Skills and workflows exist to be
		// used by the team; requiring a grant for each member to read a skill
		// turns knowledge into bureaucracy. It stays restrictable through an
		// explicit grant, which clause 2 already honours.
		return LevelUse
	}
	// A personal account has exactly one member, and that member is owner — so
	// clause 1 already answered. Reaching here means a membership that does not
	// exist. That is where the rule "a personal account's resource is never
	// shareable" comes from, for free: there is nobody to share it with.
	return LevelNone
}

// ── input validation ─────────────────────────────────────────────────────────

const nameMaxLen = 120

// Translation keys for the refusals a person reads.
const (
	KeyNameRequired     = "resource.name.required"
	KeyNameTooLong      = "resource.name.too_long"
	KeyCategoryInvalid  = "resource.integration.category_invalid"
	KeyProviderRequired = "resource.integration.provider_required"

	KeyResourceMissing       = "resource.id_missing"
	KeyNoGrant               = "resource.grant.absent"
	KeyKindUnknown           = "resource.kind.unknown"
	KeyViewerCannotCreate    = "resource.create.viewer_forbidden"
	KeyLevelInvalid          = "resource.grant.level_invalid"
	KeyGrantUserMissing      = "resource.grant.user_missing"
	KeyGranteeNotMember      = "resource.grant.not_a_member"
	KeyGrantMissing          = "resource.grant.id_missing"
	KeyOnlyManagersSeeGrants = "resource.grant.only_managers_read"
	KeyCredentialEmpty       = "resource.credential.empty"
	KeyCredentialMissing     = "resource.credential.missing"
	KeyKindHasNoCredential   = "resource.credential.kind_unsupported"
)

// ValidateName: the name is the resource's natural key within the (account,
// kind) pair, so it has to be stable and readable.
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errs.Invalid("the resource needs a name").WithCode(KeyNameRequired, nil)
	}
	if len(name) > nameMaxLen {
		return errs.Invalid("the resource name may have at most %d characters", nameMaxLen).
			WithCode(KeyNameTooLong, map[string]any{"max": nameMaxLen})
	}
	return nil
}

// IntegrationSpec is the typed reading of an integration's config — what the
// proto describes as IntegrationSpec and the database keeps in jsonb.
type IntegrationSpec struct {
	Category Category
	Provider string
	BaseURL  string
}

// ParseIntegration validates an integration's config at write time.
//
// An integration with no category and no provider is a useless row: nobody knows
// whether that is a GitHub, a Jira or an agent, and execution routing depends on
// exactly that. Validating on write avoids finding out at deploy time.
func ParseIntegration(config map[string]any) (IntegrationSpec, error) {
	var spec IntegrationSpec
	spec.Category = Category(strings.TrimSpace(str(config["category"])))
	spec.Provider = strings.TrimSpace(str(config["provider"]))
	spec.BaseURL = strings.TrimSpace(str(config["base_url"]))

	if !ValidCategory(spec.Category) {
		return spec, errs.Invalid(
			"invalid integration category: %q (use git, task_manager or agent)", spec.Category).
			WithCode(KeyCategoryInvalid, map[string]any{"category": string(spec.Category)})
	}
	if spec.Provider == "" {
		return spec, errs.Invalid("the integration has to declare its provider").
			WithCode(KeyProviderRequired, nil)
	}
	return spec, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
