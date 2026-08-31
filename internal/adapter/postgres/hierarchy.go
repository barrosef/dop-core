package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/hierarchy"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// HierarchyRepo implementa hierarchy.Repository. É o ÚNICO lugar com SQL de
// workspaces e projetos — o domínio nunca vê uma query.
type HierarchyRepo struct{ pool *pgxpool.Pool }

func NewHierarchyRepo(pool *pgxpool.Pool) *HierarchyRepo { return &HierarchyRepo{pool: pool} }

// ── workspaces ───────────────────────────────────────────────────────────────

const workspaceCols = `id, account_id, name, COALESCE(key,''), COALESCE(description,''),
	tags, created_at, updated_at`

func scanWorkspace(row pgx.Row) (*hierarchy.Workspace, error) {
	var w hierarchy.Workspace
	if err := row.Scan(&w.ID, &w.AccountID, &w.Name, &w.Key, &w.Description,
		&w.Tags, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *HierarchyRepo) ListWorkspaces(ctx context.Context, accountID string) ([]hierarchy.Workspace, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE account_id = $1 ORDER BY name`, accountID)
	if err != nil {
		return nil, Translate(err, "workspaces")
	}
	defer rows.Close()

	var out []hierarchy.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, Translate(err, "workspaces")
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// WorkspaceByID filtra por conta DENTRO do WHERE: workspace de outra conta
// devolve "não encontrado", não "sem permissão" — a existência do id não vaza.
func (r *HierarchyRepo) WorkspaceByID(ctx context.Context, accountID, id string) (*hierarchy.Workspace, error) {
	w, err := scanWorkspace(r.pool.QueryRow(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE id = $1 AND account_id = $2`, id, accountID))
	if err != nil {
		return nil, Translate(err, "workspace")
	}
	return w, nil
}

func (r *HierarchyRepo) CreateWorkspace(ctx context.Context, w *hierarchy.Workspace) (*hierarchy.Workspace, error) {
	var saved *hierarchy.Workspace
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO workspaces (account_id, name, key, description, tags)
			VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5)
			RETURNING `+workspaceCols,
			w.AccountID, w.Name, w.Key, w.Description, tagsOf(w.Tags))
		var err error
		saved, err = scanWorkspace(row)
		if err != nil {
			return Translate(err, "workspace")
		}
		// Evento na MESMA transação — outbox transacional (ADR-0019).
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "workspace", AggregateID: saved.ID,
			Type:    "dop.hierarchy.workspace.created",
			Payload: mustJSON(map[string]any{"name": saved.Name, "key": saved.Key}),
		})
	})
	return saved, err
}

func (r *HierarchyRepo) UpdateWorkspace(ctx context.Context, w *hierarchy.Workspace) (*hierarchy.Workspace, error) {
	var saved *hierarchy.Workspace
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE workspaces
			   SET name = $3, key = NULLIF($4,''), description = NULLIF($5,''),
			       tags = $6, updated_at = now()
			 WHERE id = $1 AND account_id = $2
			 RETURNING `+workspaceCols,
			w.ID, w.AccountID, w.Name, w.Key, w.Description, tagsOf(w.Tags))
		var err error
		saved, err = scanWorkspace(row)
		if err != nil {
			return Translate(err, "workspace")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "workspace", AggregateID: saved.ID,
			Type:    "dop.hierarchy.workspace.updated",
			Payload: mustJSON(map[string]any{"name": saved.Name, "key": saved.Key}),
		})
	})
	return saved, err
}

// ── projetos ─────────────────────────────────────────────────────────────────

const projectCols = `id, account_id, workspace_id, name, COALESCE(description,''),
	rules, created_at, updated_at`

func scanProject(row pgx.Row) (*hierarchy.Project, error) {
	var p hierarchy.Project
	var doc []byte
	if err := row.Scan(&p.ID, &p.AccountID, &p.WorkspaceID, &p.Name, &p.Description,
		&doc, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Rules, p.TaskManager = decodeProjectDoc(doc)
	return &p, nil
}

func (r *HierarchyRepo) ListProjects(ctx context.Context, accountID, workspaceID string) ([]hierarchy.Project, error) {
	sql := `SELECT ` + projectCols + ` FROM projects WHERE account_id = $1`
	args := []any{accountID}
	if workspaceID != "" {
		sql += ` AND workspace_id = $2`
		args = append(args, workspaceID)
	}
	projects, err := r.queryProjects(ctx, sql+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	if err := r.attachChildren(ctx, projects); err != nil {
		return nil, err
	}
	return projects, nil
}

func (r *HierarchyRepo) ProjectByID(ctx context.Context, accountID, id string) (*hierarchy.Project, error) {
	p, err := scanProject(r.pool.QueryRow(ctx,
		`SELECT `+projectCols+` FROM projects WHERE id = $1 AND account_id = $2`, id, accountID))
	if err != nil {
		return nil, Translate(err, "projeto")
	}
	one := []hierarchy.Project{*p}
	if err := r.attachChildren(ctx, one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

func (r *HierarchyRepo) CreateProject(ctx context.Context, p *hierarchy.Project) (*hierarchy.Project, error) {
	var saved *hierarchy.Project
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO projects (account_id, workspace_id, name, description, rules)
			VALUES ($1, $2, $3, NULLIF($4,''), $5)
			RETURNING `+projectCols,
			p.AccountID, p.WorkspaceID, p.Name, p.Description, encodeProjectDoc(p))
		var err error
		saved, err = scanProject(row)
		if err != nil {
			return Translate(err, "projeto")
		}
		saved.Repos, saved.Resources, saved.TaskManager = p.Repos, p.Resources, p.TaskManager
		if err := writeProjectChildren(ctx, tx, saved); err != nil {
			return err
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "project", AggregateID: saved.ID,
			Type: "dop.hierarchy.project.created",
			Payload: mustJSON(map[string]any{
				"workspace_id": saved.WorkspaceID, "name": saved.Name,
			}),
		})
	})
	return saved, err
}

func (r *HierarchyRepo) UpdateProject(ctx context.Context, p *hierarchy.Project) (*hierarchy.Project, error) {
	var saved *hierarchy.Project
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE projects
			   SET workspace_id = $3, name = $4, description = NULLIF($5,''),
			       rules = $6, updated_at = now()
			 WHERE id = $1 AND account_id = $2
			 RETURNING `+projectCols,
			p.ID, p.AccountID, p.WorkspaceID, p.Name, p.Description, encodeProjectDoc(p))
		var err error
		saved, err = scanProject(row)
		if err != nil {
			return Translate(err, "projeto")
		}
		saved.Repos, saved.Resources, saved.TaskManager = p.Repos, p.Resources, p.TaskManager
		if err := writeProjectChildren(ctx, tx, saved); err != nil {
			return err
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID, Aggregate: "project", AggregateID: saved.ID,
			Type: "dop.hierarchy.project.updated",
			Payload: mustJSON(map[string]any{
				"workspace_id": saved.WorkspaceID, "name": saved.Name,
				"repos": len(saved.Repos), "resources": len(saved.Resources),
			}),
		})
	})
	return saved, err
}

// ── árvore ───────────────────────────────────────────────────────────────────

// Tree monta a árvore com TRÊS varreduras fixas — workspaces, projetos e os
// filhos dos projetos — em vez de uma consulta por workspace. É o ponto inteiro
// de a operação existir na porta.
func (r *HierarchyRepo) Tree(ctx context.Context, accountID string) ([]hierarchy.TreeNode, error) {
	workspaces, err := r.ListWorkspaces(ctx, accountID)
	if err != nil {
		return nil, err
	}
	projects, err := r.ListProjects(ctx, accountID, "")
	if err != nil {
		return nil, err
	}

	byWorkspace := make(map[string][]hierarchy.Project, len(workspaces))
	for _, p := range projects {
		byWorkspace[p.WorkspaceID] = append(byWorkspace[p.WorkspaceID], p)
	}
	nodes := make([]hierarchy.TreeNode, 0, len(workspaces))
	for _, w := range workspaces {
		nodes = append(nodes, hierarchy.TreeNode{Workspace: w, Projects: byWorkspace[w.ID]})
	}
	return nodes, nil
}

// ResourceAccounts responde de quem é cada recurso. Ids inexistentes ficam de
// fora do mapa — a ausência é resposta, e quem decide o que fazer é o domínio.
func (r *HierarchyRepo) ResourceAccounts(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id, account_id FROM resources WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, Translate(err, "recursos")
	}
	defer rows.Close()
	for rows.Next() {
		var id, accountID string
		if err := rows.Scan(&id, &accountID); err != nil {
			return nil, Translate(err, "recursos")
		}
		out[id] = accountID
	}
	return out, rows.Err()
}

// ── auxiliares ───────────────────────────────────────────────────────────────

func (r *HierarchyRepo) queryProjects(ctx context.Context, sql string, args ...any) ([]hierarchy.Project, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, Translate(err, "projetos")
	}
	defer rows.Close()
	var out []hierarchy.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, Translate(err, "projetos")
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// attachChildren carrega repositórios e recursos de TODOS os projetos de uma
// vez — duas consultas, e não duas por projeto.
func (r *HierarchyRepo) attachChildren(ctx context.Context, projects []hierarchy.Project) error {
	if len(projects) == 0 {
		return nil
	}
	ids := make([]string, 0, len(projects))
	index := make(map[string]int, len(projects))
	for i, p := range projects {
		ids = append(ids, p.ID)
		index[p.ID] = i
	}

	ptrs := make([]*hierarchy.Project, len(projects))
	for i := range projects {
		ptrs[i] = &projects[i]
	}
	if err := loadProjectTaskManagers(ctx, r.pool, ptrs); err != nil {
		return err
	}

	repos, err := r.pool.Query(ctx, `
		SELECT project_id, id, integration_id, external_id, name, default_branch, pr_targets
		  FROM project_repos WHERE project_id = ANY($1::uuid[]) ORDER BY name`, ids)
	if err != nil {
		return Translate(err, "repositórios do projeto")
	}
	defer repos.Close()
	for repos.Next() {
		var projectID string
		var pr hierarchy.ProjectRepo
		if err := repos.Scan(&projectID, &pr.ID, &pr.IntegrationID, &pr.ExternalID,
			&pr.Name, &pr.DefaultBranch, &pr.PRTargets); err != nil {
			return Translate(err, "repositórios do projeto")
		}
		if i, ok := index[projectID]; ok {
			projects[i].Repos = append(projects[i].Repos, pr)
		}
	}
	if err := repos.Err(); err != nil {
		return Translate(err, "repositórios do projeto")
	}

	res, err := r.pool.Query(ctx,
		`SELECT project_id, resource_id FROM project_resources WHERE project_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return Translate(err, "recursos do projeto")
	}
	defer res.Close()
	for res.Next() {
		var projectID, resourceID string
		if err := res.Scan(&projectID, &resourceID); err != nil {
			return Translate(err, "recursos do projeto")
		}
		if i, ok := index[projectID]; ok {
			projects[i].Resources = append(projects[i].Resources, resourceID)
		}
	}
	return res.Err()
}

// writeProjectChildren reescreve os filhos do projeto: a atualização é do
// CONJUNTO, não incremental — o cliente manda a lista que quer ver, e apagar e
// regravar dentro da transação evita a divergência de um diff mal feito.
func writeProjectChildren(ctx context.Context, tx pgx.Tx, p *hierarchy.Project) error {
	if _, err := tx.Exec(ctx, `DELETE FROM project_repos WHERE project_id = $1`, p.ID); err != nil {
		return Translate(err, "repositórios do projeto")
	}
	for i := range p.Repos {
		branch := p.Repos[i].DefaultBranch
		if branch == "" {
			branch = hierarchy.DefaultBranch
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO project_repos
			       (project_id, integration_id, external_id, name, default_branch, pr_targets)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id`,
			p.ID, p.Repos[i].IntegrationID, p.Repos[i].ExternalID, p.Repos[i].Name,
			branch, tagsOf(p.Repos[i].PRTargets)).
			Scan(&p.Repos[i].ID); err != nil {
			return Translate(err, "repositório do projeto")
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM project_resources WHERE project_id = $1`, p.ID); err != nil {
		return Translate(err, "recursos do projeto")
	}
	for _, id := range p.Resources {
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_resources (project_id, resource_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, p.ID, id); err != nil {
			return Translate(err, "recurso do projeto")
		}
	}
	return writeProjectTaskManager(ctx, tx, p)
}

// writeProjectTaskManager grava o vínculo com o quadro do provedor.
//
// Nil apaga: um projeto que perdeu o quadro não fica com o vínculo velho
// pendurado. As FKs impedem apontar para integração inexistente — era
// exatamente isso que o envelope JSON não conseguia garantir.
func writeProjectTaskManager(ctx context.Context, tx pgx.Tx, p *hierarchy.Project) error {
	if p.TaskManager == nil {
		_, err := tx.Exec(ctx, `DELETE FROM project_task_managers WHERE project_id = $1`, p.ID)
		return Translate(err, "quadro do projeto")
	}
	tm := p.TaskManager
	_, err := tx.Exec(ctx, `
		INSERT INTO project_task_managers
		       (project_id, integration_id, external_space_id, external_project_id, card_types)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (project_id) DO UPDATE
		   SET integration_id      = EXCLUDED.integration_id,
		       external_space_id   = EXCLUDED.external_space_id,
		       external_project_id = EXCLUDED.external_project_id,
		       card_types          = EXCLUDED.card_types,
		       updated_at          = now()`,
		p.ID, tm.IntegrationID, tm.ExternalSpaceID, tm.ExternalProjectID, tagsOf(tm.CardTypes))
	return Translate(err, "quadro do projeto")
}

// loadProjectTaskManagers preenche TaskManager de um lote de projetos numa
// única consulta — a árvore do cockpit traz N projetos e não pode virar N+1.
func loadProjectTaskManagers(ctx context.Context, q pgxQuerier, projects []*hierarchy.Project) error {
	if len(projects) == 0 {
		return nil
	}
	byID := make(map[string]*hierarchy.Project, len(projects))
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		byID[p.ID] = p
		ids = append(ids, p.ID)
	}
	rows, err := q.Query(ctx, `
		SELECT project_id, integration_id, external_space_id, external_project_id, card_types
		  FROM project_task_managers WHERE project_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return Translate(err, "quadros dos projetos")
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		tm := &hierarchy.ProjectTaskManager{}
		if err := rows.Scan(&pid, &tm.IntegrationID, &tm.ExternalSpaceID,
			&tm.ExternalProjectID, &tm.CardTypes); err != nil {
			return Translate(err, "quadro do projeto")
		}
		if p, ok := byID[pid]; ok {
			p.TaskManager = tm
		}
	}
	return Translate(rows.Err(), "quadros dos projetos")
}

// projectDoc: MANTIDO apenas para ler dados gravados antes da migração 0004.
// Escrita nova usa a tabela project_task_managers.
//
// O schema atual não tem coluna para o vínculo do gerenciador de tarefas — que
// é POR PROJETO (a mesma integração serve muitos projetos, com espaços e
// quadros diferentes). Até a migração existir, o vínculo viaja junto das regras
// neste envelope, e a leitura aceita as duas formas: o envelope e o array puro
// do DEFAULT '[]'. Assim nada se perde e nada quebra quando a coluna nascer.
type projectDoc struct {
	Rules       []string        `json:"rules"`
	TaskManager *taskManagerDoc `json:"task_manager,omitempty"`
}

type taskManagerDoc struct {
	IntegrationID     string   `json:"integration_id"`
	ExternalSpaceID   string   `json:"external_space_id"`
	ExternalProjectID string   `json:"external_project_id"`
	CardTypes         []string `json:"card_types,omitempty"`
}

func encodeProjectDoc(p *hierarchy.Project) []byte {
	// A partir da migração 0004 a coluna guarda SÓ as regras; o vínculo do
	// task manager tem tabela própria, com FK — ver writeProjectTaskManager.
	if len(p.Rules) == 0 {
		return []byte(`[]`)
	}
	return mustJSON(p.Rules)
}

func decodeProjectDoc(raw []byte) ([]string, *hierarchy.ProjectTaskManager) { //nolint:unparam // envelope legado
	if len(raw) == 0 {
		return nil, nil
	}
	var doc projectDoc
	if err := json.Unmarshal(raw, &doc); err == nil {
		var tm *hierarchy.ProjectTaskManager
		if doc.TaskManager != nil {
			tm = &hierarchy.ProjectTaskManager{
				IntegrationID:     doc.TaskManager.IntegrationID,
				ExternalSpaceID:   doc.TaskManager.ExternalSpaceID,
				ExternalProjectID: doc.TaskManager.ExternalProjectID,
				CardTypes:         doc.TaskManager.CardTypes,
			}
		}
		return doc.Rules, tm
	}
	// Forma antiga: array puro de regras, como manda o DEFAULT da coluna.
	var rules []string
	_ = json.Unmarshal(raw, &rules)
	return rules, nil
}

// tagsOf garante array vazio em vez de NULL: as colunas são NOT NULL DEFAULT
// '{}' e nil viraria violação em vez de lista vazia.
func tagsOf(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

var _ hierarchy.Repository = (*HierarchyRepo)(nil)
