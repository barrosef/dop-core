// Package hierarchy is the domain of the cockpit's tree: workspaces and the
// projects that live inside them.
//
// House rule: this package knows nothing of Postgres, gRPC or any SDK. It
// declares what it needs as a PORT (repository.go) and the composition root
// wires it.
package hierarchy

import (
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Workspace groups projects and belongs to ONE account. The account is the
// isolation boundary: nothing crosses from one account to another (ADR-0017).
type Workspace struct {
	ID          string
	AccountID   string
	Name        string
	Key         string // optional — a short prefix that labels the workspace in the UI
	Description string
	Tags        []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Project is the unit of work. It does NOT declare its account: that is
// inherited from the workspace. Letting the client choose a project's account
// would open exactly the hole multi-tenant isolation exists to close.
type Project struct {
	ID          string
	AccountID   string // inherited from the workspace, never supplied by the caller
	WorkspaceID string
	Name        string
	Description string
	Repos       []ProjectRepo
	TaskManager *ProjectTaskManager // optional: not every project has a board
	Resources   []string            // ids of attached resources (skill, workflow, git_flow)
	Rules       []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ProjectRepo is a repository attached to the project.
//
// The provider belongs to the REPOSITORY, not to the project (ADR-0013): that
// is why IntegrationID lives here and not on Project. Without that choice, a
// project with one repo on GitHub and another on GitLab would be impossible to
// represent.
type ProjectRepo struct {
	ID            string
	IntegrationID string
	ExternalID    string
	Name          string
	DefaultBranch string
	PRTargets     []string
}

// DefaultBranch applied when the caller does not say — mirrors the column's
// default, so that the domain and the database do not disagree.
const DefaultBranch = "main"

// ProjectTaskManager is the link to the provider's board.
//
// ExternalSpaceID holds the provider's "space" — never the bare word workspace:
// workspace is already OUR concept, and confusing the two is the short path to
// linking the project to the wrong board.
type ProjectTaskManager struct {
	IntegrationID     string
	ExternalSpaceID   string
	ExternalProjectID string
	CardTypes         []string // dynamic: the provider owns this vocabulary
}

// TreeNode is a workspace together with the projects it contains. It exists so
// the cockpit's tree arrives in ONE response — see Repository.Tree.
type TreeNode struct {
	Workspace Workspace
	Projects  []Project
}

// ReferencedResourceIDs lists EVERY resource the project cites: the
// repositories' integrations, the task manager's integration and the attached
// resources.
//
// It is the input to the account check — which is why it returns everything
// together, without distinguishing the origin: any id that escapes the owning
// account is a problem.
func (p Project) ReferencedResourceIDs() []string {
	seen := make(map[string]bool, len(p.Repos)+len(p.Resources)+1)
	out := make([]string, 0, len(p.Repos)+len(p.Resources)+1)
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, r := range p.Repos {
		add(r.IntegrationID)
	}
	if p.TaskManager != nil {
		add(p.TaskManager.IntegrationID)
	}
	for _, id := range p.Resources {
		add(id)
	}
	return out
}

// ── name and key rules ───────────────────────────────────────────────────────

const (
	nameMaxLen = 120
	keyMinLen  = 2
	keyMaxLen  = 12
)

// Translation keys for the refusals a person reads while filling in a form.
const (
	KeyNameRequired = "hierarchy.name.required"
	KeyNameTooLong  = "hierarchy.name.too_long"
	KeyKeyTooShort  = "hierarchy.key.too_short"
	KeyKeyTooLong   = "hierarchy.key.too_long"
	KeyKeyCharset   = "hierarchy.key.charset"
)

// ValidateName applies to workspaces and projects alike: a name is required.
// With no name, the cockpit's tree becomes a list of blank items.
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errs.Invalid("name is required").
			WithCode(KeyNameRequired, nil)
	}
	if len(name) > nameMaxLen {
		return errs.Invalid("name may have at most %d characters", nameMaxLen).
			WithCode(KeyNameTooLong, map[string]any{"max": nameMaxLen})
	}
	return nil
}

// NormalizeKey produces a valid key from free text: uppercase and alphanumeric.
func NormalizeKey(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidateKey accepts an EMPTY key — it is optional. When present it has to be
// short and uppercase alphanumeric: the key shows up as a prefix in labels and
// references, and a long or punctuated prefix disappears off the screen.
func ValidateKey(k string) error {
	if k == "" {
		return nil
	}
	if len(k) < keyMinLen {
		return errs.Invalid("the key needs at least %d characters", keyMinLen).
			WithCode(KeyKeyTooShort, map[string]any{"min": keyMinLen})
	}
	if len(k) > keyMaxLen {
		return errs.Invalid("the key may have at most %d characters", keyMaxLen).
			WithCode(KeyKeyTooLong, map[string]any{"max": keyMaxLen})
	}
	if NormalizeKey(k) != k {
		return errs.Invalid("the key accepts only uppercase letters and digits").
			WithCode(KeyKeyCharset, nil)
	}
	return nil
}
