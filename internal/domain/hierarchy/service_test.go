package hierarchy_test

import (
	"context"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/hierarchy"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT a database: the repository is a port, and an
// in-memory double goes in here. It is the practical return on hexagonal
// architecture.

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"dop":        "DOP",
		"  dop-x  ":  "DOPX",
		"Plat form1": "PLATFORM1",
		"---":        "",
	}
	for input, want := range cases {
		if got := hierarchy.NormalizeKey(input); got != want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateKey(t *testing.T) {
	// The key is OPTIONAL: absence is not an error.
	if err := hierarchy.ValidateKey(""); err != nil {
		t.Errorf("an empty key should be accepted: %v", err)
	}
	if err := hierarchy.ValidateKey("DOP"); err != nil {
		t.Errorf("a short uppercase key was refused: %v", err)
	}
	if err := hierarchy.ValidateKey("dop"); err == nil {
		t.Error("a lowercase key should be refused")
	}
	if err := hierarchy.ValidateKey("DOP-X"); err == nil {
		t.Error("a key with punctuation should be refused")
	}
	if err := hierarchy.ValidateKey("D"); err == nil {
		t.Error("a 1-character key should be refused")
	}
	if err := hierarchy.ValidateKey("DIGITALPLATFORM"); err == nil {
		t.Error("a long key should be refused")
	}
}

// A refusal a person reads has to carry its translation key, otherwise the form
// can only ever apologize in English.
func TestValidationRefusalsCarryATranslationKey(t *testing.T) {
	for name, err := range map[string]error{
		"empty name":  hierarchy.ValidateName(""),
		"long name":   hierarchy.ValidateName(string(make([]byte, 200))),
		"short key":   hierarchy.ValidateKey("D"),
		"long key":    hierarchy.ValidateKey("DIGITALPLATFORM"),
		"key charset": hierarchy.ValidateKey("dop"),
	} {
		if err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
		if code, _ := errs.CodeOf(err); code == "" {
			t.Errorf("%s: refusal has no translation key — %v", name, err)
		}
	}
}

func TestReferencedResourceIDsJoinsEverythingWithoutRepeating(t *testing.T) {
	p := hierarchy.Project{
		Repos: []hierarchy.ProjectRepo{
			{IntegrationID: "int-1"}, {IntegrationID: "int-1"}, {IntegrationID: "int-2"},
		},
		TaskManager: &hierarchy.ProjectTaskManager{IntegrationID: "int-3"},
		Resources:   []string{"res-1", "int-2"},
	}
	got := p.ReferencedResourceIDs()
	if len(got) != 4 {
		t.Fatalf("expected 4 distinct ids, got %v", got)
	}
}

// ── in-memory double ────────────────────────────────────────────────────────

type fakeRepo struct {
	workspaces map[string]*hierarchy.Workspace
	projects   map[string]*hierarchy.Project
	// the owner of each resource, by id — this is what backs the account rule.
	resources map[string]string
	nextID    int
	treeCalls int
	listCalls int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		workspaces: map[string]*hierarchy.Workspace{},
		projects:   map[string]*hierarchy.Project{},
		resources:  map[string]string{},
	}
}

func (f *fakeRepo) id(prefix string) string {
	f.nextID++
	return prefix + "-" + string(rune('a'+f.nextID))
}

func (f *fakeRepo) ListWorkspaces(_ context.Context, accountID string) ([]hierarchy.Workspace, error) {
	var out []hierarchy.Workspace
	for _, w := range f.workspaces {
		if w.AccountID == accountID {
			out = append(out, *w)
		}
	}
	return out, nil
}

func (f *fakeRepo) WorkspaceByID(_ context.Context, accountID, id string) (*hierarchy.Workspace, error) {
	w, ok := f.workspaces[id]
	if !ok || w.AccountID != accountID {
		return nil, errs.NotFound("workspace")
	}
	cp := *w
	return &cp, nil
}

func (f *fakeRepo) CreateWorkspace(_ context.Context, w *hierarchy.Workspace) (*hierarchy.Workspace, error) {
	w.ID = f.id("ws")
	cp := *w
	f.workspaces[w.ID] = &cp
	return &cp, nil
}

func (f *fakeRepo) UpdateWorkspace(_ context.Context, w *hierarchy.Workspace) (*hierarchy.Workspace, error) {
	cur, ok := f.workspaces[w.ID]
	if !ok || cur.AccountID != w.AccountID {
		return nil, errs.NotFound("workspace")
	}
	cp := *w
	f.workspaces[w.ID] = &cp
	return &cp, nil
}

func (f *fakeRepo) ListProjects(_ context.Context, accountID, workspaceID string) ([]hierarchy.Project, error) {
	f.listCalls++
	var out []hierarchy.Project
	for _, p := range f.projects {
		if p.AccountID != accountID {
			continue
		}
		if workspaceID != "" && p.WorkspaceID != workspaceID {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (f *fakeRepo) ProjectByID(_ context.Context, accountID, id string) (*hierarchy.Project, error) {
	p, ok := f.projects[id]
	if !ok || p.AccountID != accountID {
		return nil, errs.NotFound("project")
	}
	cp := *p
	return &cp, nil
}

func (f *fakeRepo) CreateProject(_ context.Context, p *hierarchy.Project) (*hierarchy.Project, error) {
	p.ID = f.id("prj")
	cp := *p
	f.projects[p.ID] = &cp
	return &cp, nil
}

func (f *fakeRepo) UpdateProject(_ context.Context, p *hierarchy.Project) (*hierarchy.Project, error) {
	cur, ok := f.projects[p.ID]
	if !ok || cur.AccountID != p.AccountID {
		return nil, errs.NotFound("project")
	}
	cp := *p
	f.projects[p.ID] = &cp
	return &cp, nil
}

func (f *fakeRepo) Tree(ctx context.Context, accountID string) ([]hierarchy.TreeNode, error) {
	f.treeCalls++
	workspaces, _ := f.ListWorkspaces(ctx, accountID)
	projects, _ := f.ListProjects(ctx, accountID, "")
	byWS := map[string][]hierarchy.Project{}
	for _, p := range projects {
		byWS[p.WorkspaceID] = append(byWS[p.WorkspaceID], p)
	}
	out := make([]hierarchy.TreeNode, 0, len(workspaces))
	for _, w := range workspaces {
		out = append(out, hierarchy.TreeNode{Workspace: w, Projects: byWS[w.ID]})
	}
	return out, nil
}

func (f *fakeRepo) ResourceAccounts(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if owner, ok := f.resources[id]; ok {
			out[id] = owner
		}
	}
	return out, nil
}

// ── shared scenario ─────────────────────────────────────────────────────────

func setup(t *testing.T) (*fakeRepo, *hierarchy.Service, context.Context) {
	t.Helper()
	repo := newFakeRepo()
	svc := hierarchy.NewService(repo)
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: "acct-1", ActorID: "usr-1", ActorKind: ctxutil.ActorUser,
	})
	return repo, svc, ctx
}

// ── service tests ───────────────────────────────────────────────────────────

func TestCreateWorkspaceRequiresAName(t *testing.T) {
	_, svc, ctx := setup(t)
	if _, err := svc.CreateWorkspace(ctx, "   ", "", "", nil); err == nil ||
		errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a workspace with no name should be refused; error: %v", err)
	}
}

func TestCreateWorkspaceRefusesAnInvalidKey(t *testing.T) {
	_, svc, ctx := setup(t)
	if _, err := svc.CreateWorkspace(ctx, "Platform", "plat", "", nil); err == nil {
		t.Error("a lowercase key should be refused")
	}
	w, err := svc.CreateWorkspace(ctx, "Platform", "PLAT", "", nil)
	if err != nil {
		t.Fatalf("a valid key was refused: %v", err)
	}
	if w.AccountID != "acct-1" {
		t.Errorf("the workspace should be born in the active account, got %q", w.AccountID)
	}
}

func TestOperationWithoutAnActiveAccountIsRefused(t *testing.T) {
	svc := hierarchy.NewService(newFakeRepo())
	// No AccountID: the SP-0 rule — a request with no active account is invalid.
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "usr-1"})
	if _, err := svc.CreateWorkspace(ctx, "Platform", "", "", nil); err == nil {
		t.Error("creating with no active account should be refused")
	}
	if _, err := svc.ListWorkspaces(ctx); err == nil {
		t.Error("listing with no active account should be refused")
	}
	if _, err := svc.GetTree(ctx); err == nil {
		t.Error("the tree with no active account should be refused")
	}
}

func TestAnotherAccountsWorkspaceIsNotVisible(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.workspaces["ws-foreign"] = &hierarchy.Workspace{
		ID: "ws-foreign", AccountID: "acct-2", Name: "From another account",
	}
	// Not "forbidden" but "not found": the id does not leak.
	if _, err := svc.GetWorkspace(ctx, "ws-foreign"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("another account's workspace should be not found; error: %v", err)
	}
	list, _ := svc.ListWorkspaces(ctx)
	if len(list) != 0 {
		t.Errorf("the listing should not bring another account's workspace: %v", list)
	}
}

func TestProjectInheritsTheWorkspacesAccount(t *testing.T) {
	_, svc, ctx := setup(t)
	ws, err := svc.CreateWorkspace(ctx, "Platform", "PLAT", "", nil)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	p, err := svc.CreateProject(ctx, ws.ID, "Cockpit", "the screen")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.AccountID != ws.AccountID {
		t.Errorf("the project should inherit the workspace's account: %q vs %q", p.AccountID, ws.AccountID)
	}
	if p.WorkspaceID != ws.ID {
		t.Errorf("the project should point at the workspace: %q", p.WorkspaceID)
	}
}

func TestProjectInAnotherAccountsWorkspaceIsRefused(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.workspaces["ws-foreign"] = &hierarchy.Workspace{
		ID: "ws-foreign", AccountID: "acct-2", Name: "From another account",
	}
	if _, err := svc.CreateProject(ctx, "ws-foreign", "Cockpit", ""); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("a project in another account's workspace should fail; error: %v", err)
	}
}

// The "resources from different accounts do not mix" rule: a resource id
// travels in the request body, and without this check sending someone else's
// integration id would be enough to use their credential.
func TestProjectCannotReferenceAnotherAccountsIntegration(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.resources["int-ours"] = "acct-1"
	repo.resources["int-theirs"] = "acct-2"

	ws, _ := svc.CreateWorkspace(ctx, "Platform", "PLAT", "", nil)
	p, _ := svc.CreateProject(ctx, ws.ID, "Cockpit", "")

	p.Repos = []hierarchy.ProjectRepo{{
		IntegrationID: "int-theirs", ExternalID: "42", Name: "dop-core",
	}}
	_, err := svc.UpdateProject(ctx, *p)
	if err == nil || errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a repo on another account's integration should be Invalid; error: %v", err)
	}

	// The same holds for the task manager and for attached resources.
	p.Repos = nil
	p.TaskManager = &hierarchy.ProjectTaskManager{IntegrationID: "int-theirs", ExternalSpaceID: "s1"}
	if _, err := svc.UpdateProject(ctx, *p); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("another account's task manager should be Invalid; error: %v", err)
	}
	p.TaskManager = nil
	p.Resources = []string{"int-theirs"}
	if _, err := svc.UpdateProject(ctx, *p); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("another account's attached resource should be Invalid; error: %v", err)
	}
}

func TestProjectAcceptsAnIntegrationFromTheSameAccount(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.resources["int-ours"] = "acct-1"

	ws, _ := svc.CreateWorkspace(ctx, "Platform", "PLAT", "", nil)
	p, _ := svc.CreateProject(ctx, ws.ID, "Cockpit", "")
	p.Repos = []hierarchy.ProjectRepo{{
		IntegrationID: "int-ours", ExternalID: "42", Name: "dop-core",
	}}

	saved, err := svc.UpdateProject(ctx, *p)
	if err != nil {
		t.Fatalf("an integration from the account itself should be accepted: %v", err)
	}
	// The default branch is filled in by the domain — database and domain do
	// not disagree.
	if saved.Repos[0].DefaultBranch != hierarchy.DefaultBranch {
		t.Errorf("the default branch should be %q, got %q",
			hierarchy.DefaultBranch, saved.Repos[0].DefaultBranch)
	}
}

func TestProjectRefusesANonexistentResource(t *testing.T) {
	_, svc, ctx := setup(t)
	ws, _ := svc.CreateWorkspace(ctx, "Platform", "PLAT", "", nil)
	p, _ := svc.CreateProject(ctx, ws.ID, "Cockpit", "")
	p.Resources = []string{"res-ghost"}
	if _, err := svc.UpdateProject(ctx, *p); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a nonexistent resource should be Invalid; error: %v", err)
	}
}

func TestRepoWithoutAnExternalIDIsRefused(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.resources["int-ours"] = "acct-1"
	ws, _ := svc.CreateWorkspace(ctx, "Platform", "PLAT", "", nil)
	p, _ := svc.CreateProject(ctx, ws.ID, "Cockpit", "")
	p.Repos = []hierarchy.ProjectRepo{{IntegrationID: "int-ours", Name: "dop-core"}}
	if _, err := svc.UpdateProject(ctx, *p); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a repo with no external id should be Invalid; error: %v", err)
	}
}

// GetTree exists so the tree costs ONE trip to the repository — not one per
// workspace. This test guards exactly that.
func TestGetTreeGroupsProjectsInASingleCall(t *testing.T) {
	repo, svc, ctx := setup(t)
	ws1, _ := svc.CreateWorkspace(ctx, "Platform", "PLAT", "", nil)
	ws2, _ := svc.CreateWorkspace(ctx, "Product", "PROD", "", nil)
	svc.CreateProject(ctx, ws1.ID, "Cockpit", "")
	svc.CreateProject(ctx, ws1.ID, "Core", "")
	svc.CreateProject(ctx, ws2.ID, "Site", "")

	repo.treeCalls = 0
	nodes, err := svc.GetTree(ctx)
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if repo.treeCalls != 1 {
		t.Errorf("the tree should cost ONE repository call, it cost %d", repo.treeCalls)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 workspaces in the tree, got %d", len(nodes))
	}
	total := 0
	for _, n := range nodes {
		total += len(n.Projects)
		for _, p := range n.Projects {
			if p.WorkspaceID != n.Workspace.ID {
				t.Errorf("project %q hung under the wrong workspace", p.ID)
			}
		}
	}
	if total != 3 {
		t.Errorf("expected 3 projects in the tree, got %d", total)
	}
}

func TestGetTreeIgnoresOtherAccounts(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.workspaces["ws-foreign"] = &hierarchy.Workspace{ID: "ws-foreign", AccountID: "acct-2"}
	repo.projects["prj-foreign"] = &hierarchy.Project{
		ID: "prj-foreign", AccountID: "acct-2", WorkspaceID: "ws-foreign",
	}
	nodes, err := svc.GetTree(ctx)
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("the tree should contain nothing from another account: %v", nodes)
	}
}

func TestUpdateWorkspaceDoesNotChangeAccounts(t *testing.T) {
	repo, svc, ctx := setup(t)
	ws, _ := svc.CreateWorkspace(ctx, "Platform", "PLAT", "", nil)

	// The caller tries to push another account through the request body.
	saved, err := svc.UpdateWorkspace(ctx, hierarchy.Workspace{
		ID: ws.ID, AccountID: "acct-2", Name: "Digital Platform", Key: "PLAT",
	})
	if err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if saved.AccountID != "acct-1" {
		t.Errorf("a workspace's account must not be changed by an update: %q", saved.AccountID)
	}
	if repo.workspaces[ws.ID].Name != "Digital Platform" {
		t.Error("the name should have been updated")
	}
}
