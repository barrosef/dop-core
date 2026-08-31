package hierarchy_test

import (
	"context"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/hierarchy"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM banco: o repositório é porta, e aqui entra um duplo
// em memória. É o retorno prático da arquitetura hexagonal.

func TestNormalizeKey(t *testing.T) {
	casos := map[string]string{
		"dop":        "DOP",
		"  dop-x  ":  "DOPX",
		"Plat form1": "PLATFORM1",
		"---":        "",
	}
	for entrada, esperado := range casos {
		if got := hierarchy.NormalizeKey(entrada); got != esperado {
			t.Errorf("NormalizeKey(%q) = %q, esperado %q", entrada, got, esperado)
		}
	}
}

func TestValidateKey(t *testing.T) {
	// Chave é OPCIONAL: ausência não é erro.
	if err := hierarchy.ValidateKey(""); err != nil {
		t.Errorf("chave vazia deveria ser aceita: %v", err)
	}
	if err := hierarchy.ValidateKey("DOP"); err != nil {
		t.Errorf("chave maiúscula curta recusada: %v", err)
	}
	if err := hierarchy.ValidateKey("dop"); err == nil {
		t.Error("chave minúscula deveria ser recusada")
	}
	if err := hierarchy.ValidateKey("DOP-X"); err == nil {
		t.Error("chave com pontuação deveria ser recusada")
	}
	if err := hierarchy.ValidateKey("D"); err == nil {
		t.Error("chave de 1 caractere deveria ser recusada")
	}
	if err := hierarchy.ValidateKey("PLATAFORMADIGITAL"); err == nil {
		t.Error("chave longa deveria ser recusada")
	}
}

func TestReferencedResourceIDsJuntaTudoSemRepetir(t *testing.T) {
	p := hierarchy.Project{
		Repos: []hierarchy.ProjectRepo{
			{IntegrationID: "int-1"}, {IntegrationID: "int-1"}, {IntegrationID: "int-2"},
		},
		TaskManager: &hierarchy.ProjectTaskManager{IntegrationID: "int-3"},
		Resources:   []string{"res-1", "int-2"},
	}
	got := p.ReferencedResourceIDs()
	if len(got) != 4 {
		t.Fatalf("esperado 4 ids distintos, veio %v", got)
	}
}

// ── duplo em memória ────────────────────────────────────────────────────────

type fakeRepo struct {
	workspaces map[string]*hierarchy.Workspace
	projects   map[string]*hierarchy.Project
	// dono de cada recurso, por id — é o que sustenta a regra das contas.
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
		return nil, errs.NotFound("projeto")
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
		return nil, errs.NotFound("projeto")
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

// ── cenário comum ───────────────────────────────────────────────────────────

func setup(t *testing.T) (*fakeRepo, *hierarchy.Service, context.Context) {
	t.Helper()
	repo := newFakeRepo()
	svc := hierarchy.NewService(repo)
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: "acct-1", ActorID: "usr-1", ActorKind: ctxutil.ActorUser,
	})
	return repo, svc, ctx
}

// ── testes do serviço ───────────────────────────────────────────────────────

func TestCreateWorkspaceExigeNome(t *testing.T) {
	_, svc, ctx := setup(t)
	if _, err := svc.CreateWorkspace(ctx, "   ", "", "", nil); err == nil ||
		errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("workspace sem nome deveria ser recusado; erro: %v", err)
	}
}

func TestCreateWorkspaceRecusaChaveInvalida(t *testing.T) {
	_, svc, ctx := setup(t)
	if _, err := svc.CreateWorkspace(ctx, "Plataforma", "plat", "", nil); err == nil {
		t.Error("chave minúscula deveria ser recusada")
	}
	w, err := svc.CreateWorkspace(ctx, "Plataforma", "PLAT", "", nil)
	if err != nil {
		t.Fatalf("chave válida recusada: %v", err)
	}
	if w.AccountID != "acct-1" {
		t.Errorf("workspace deveria nascer na conta ativa, veio %q", w.AccountID)
	}
}

func TestOperacaoSemContaAtivaERecusada(t *testing.T) {
	svc := hierarchy.NewService(newFakeRepo())
	// Sem AccountID: regra do SP-0 — requisição sem conta ativa é inválida.
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "usr-1"})
	if _, err := svc.CreateWorkspace(ctx, "Plataforma", "", "", nil); err == nil {
		t.Error("criação sem conta ativa deveria ser recusada")
	}
	if _, err := svc.ListWorkspaces(ctx); err == nil {
		t.Error("listagem sem conta ativa deveria ser recusada")
	}
	if _, err := svc.GetTree(ctx); err == nil {
		t.Error("árvore sem conta ativa deveria ser recusada")
	}
}

func TestWorkspaceDeOutraContaNaoEVisto(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.workspaces["ws-alheio"] = &hierarchy.Workspace{
		ID: "ws-alheio", AccountID: "acct-2", Name: "De outra conta",
	}
	// Não é "sem permissão" e sim "não encontrado": o id não vaza.
	if _, err := svc.GetWorkspace(ctx, "ws-alheio"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("workspace de outra conta deveria ser não encontrado; erro: %v", err)
	}
	list, _ := svc.ListWorkspaces(ctx)
	if len(list) != 0 {
		t.Errorf("a listagem não deveria trazer workspace de outra conta: %v", list)
	}
}

func TestProjetoHerdaContaDoWorkspace(t *testing.T) {
	_, svc, ctx := setup(t)
	ws, err := svc.CreateWorkspace(ctx, "Plataforma", "PLAT", "", nil)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	p, err := svc.CreateProject(ctx, ws.ID, "Cockpit", "a tela")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.AccountID != ws.AccountID {
		t.Errorf("projeto deveria herdar a conta do workspace: %q vs %q", p.AccountID, ws.AccountID)
	}
	if p.WorkspaceID != ws.ID {
		t.Errorf("projeto deveria apontar para o workspace: %q", p.WorkspaceID)
	}
}

func TestProjetoEmWorkspaceDeOutraContaERecusado(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.workspaces["ws-alheio"] = &hierarchy.Workspace{
		ID: "ws-alheio", AccountID: "acct-2", Name: "De outra conta",
	}
	if _, err := svc.CreateProject(ctx, "ws-alheio", "Cockpit", ""); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("projeto em workspace de outra conta deveria falhar; erro: %v", err)
	}
}

// A regra do "recursos de contas diferentes não se misturam": um id de recurso
// viaja no corpo da requisição, e sem esta checagem bastaria mandar o id da
// integração alheia para usar a credencial dela.
func TestProjetoNaoReferenciaIntegracaoDeOutraConta(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.resources["int-nossa"] = "acct-1"
	repo.resources["int-alheia"] = "acct-2"

	ws, _ := svc.CreateWorkspace(ctx, "Plataforma", "PLAT", "", nil)
	p, _ := svc.CreateProject(ctx, ws.ID, "Cockpit", "")

	p.Repos = []hierarchy.ProjectRepo{{
		IntegrationID: "int-alheia", ExternalID: "42", Name: "dop-core",
	}}
	_, err := svc.UpdateProject(ctx, *p)
	if err == nil || errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("repo de integração de outra conta deveria dar Invalid; erro: %v", err)
	}

	// O mesmo vale para o gerenciador de tarefas e para recursos anexados.
	p.Repos = nil
	p.TaskManager = &hierarchy.ProjectTaskManager{IntegrationID: "int-alheia", ExternalSpaceID: "s1"}
	if _, err := svc.UpdateProject(ctx, *p); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("gerenciador de tarefas de outra conta deveria dar Invalid; erro: %v", err)
	}
	p.TaskManager = nil
	p.Resources = []string{"int-alheia"}
	if _, err := svc.UpdateProject(ctx, *p); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("recurso anexado de outra conta deveria dar Invalid; erro: %v", err)
	}
}

func TestProjetoAceitaIntegracaoDaMesmaConta(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.resources["int-nossa"] = "acct-1"

	ws, _ := svc.CreateWorkspace(ctx, "Plataforma", "PLAT", "", nil)
	p, _ := svc.CreateProject(ctx, ws.ID, "Cockpit", "")
	p.Repos = []hierarchy.ProjectRepo{{
		IntegrationID: "int-nossa", ExternalID: "42", Name: "dop-core",
	}}

	saved, err := svc.UpdateProject(ctx, *p)
	if err != nil {
		t.Fatalf("integração da própria conta deveria ser aceita: %v", err)
	}
	// Branch padrão preenchida pelo domínio — banco e domínio não discordam.
	if saved.Repos[0].DefaultBranch != hierarchy.DefaultBranch {
		t.Errorf("branch padrão deveria ser %q, veio %q",
			hierarchy.DefaultBranch, saved.Repos[0].DefaultBranch)
	}
}

func TestProjetoRecusaRecursoInexistente(t *testing.T) {
	_, svc, ctx := setup(t)
	ws, _ := svc.CreateWorkspace(ctx, "Plataforma", "PLAT", "", nil)
	p, _ := svc.CreateProject(ctx, ws.ID, "Cockpit", "")
	p.Resources = []string{"res-fantasma"}
	if _, err := svc.UpdateProject(ctx, *p); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("recurso inexistente deveria dar Invalid; erro: %v", err)
	}
}

func TestRepoSemIdentificadorExternoERecusado(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.resources["int-nossa"] = "acct-1"
	ws, _ := svc.CreateWorkspace(ctx, "Plataforma", "PLAT", "", nil)
	p, _ := svc.CreateProject(ctx, ws.ID, "Cockpit", "")
	p.Repos = []hierarchy.ProjectRepo{{IntegrationID: "int-nossa", Name: "dop-core"}}
	if _, err := svc.UpdateProject(ctx, *p); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("repo sem id externo deveria dar Invalid; erro: %v", err)
	}
}

// GetTree existe para a árvore custar UMA ida ao repositório — não uma por
// workspace. O teste protege exatamente isso.
func TestGetTreeAgrupaProjetosEmUmaChamada(t *testing.T) {
	repo, svc, ctx := setup(t)
	ws1, _ := svc.CreateWorkspace(ctx, "Plataforma", "PLAT", "", nil)
	ws2, _ := svc.CreateWorkspace(ctx, "Produto", "PROD", "", nil)
	svc.CreateProject(ctx, ws1.ID, "Cockpit", "")
	svc.CreateProject(ctx, ws1.ID, "Core", "")
	svc.CreateProject(ctx, ws2.ID, "Site", "")

	repo.treeCalls = 0
	nodes, err := svc.GetTree(ctx)
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if repo.treeCalls != 1 {
		t.Errorf("a árvore deveria custar UMA chamada ao repositório, custou %d", repo.treeCalls)
	}
	if len(nodes) != 2 {
		t.Fatalf("esperados 2 workspaces na árvore, vieram %d", len(nodes))
	}
	total := 0
	for _, n := range nodes {
		total += len(n.Projects)
		for _, p := range n.Projects {
			if p.WorkspaceID != n.Workspace.ID {
				t.Errorf("projeto %q pendurado no workspace errado", p.ID)
			}
		}
	}
	if total != 3 {
		t.Errorf("esperados 3 projetos na árvore, vieram %d", total)
	}
}

func TestGetTreeIgnoraOutraConta(t *testing.T) {
	repo, svc, ctx := setup(t)
	repo.workspaces["ws-alheio"] = &hierarchy.Workspace{ID: "ws-alheio", AccountID: "acct-2"}
	repo.projects["prj-alheio"] = &hierarchy.Project{
		ID: "prj-alheio", AccountID: "acct-2", WorkspaceID: "ws-alheio",
	}
	nodes, err := svc.GetTree(ctx)
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("a árvore não deveria conter nada de outra conta: %v", nodes)
	}
}

func TestUpdateWorkspaceNaoTrocaDeConta(t *testing.T) {
	repo, svc, ctx := setup(t)
	ws, _ := svc.CreateWorkspace(ctx, "Plataforma", "PLAT", "", nil)

	// O chamador tenta empurrar outra conta pelo corpo da requisição.
	saved, err := svc.UpdateWorkspace(ctx, hierarchy.Workspace{
		ID: ws.ID, AccountID: "acct-2", Name: "Plataforma Digital", Key: "PLAT",
	})
	if err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if saved.AccountID != "acct-1" {
		t.Errorf("a conta do workspace não pode ser trocada por atualização: %q", saved.AccountID)
	}
	if repo.workspaces[ws.ID].Name != "Plataforma Digital" {
		t.Error("o nome deveria ter sido atualizado")
	}
}
