package hierarchy

import "context"

// Repository is the hierarchy domain's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
//
// Every operation takes accountID explicitly: filtering by the active account is
// a parameter of the port, not a detail the adapter could forget.
type Repository interface {
	// Workspaces
	ListWorkspaces(ctx context.Context, accountID string) ([]Workspace, error)
	WorkspaceByID(ctx context.Context, accountID, id string) (*Workspace, error)
	CreateWorkspace(ctx context.Context, w *Workspace) (*Workspace, error)
	UpdateWorkspace(ctx context.Context, w *Workspace) (*Workspace, error)

	// Projects. An empty workspaceID means all of the account's projects.
	ListProjects(ctx context.Context, accountID, workspaceID string) ([]Project, error)
	ProjectByID(ctx context.Context, accountID, id string) (*Project, error)
	CreateProject(ctx context.Context, p *Project) (*Project, error)
	UpdateProject(ctx context.Context, p *Project) (*Project, error)

	// Tree returns the account's workspaces and projects already grouped.
	//
	// It exists as an operation of its OWN — rather than a loop over
	// ListProjects — so the cockpit's tree costs one scan instead of one query
	// per workspace. The N+1 here would show up on everybody's first screen.
	Tree(ctx context.Context, accountID string) ([]TreeNode, error)

	// ResourceAccounts says which account each requested resource belongs to.
	// Ids that do not exist simply do not come back in the map.
	//
	// It is what lets the domain refuse another account's resource without
	// knowing what a resource is — the resource domain knows that.
	ResourceAccounts(ctx context.Context, ids []string) (map[string]string, error)
}
