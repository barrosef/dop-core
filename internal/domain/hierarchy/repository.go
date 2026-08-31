package hierarchy

import "context"

// Repository é a PORTA de persistência do domínio de hierarquia.
//
// Declarada aqui, em linguagem de domínio; implementada em
// internal/adapter/postgres. O domínio nunca vê SQL.
//
// Toda operação recebe accountID explicitamente: o filtro por conta ativa é
// parâmetro da porta, não detalhe que o adaptador possa esquecer.
type Repository interface {
	// Workspaces
	ListWorkspaces(ctx context.Context, accountID string) ([]Workspace, error)
	WorkspaceByID(ctx context.Context, accountID, id string) (*Workspace, error)
	CreateWorkspace(ctx context.Context, w *Workspace) (*Workspace, error)
	UpdateWorkspace(ctx context.Context, w *Workspace) (*Workspace, error)

	// Projetos. workspaceID vazio = todos os projetos da conta.
	ListProjects(ctx context.Context, accountID, workspaceID string) ([]Project, error)
	ProjectByID(ctx context.Context, accountID, id string) (*Project, error)
	CreateProject(ctx context.Context, p *Project) (*Project, error)
	UpdateProject(ctx context.Context, p *Project) (*Project, error)

	// Tree devolve workspaces e projetos da conta já agrupados.
	//
	// Existe como operação PRÓPRIA — e não como laço sobre ListProjects — para
	// que a árvore do cockpit custe uma varredura, não uma consulta por
	// workspace. O N+1 aqui apareceria na primeira tela de todo mundo.
	Tree(ctx context.Context, accountID string) ([]TreeNode, error)

	// ResourceAccounts diz a que conta pertence cada recurso pedido. Ids
	// inexistentes simplesmente não voltam no mapa.
	//
	// É o que permite ao domínio recusar recurso de outra conta sem saber o que
	// é um recurso — quem sabe é o domínio de recursos.
	ResourceAccounts(ctx context.Context, ids []string) (map[string]string, error)
}
