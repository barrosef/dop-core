package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// WorkflowRepo implementa workflow.Repository e workflow.Ancestry. É o ÚNICO
// lugar com SQL de fluxo — o domínio nunca vê uma query.
//
// As duas portas moram no mesmo tipo porque a linhagem é uma consulta sobre as
// MESMAS tabelas da árvore que a resolução já precisa varrer. Separá-las em
// dois adaptadores duplicaria o pool e não separaria nada de verdade.
type WorkflowRepo struct{ pool *pgxpool.Pool }

func NewWorkflowRepo(pool *pgxpool.Pool) *WorkflowRepo { return &WorkflowRepo{pool: pool} }

// flowCols junta identidade (flows) e conteúdo congelado (flow_versions).
// O fluxo que o domínio manipula é sempre UMA versão — nunca a linha de flows
// sozinha, que não tem etapa nenhuma.
const flowCols = `f.id, COALESCE(f.account_id::text,''), f.owner_scope::text,
	COALESCE(f.owner_id::text,''), v.name, COALESCE(v.description,''), v.version,
	v.stages, COALESCE(v.created_by::text,''), f.created_at, v.created_at`

// currentJoin amarra a versão CORRENTE; frozenJoin amarra uma versão pedida.
const currentJoin = ` FROM flows f JOIN flow_versions v
	ON v.flow_id = f.id AND v.version = f.current_version`

// visibleToAccount é o filtro multi-tenant das LEITURAS.
//
// O catálogo da plataforma entra porque não tem dono e vale para todas as
// contas — é o nível 0 da cadeia. Toda ESCRITA usa `f.account_id = $1` puro:
// nenhuma conta escreve no catálogo.
const visibleToAccount = ` WHERE (f.account_id = $1 OR f.owner_scope = 'platform')`

func scanFlow(row pgx.Row) (*workflow.Flow, error) {
	var (
		f      workflow.Flow
		scope  string
		stages []byte
	)
	if err := row.Scan(&f.ID, &f.AccountID, &scope, &f.OwnerID, &f.Name, &f.Description,
		&f.Version, &stages, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, err
	}
	f.OwnerScope = workflow.Scope(scope)
	f.Stages = decodeStages(stages)
	return &f, nil
}

// ── leitura ──────────────────────────────────────────────────────────────────

func (r *WorkflowRepo) List(ctx context.Context, accountID string, scope workflow.Scope, ownerID string) ([]workflow.Flow, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+flowCols+currentJoin+visibleToAccount+`
		  AND ($2 = '' OR f.owner_scope::text = $2)
		  AND ($3 = '' OR COALESCE(f.owner_id::text,'') = $3)
		ORDER BY f.owner_scope, v.name`, accountID, string(scope), ownerID)
	if err != nil {
		return nil, Translate(err, "fluxos")
	}
	defer rows.Close()

	var out []workflow.Flow
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, Translate(err, "fluxos")
		}
		out = append(out, *f)
	}
	return out, Translate(rows.Err(), "fluxos")
}

func (r *WorkflowRepo) ByID(ctx context.Context, accountID, id string) (*workflow.Flow, error) {
	f, err := scanFlow(r.pool.QueryRow(ctx,
		`SELECT `+flowCols+currentJoin+visibleToAccount+` AND f.id = $2`, accountID, id))
	if err != nil {
		return nil, Translate(err, "fluxo")
	}
	return f, nil
}

// VersionOf lê uma versão congelada. Não passa por current_version de
// propósito: quem pergunta pela versão 3 quer a 3, mesmo que o fluxo já esteja
// na 7 — é assim que a demanda em execução continua enxergando o que assinou.
func (r *WorkflowRepo) VersionOf(ctx context.Context, accountID, id string, version int32) (*workflow.Flow, error) {
	f, err := scanFlow(r.pool.QueryRow(ctx, `SELECT `+flowCols+`
		  FROM flows f JOIN flow_versions v ON v.flow_id = f.id
		 WHERE (f.account_id = $1 OR f.owner_scope = 'platform')
		   AND f.id = $2 AND v.version = $3`, accountID, id, version))
	if err != nil {
		return nil, Translate(err, "versão do fluxo")
	}
	return f, nil
}

// ByOwners traz os fluxos de todos os níveis da cadeia numa varredura. Ver o
// comentário na porta: a resolução acontece a cada abertura de demanda.
func (r *WorkflowRepo) ByOwners(ctx context.Context, accountID string, refs []workflow.ScopeRef) ([]workflow.Flow, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	scopes := make([]string, 0, len(refs))
	owners := make([]string, 0, len(refs))
	for _, ref := range refs {
		scopes = append(scopes, string(ref.Scope))
		owners = append(owners, ref.ID)
	}
	rows, err := r.pool.Query(ctx, `SELECT `+flowCols+currentJoin+visibleToAccount+`
		  AND (f.owner_scope::text, COALESCE(f.owner_id::text,''))
		      IN (SELECT * FROM unnest($2::text[], $3::text[]))`,
		accountID, scopes, owners)
	if err != nil {
		return nil, Translate(err, "fluxos da cadeia")
	}
	defer rows.Close()

	var out []workflow.Flow
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, Translate(err, "fluxos da cadeia")
		}
		out = append(out, *f)
	}
	return out, Translate(rows.Err(), "fluxos da cadeia")
}

// ── escrita ──────────────────────────────────────────────────────────────────

// Create grava o fluxo e a versão 1 na MESMA transação do evento (ADR-0019).
//
// A repetição é resolvida pela chave de idempotência e não por "consultar
// antes de inserir": entre a consulta e a inserção cabe outra requisição, e o
// resultado seriam dois fluxos no mesmo nível.
func (r *WorkflowRepo) Create(ctx context.Context, f *workflow.Flow, idempotencyKey string) (*workflow.Flow, error) {
	var saved *workflow.Flow
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		id, created, err := insertFlowRow(ctx, tx, f, idempotencyKey)
		if err != nil {
			return err
		}
		if !created {
			saved, err = flowByKey(ctx, tx, idempotencyKey)
			return err
		}
		if _, err := insertVersion(ctx, tx, id, 1, f, idempotencyKey+":1"); err != nil {
			return err
		}
		saved, err = loadFlow(ctx, tx, f.AccountID, id)
		if err != nil {
			return err
		}
		return Emit(ctx, tx, flowEvent("dop.workflow.flow.created", saved))
	})
	return saved, err
}

// AppendVersion grava a versão seguinte SEM tocar na anterior — a trigger do
// banco recusaria de qualquer forma, e é bom que recuse: a proteção precisa
// valer também para o caminho que ninguém previu.
func (r *WorkflowRepo) AppendVersion(ctx context.Context, accountID string, f *workflow.Flow, baseVersion int32, idempotencyKey string) (*workflow.Flow, error) {
	var saved *workflow.Flow
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// FOR UPDATE serializa duas edições simultâneas: sem a trava, as duas
		// leriam a versão 3 e as duas tentariam gravar a 4.
		var current int32
		err := tx.QueryRow(ctx,
			`SELECT current_version FROM flows WHERE id = $1 AND account_id = $2 FOR UPDATE`,
			f.ID, accountID).Scan(&current)
		if err != nil {
			return Translate(err, "fluxo")
		}
		if current != baseVersion {
			return errs.Conflict("o fluxo já está na versão %d; a alteração foi escrita sobre a %d",
				current, baseVersion)
		}

		next := current + 1
		created, err := insertVersion(ctx, tx, f.ID, next, f, idempotencyKey)
		if err != nil {
			return err
		}
		if !created {
			// Reenvio idêntico: devolve a versão que já foi gravada.
			saved, err = flowVersionByKey(ctx, tx, idempotencyKey)
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE flows SET current_version = $3, updated_at = $4 WHERE id = $1 AND account_id = $2`,
			f.ID, accountID, next, f.UpdatedAt); err != nil {
			return Translate(err, "fluxo")
		}
		saved, err = loadFlow(ctx, tx, accountID, f.ID)
		if err != nil {
			return err
		}
		return Emit(ctx, tx, flowEvent("dop.workflow.flow.version.created", saved))
	})
	return saved, err
}

// Promote publica o conteúdo no nível alvo. Se o alvo já tem fluxo, o conteúdo
// entra como versão NOVA dele — o fluxo do alvo não é substituído, é versionado,
// pelo mesmo motivo de sempre: alguém pode estar seguindo a versão atual.
func (r *WorkflowRepo) Promote(ctx context.Context, accountID string, src *workflow.Flow, target workflow.ScopeRef, idempotencyKey string) (*workflow.Flow, error) {
	var saved *workflow.Flow
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			targetID string
			current  int32
		)
		err := tx.QueryRow(ctx, `
			SELECT id, current_version FROM flows
			 WHERE account_id = $1 AND owner_scope = $2::flow_scope AND owner_id = $3
			 FOR UPDATE`, accountID, string(target.Scope), target.ID).Scan(&targetID, &current)

		promoted := *src
		promoted.AccountID = accountID
		promoted.OwnerScope, promoted.OwnerID = target.Scope, target.ID

		switch {
		case NoRows(err):
			promoted.ID = ""
			promoted.Version = 1
			promoted.CreatedAt = src.UpdatedAt
			id, created, err := insertFlowRow(ctx, tx, &promoted, idempotencyKey)
			if err != nil {
				return err
			}
			if !created {
				saved, err = flowByKey(ctx, tx, idempotencyKey)
				return err
			}
			if _, err := insertVersion(ctx, tx, id, 1, &promoted, idempotencyKey+":1"); err != nil {
				return err
			}
			targetID = id
		case err != nil:
			return Translate(err, "fluxo do nível de destino")
		default:
			promoted.ID = targetID
			promoted.Version = current + 1
			created, err := insertVersion(ctx, tx, targetID, promoted.Version, &promoted, idempotencyKey)
			if err != nil {
				return err
			}
			if !created {
				saved, err = flowVersionByKey(ctx, tx, idempotencyKey)
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE flows SET current_version = $3, updated_at = $4 WHERE id = $1 AND account_id = $2`,
				targetID, accountID, promoted.Version, src.UpdatedAt); err != nil {
				return Translate(err, "fluxo do nível de destino")
			}
		}

		saved, err = loadFlow(ctx, tx, accountID, targetID)
		if err != nil {
			return err
		}
		ev := flowEvent("dop.workflow.flow.promoted", saved)
		ev.Payload = mustJSON(map[string]any{
			"name": saved.Name, "version": saved.Version,
			"owner_scope": string(saved.OwnerScope), "owner_id": saved.OwnerID,
			// A procedência da promoção: sem ela, ninguém explica de onde veio
			// o fluxo que passou a valer para o nível inteiro.
			"promoted_from_flow_id": src.ID,
			"promoted_from_scope":   string(src.OwnerScope),
			"promoted_from_owner":   src.OwnerID,
			"promoted_from_version": src.Version,
		})
		return Emit(ctx, tx, ev)
	})
	return saved, err
}

// ── linhagem (workflow.Ancestry) ─────────────────────────────────────────────

// ChainOf devolve a cadeia de níveis acima do alvo, do mais genérico ao mais
// específico. Cada salto filtra por conta: id de outra conta simplesmente não
// existe daqui, e volta como NotFound.
func (r *WorkflowRepo) ChainOf(ctx context.Context, accountID string, target workflow.ScopeRef) ([]workflow.ScopeRef, error) {
	base := []workflow.ScopeRef{{Scope: workflow.ScopePlatform}}
	switch target.Scope {
	case workflow.ScopePlatform:
		return base, nil
	case workflow.ScopeAccount:
		if target.ID != accountID {
			return nil, errs.NotFound("conta %s", target.ID)
		}
		return append(base, workflow.ScopeRef{Scope: workflow.ScopeAccount, ID: accountID}), nil
	}
	base = append(base, workflow.ScopeRef{Scope: workflow.ScopeAccount, ID: accountID})

	switch target.Scope {
	case workflow.ScopeWorkspace:
		var id string
		if err := r.pool.QueryRow(ctx,
			`SELECT id FROM workspaces WHERE id = $1 AND account_id = $2`,
			target.ID, accountID).Scan(&id); err != nil {
			return nil, Translate(err, "workspace")
		}
		return append(base, workflow.ScopeRef{Scope: workflow.ScopeWorkspace, ID: id}), nil

	case workflow.ScopeProject:
		var workspaceID string
		if err := r.pool.QueryRow(ctx,
			`SELECT workspace_id FROM projects WHERE id = $1 AND account_id = $2`,
			target.ID, accountID).Scan(&workspaceID); err != nil {
			return nil, Translate(err, "projeto")
		}
		return append(base,
			workflow.ScopeRef{Scope: workflow.ScopeWorkspace, ID: workspaceID},
			workflow.ScopeRef{Scope: workflow.ScopeProject, ID: target.ID}), nil

	case workflow.ScopeDemand:
		return r.demandChain(ctx, accountID, base, target.ID)
	}
	return nil, errs.Invalid("nível desconhecido: %q", target.Scope)
}

// demandChain sobe da demanda até a conta.
//
// A tabela de demandas chega em migração de OUTRO domínio, que ainda não
// aterrissou neste banco. Perguntar pela existência antes de consultar troca um
// "relation does not exist" (que chega ao usuário como falha interna) por uma
// recusa que diz o que está faltando — e some sozinha quando a tabela nascer.
func (r *WorkflowRepo) demandChain(ctx context.Context, accountID string, base []workflow.ScopeRef, demandID string) ([]workflow.ScopeRef, error) {
	var ready bool
	if err := r.pool.QueryRow(ctx,
		`SELECT to_regclass('public.demands') IS NOT NULL`).Scan(&ready); err != nil {
		return nil, Translate(err, "demanda")
	}
	if !ready {
		return nil, errs.Precondition(
			"o domínio de demanda ainda não existe neste banco: resolva o fluxo pelo projeto até a migração de demanda ser aplicada")
	}
	var projectID, workspaceID string
	if err := r.pool.QueryRow(ctx, `
		SELECT d.project_id, p.workspace_id
		  FROM demands d JOIN projects p ON p.id = d.project_id
		 WHERE d.id = $1 AND d.account_id = $2`, demandID, accountID).
		Scan(&projectID, &workspaceID); err != nil {
		return nil, Translate(err, "demanda")
	}
	return append(base,
		workflow.ScopeRef{Scope: workflow.ScopeWorkspace, ID: workspaceID},
		workflow.ScopeRef{Scope: workflow.ScopeProject, ID: projectID},
		workflow.ScopeRef{Scope: workflow.ScopeDemand, ID: demandID}), nil
}

// ── auxiliares ───────────────────────────────────────────────────────────────

// insertFlowRow grava a identidade do fluxo. created=false significa que a
// chave de idempotência já tinha sido usada — repetição, não erro.
func insertFlowRow(ctx context.Context, tx pgx.Tx, f *workflow.Flow, key string) (string, bool, error) {
	var accountID, ownerID any
	if f.AccountID != "" {
		accountID = f.AccountID
	}
	if f.OwnerID != "" {
		ownerID = f.OwnerID
	}
	var createdBy any
	if f.CreatedBy != "" {
		createdBy = f.CreatedBy
	}

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO flows (account_id, owner_scope, owner_id, current_version,
		                   idempotency_key, created_by, created_at, updated_at)
		VALUES ($1, $2::flow_scope, $3, 1, $4, $5, $6, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		accountID, string(f.OwnerScope), ownerID, key, createdBy, f.CreatedAt).Scan(&id)
	if NoRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, Translate(err, "fluxo")
	}
	return id, true, nil
}

func insertVersion(ctx context.Context, tx pgx.Tx, flowID string, version int32, f *workflow.Flow, key string) (bool, error) {
	var createdBy any
	if f.CreatedBy != "" {
		createdBy = f.CreatedBy
	}
	var got int32
	err := tx.QueryRow(ctx, `
		INSERT INTO flow_versions (flow_id, version, name, description, stages,
		                           idempotency_key, created_by, created_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING version`,
		flowID, version, f.Name, f.Description, encodeStages(f.Stages), key, createdBy, f.UpdatedAt).
		Scan(&got)
	if NoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, Translate(err, "versão do fluxo")
	}
	return true, nil
}

func loadFlow(ctx context.Context, tx pgx.Tx, accountID, id string) (*workflow.Flow, error) {
	f, err := scanFlow(tx.QueryRow(ctx,
		`SELECT `+flowCols+currentJoin+visibleToAccount+` AND f.id = $2`, accountID, id))
	if err != nil {
		return nil, Translate(err, "fluxo")
	}
	return f, nil
}

// flowByKey e flowVersionByKey são o retorno da repetição: a chave já gravada
// aponta para o que o chamador queria criar, e devolver isso é o que torna a
// escrita idempotente de verdade (ADR-0017).
func flowByKey(ctx context.Context, tx pgx.Tx, key string) (*workflow.Flow, error) {
	f, err := scanFlow(tx.QueryRow(ctx, `SELECT `+flowCols+currentJoin+
		` WHERE f.idempotency_key = $1`, key))
	if err != nil {
		return nil, Translate(err, "fluxo")
	}
	return f, nil
}

func flowVersionByKey(ctx context.Context, tx pgx.Tx, key string) (*workflow.Flow, error) {
	f, err := scanFlow(tx.QueryRow(ctx, `SELECT `+flowCols+
		` FROM flows f JOIN flow_versions v ON v.flow_id = f.id
		  WHERE v.idempotency_key = $1`, key))
	if err != nil {
		return nil, Translate(err, "versão do fluxo")
	}
	return f, nil
}

func flowEvent(kind string, f *workflow.Flow) ports.Event {
	return ports.Event{
		AccountID: f.AccountID, Aggregate: "workflow", AggregateID: f.ID, Type: kind,
		Payload: mustJSON(map[string]any{
			"name": f.Name, "version": f.Version,
			"owner_scope": string(f.OwnerScope), "owner_id": f.OwnerID,
			"stages": len(f.Stages),
		}),
	}
}

// stageDoc é a forma no banco. Existe separada do tipo de domínio de propósito:
// renomear um campo do domínio não pode reescrever silenciosamente o JSON de
// versões que já estão congeladas há meses.
type stageDoc struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Artifacts []string `json:"artifacts"`
	Gate      string   `json:"gate"`
	Subtypes  []string `json:"subtypes"`
}

func encodeStages(stages []workflow.StageSpec) []byte {
	docs := make([]stageDoc, 0, len(stages))
	for _, st := range stages {
		artifacts := make([]string, 0, len(st.Artifacts))
		for _, a := range st.Artifacts {
			artifacts = append(artifacts, string(a))
		}
		subtypes := st.Subtypes
		if subtypes == nil {
			subtypes = []string{}
		}
		docs = append(docs, stageDoc{
			Key: st.Key, Name: st.Name, Type: string(st.Type),
			Artifacts: artifacts, Gate: string(st.Gate), Subtypes: subtypes,
		})
	}
	return mustJSON(docs)
}

func decodeStages(raw []byte) []workflow.StageSpec {
	if len(raw) == 0 {
		return nil
	}
	var docs []stageDoc
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil
	}
	out := make([]workflow.StageSpec, 0, len(docs))
	for _, d := range docs {
		artifacts := make([]workflow.ArtifactKind, 0, len(d.Artifacts))
		for _, a := range d.Artifacts {
			artifacts = append(artifacts, workflow.ArtifactKind(a))
		}
		out = append(out, workflow.StageSpec{
			Key: d.Key, Name: d.Name, Type: workflow.StageType(d.Type),
			Artifacts: artifacts, Gate: workflow.GateKind(d.Gate), Subtypes: d.Subtypes,
		})
	}
	return out
}

var (
	_ workflow.Repository = (*WorkflowRepo)(nil)
	_ workflow.Ancestry   = (*WorkflowRepo)(nil)
)
