package hierarchy

import (
	"context"
	"strings"

	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Service concentrates the hierarchy rules. It takes only PORTS.
type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service { return &Service{repo: repo} }

// Translation keys for the refusals a person reads.
const (
	KeyWorkspaceIDMissing = "hierarchy.workspace.id_missing"
	KeyProjectIDMissing   = "hierarchy.project.id_missing"
	KeyProjectNeedsWS     = "hierarchy.project.workspace_required"
	KeyResourceUnknown    = "hierarchy.resource.unknown"
	KeyResourceOtherAcct  = "hierarchy.resource.other_account"
	KeyRepoNoIntegration  = "hierarchy.repo.integration_missing"
	KeyRepoNoExternalID   = "hierarchy.repo.external_id_missing"
)

// ── workspaces ───────────────────────────────────────────────────────────────

func (s *Service) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListWorkspaces(ctx, accountID)
}

// GetWorkspace ALWAYS looks inside the active account. Another account's
// workspace does not return "forbidden" but "not found" — someone outside the
// account should not even discover that the id exists.
func (s *Service) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errs.Invalid("workspace id not provided").WithCode(KeyWorkspaceIDMissing, nil)
	}
	return s.repo.WorkspaceByID(ctx, accountID, id)
}

func (s *Service) CreateWorkspace(ctx context.Context, name, key, description string, tags []string) (*Workspace, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	return s.repo.CreateWorkspace(ctx, &Workspace{
		AccountID:   accountID,
		Name:        name,
		Key:         key,
		Description: strings.TrimSpace(description),
		Tags:        tags,
	})
}

// UpdateWorkspace ignores whatever account arrives in the argument and uses the
// context's: a workspace's account is not changed by an update.
func (s *Service) UpdateWorkspace(ctx context.Context, in Workspace) (*Workspace, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if in.ID == "" {
		return nil, errs.Invalid("workspace id not provided").WithCode(KeyWorkspaceIDMissing, nil)
	}
	name := strings.TrimSpace(in.Name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.Key)
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	// Confirm the workspace belongs to the active account BEFORE writing.
	if _, err := s.repo.WorkspaceByID(ctx, accountID, in.ID); err != nil {
		return nil, err
	}
	return s.repo.UpdateWorkspace(ctx, &Workspace{
		ID:          in.ID,
		AccountID:   accountID,
		Name:        name,
		Key:         key,
		Description: strings.TrimSpace(in.Description),
		Tags:        in.Tags,
	})
}

// ── projects ─────────────────────────────────────────────────────────────────

func (s *Service) ListProjects(ctx context.Context, workspaceID string) ([]Project, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListProjects(ctx, accountID, workspaceID)
}

func (s *Service) GetProject(ctx context.Context, id string) (*Project, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errs.Invalid("project id not provided").WithCode(KeyProjectIDMissing, nil)
	}
	return s.repo.ProjectByID(ctx, accountID, id)
}

// CreateProject binds the project to the workspace and INHERITS the account
// from it. It is the only path by which a project gains an account.
func (s *Service) CreateProject(ctx context.Context, workspaceID, name, description string) (*Project, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if workspaceID == "" {
		return nil, errs.Invalid("a project needs a workspace").WithCode(KeyProjectNeedsWS, nil)
	}
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	ws, err := s.repo.WorkspaceByID(ctx, accountID, workspaceID)
	if err != nil {
		return nil, err
	}
	return s.repo.CreateProject(ctx, &Project{
		AccountID:   ws.AccountID,
		WorkspaceID: ws.ID,
		Name:        name,
		Description: strings.TrimSpace(description),
	})
}

// UpdateProject applies the name, the description and the attached resources.
//
// This is where the resource rule bites: everything the project references has
// to belong to the account that owns the WORKSPACE — not to whichever account
// happens to be active, not to the account of whoever sent the id.
func (s *Service) UpdateProject(ctx context.Context, in Project) (*Project, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if in.ID == "" {
		return nil, errs.Invalid("project id not provided").WithCode(KeyProjectIDMissing, nil)
	}
	name := strings.TrimSpace(in.Name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	current, err := s.repo.ProjectByID(ctx, accountID, in.ID)
	if err != nil {
		return nil, err
	}

	// Moving between workspaces is allowed — as long as the destination belongs
	// to the active account.
	workspaceID := current.WorkspaceID
	if in.WorkspaceID != "" {
		workspaceID = in.WorkspaceID
	}
	ws, err := s.repo.WorkspaceByID(ctx, accountID, workspaceID)
	if err != nil {
		return nil, err
	}

	next := Project{
		ID:          current.ID,
		AccountID:   ws.AccountID, // stays inherited, always
		WorkspaceID: ws.ID,
		Name:        name,
		Description: strings.TrimSpace(in.Description),
		Repos:       normalizeRepos(in.Repos),
		TaskManager: in.TaskManager,
		Resources:   in.Resources,
		Rules:       in.Rules,
	}
	if err := validateRepos(next.Repos); err != nil {
		return nil, err
	}
	if err := s.assertSameAccount(ctx, ws.AccountID, next.ReferencedResourceIDs()); err != nil {
		return nil, err
	}
	return s.repo.UpdateProject(ctx, &next)
}

// ── tree ─────────────────────────────────────────────────────────────────────

// GetTree returns workspaces with their projects in ONE call — it is what the
// cockpit's first screen consumes. See the comment on Repository.Tree.
func (s *Service) GetTree(ctx context.Context) ([]TreeNode, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.Tree(ctx, accountID)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// assertSameAccount is the "resources from different accounts do not mix" rule.
//
// A resource id is guessable and travels in the request body: without this
// check, sending another account's integration id would be enough to make the
// project start using their credential. It refuses as Invalid — not NotFound —
// because the request itself is wrong, not the resource missing.
func (s *Service) assertSameAccount(ctx context.Context, accountID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	owners, err := s.repo.ResourceAccounts(ctx, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		owner, ok := owners[id]
		if !ok {
			return errs.Invalid("resource %s does not exist", id).
				WithCode(KeyResourceUnknown, map[string]any{"id": id})
		}
		if owner != accountID {
			return errs.Invalid("resource %s belongs to another account", id).
				WithCode(KeyResourceOtherAcct, map[string]any{"id": id})
		}
	}
	return nil
}

func normalizeRepos(repos []ProjectRepo) []ProjectRepo {
	out := make([]ProjectRepo, 0, len(repos))
	for _, r := range repos {
		r.Name = strings.TrimSpace(r.Name)
		r.ExternalID = strings.TrimSpace(r.ExternalID)
		if strings.TrimSpace(r.DefaultBranch) == "" {
			r.DefaultBranch = DefaultBranch
		}
		out = append(out, r)
	}
	return out
}

func validateRepos(repos []ProjectRepo) error {
	for _, r := range repos {
		if r.IntegrationID == "" {
			return errs.Invalid("repository %q has no integration", r.Name).
				WithCode(KeyRepoNoIntegration, map[string]any{"repo": r.Name})
		}
		if r.ExternalID == "" {
			return errs.Invalid("repository %q has no provider identifier", r.Name).
				WithCode(KeyRepoNoExternalID, map[string]any{"repo": r.Name})
		}
	}
	return nil
}
