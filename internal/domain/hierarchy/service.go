package hierarchy

import (
	"context"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service concentra as regras da hierarquia. Recebe apenas PORTAS.
type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service { return &Service{repo: repo} }

// ── workspaces ───────────────────────────────────────────────────────────────

func (s *Service) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListWorkspaces(ctx, accountID)
}

// GetWorkspace busca SEMPRE dentro da conta ativa. Workspace de outra conta não
// devolve "sem permissão" e sim "não encontrado" — quem não é da conta nem
// deveria descobrir que o id existe.
func (s *Service) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errs.Invalid("identificador do workspace não informado")
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

// UpdateWorkspace ignora a conta que vier no argumento e usa a do contexto: a
// conta de um workspace não se troca por atualização.
func (s *Service) UpdateWorkspace(ctx context.Context, in Workspace) (*Workspace, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if in.ID == "" {
		return nil, errs.Invalid("identificador do workspace não informado")
	}
	name := strings.TrimSpace(in.Name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.Key)
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	// Confirma que o workspace é da conta ativa ANTES de escrever.
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

// ── projetos ─────────────────────────────────────────────────────────────────

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
		return nil, errs.Invalid("identificador do projeto não informado")
	}
	return s.repo.ProjectByID(ctx, accountID, id)
}

// CreateProject amarra o projeto ao workspace e HERDA dele a conta. É o único
// caminho pelo qual um projeto ganha conta.
func (s *Service) CreateProject(ctx context.Context, workspaceID, name, description string) (*Project, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if workspaceID == "" {
		return nil, errs.Invalid("projeto precisa de um workspace")
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

// UpdateProject aplica nome, descrição e os recursos anexados.
//
// É aqui que a regra dos recursos vale: tudo que o projeto referencia precisa
// ser da conta dona do WORKSPACE — não da conta ativa por acaso, não da conta
// de quem mandou o id.
func (s *Service) UpdateProject(ctx context.Context, in Project) (*Project, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if in.ID == "" {
		return nil, errs.Invalid("identificador do projeto não informado")
	}
	name := strings.TrimSpace(in.Name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	current, err := s.repo.ProjectByID(ctx, accountID, in.ID)
	if err != nil {
		return nil, err
	}

	// Mover de workspace é permitido — desde que o destino seja da conta ativa.
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
		AccountID:   ws.AccountID, // segue herdada, sempre
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

// ── árvore ───────────────────────────────────────────────────────────────────

// GetTree devolve workspaces com seus projetos em UMA chamada — é o que a tela
// inicial do cockpit consome. Ver o comentário em Repository.Tree.
func (s *Service) GetTree(ctx context.Context) ([]TreeNode, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.Tree(ctx, accountID)
}

// ── auxiliares ───────────────────────────────────────────────────────────────

// assertSameAccount é a regra do "recursos de contas diferentes não se
// misturam".
//
// Um id de recurso é adivinhável e viaja no corpo da requisição: sem esta
// checagem, bastaria mandar o id da integração de outra conta para o projeto
// passar a usar a credencial dela. Recusa como Invalid — e não como NotFound —
// porque o pedido em si está errado, não o recurso ausente.
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
			return errs.Invalid("recurso %s não existe", id)
		}
		if owner != accountID {
			return errs.Invalid("recurso %s pertence a outra conta", id)
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
			return errs.Invalid("repositório %q sem integração", r.Name)
		}
		if r.ExternalID == "" {
			return errs.Invalid("repositório %q sem identificador no provedor", r.Name)
		}
	}
	return nil
}
