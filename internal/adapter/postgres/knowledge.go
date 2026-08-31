package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// KnowledgeRepo implementa knowledge.Repository. É o ÚNICO lugar com SQL de
// conhecimento — o domínio nunca vê uma query.
//
// Duas coisas valem por toda a leitura deste arquivo:
//
//   - account_id abre TODA cláusula WHERE, inclusive as de busca. Conhecimento
//     que atravessa de uma conta para outra é o pior defeito concebível nesta
//     plataforma, e o isolamento é constraint da query, não confiança no
//     chamador;
//   - a escrita grava estado e evento na MESMA transação, por InTx + Emit, com
//     a marca de idempotência fechada dentro dela (ADR-0017/0019).
type KnowledgeRepo struct{ pool *pgxpool.Pool }

func NewKnowledgeRepo(pool *pgxpool.Pool) *KnowledgeRepo { return &KnowledgeRepo{pool: pool} }

// A coluna embedding fica de fora da leitura de propósito: são 1536 floats por
// linha que ninguém consome fora do índice — trazê-los seria pagar o vetor
// inteiro em toda montagem de pacote.
const knowledgeCols = `k.id, k.account_id, k.scope::text,
	COALESCE(k.workspace_id::text,''), COALESCE(k.project_id::text,''),
	k.kind::text, k.name, k.version, k.body, k.object_ref, k.size_bytes,
	k.est_tokens, k.meta, COALESCE(k.created_by::text,''), k.created_at, k.updated_at`

// knowledgeColsUpsert é a mesma lista sem o apelido da tabela: o RETURNING do
// upsert enxerga a tabela pelo nome, não pelo alias do SELECT.
var knowledgeColsUpsert = strings.ReplaceAll(knowledgeCols, "k.", "knowledge_artifacts.")

func scanArtifact(row pgx.Row, extra ...any) (*knowledge.Artifact, error) {
	var a knowledge.Artifact
	var scope, kind string
	var meta []byte
	dest := append([]any{
		&a.ID, &a.Scope.AccountID, &scope, &a.Scope.WorkspaceID, &a.Scope.ProjectID,
		&kind, &a.Name, &a.Version, &a.Body, &a.ObjectRef, &a.SizeBytes,
		&a.EstTokens, &meta, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt,
	}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	a.Scope.Level = knowledge.ScopeLevel(scope)
	a.Kind = knowledge.Kind(kind)
	a.Meta = map[string]any{}
	_ = json.Unmarshal(meta, &a.Meta)
	return &a, nil
}

// scopeReach é o predicado da HERANÇA, escrito UMA vez.
//
// Alcança o projeto tudo que está no projeto, no workspace que o contém e na
// conta. O workspace é resolvido por subconsulta filtrada pela conta — sem
// esse filtro, um id de projeto de outra conta traria as regras do workspace
// dela. $1 é a conta; $2 é o projeto (vazio = só o escopo de conta).
const scopeReach = `(
	   k.scope = 'account'
	OR (k.scope = 'project'   AND k.project_id = NULLIF($2,'')::uuid)
	OR (k.scope = 'workspace' AND k.workspace_id = (
	      SELECT p.workspace_id FROM projects p
	       WHERE p.id = NULLIF($2,'')::uuid AND p.account_id = $1))
)`

// RulesFor traz as CANDIDATAS; a precedência da herança é resolvida no domínio
// (knowledge.ResolveRules), para que a regra de "o mais específico ganha"
// exista em um lugar só.
func (r *KnowledgeRepo) RulesFor(ctx context.Context, accountID, projectID string) ([]knowledge.Artifact, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+knowledgeCols+`
		  FROM knowledge_artifacts k
		 WHERE k.account_id = $1 AND k.kind = 'rule' AND `+scopeReach+`
		 ORDER BY k.name`, accountID, projectID)
	if err != nil {
		return nil, Translate(err, "regras")
	}
	defer rows.Close()
	return collectArtifacts(rows, "regras")
}

// IndexFor traz o índice DOS REPOSITÓRIOS PEDIDOS. Lista vazia devolve nada —
// e não "tudo", que é o erro que transformaria o pacote de contexto num
// despejo do projeto inteiro.
func (r *KnowledgeRepo) IndexFor(ctx context.Context, accountID, projectID string, repos []string) ([]knowledge.Artifact, error) {
	if len(repos) == 0 || projectID == "" {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+knowledgeCols+`
		  FROM knowledge_artifacts k
		 WHERE k.account_id = $1 AND k.project_id = $2::uuid
		   AND k.kind = 'index' AND k.name = ANY($3::text[])
		 ORDER BY k.name`, accountID, projectID, repos)
	if err != nil {
		return nil, Translate(err, "índice")
	}
	defer rows.Close()
	return collectArtifacts(rows, "índice")
}

// IndexOf devolve (nil, nil) quando não há mapa: quem decide se a ausência é
// erro é o caso de uso.
func (r *KnowledgeRepo) IndexOf(ctx context.Context, accountID, projectID, repo string) (*knowledge.Artifact, error) {
	a, err := scanArtifact(r.pool.QueryRow(ctx, `
		SELECT `+knowledgeCols+`
		  FROM knowledge_artifacts k
		 WHERE k.account_id = $1 AND k.project_id = $2::uuid
		   AND k.kind = 'index' AND k.name = $3`, accountID, projectID, repo))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "índice")
	}
	return a, nil
}

// SearchMemory tem dois caminhos, e o domínio escolhe qual pedindo (ou não) um
// vetor na consulta.
//
// SEMÂNTICO: distância de cosseno com o índice HNSW. O score devolvido é
// 1 - distância, para que "maior é melhor" valha nos dois caminhos — score que
// inverte de sentido conforme o caminho é convite a bug na ordenação.
//
// LEXICAL: word_similarity, não similarity. A consulta é curta e o corpo da
// memória é longo; similarity() normaliza pelo texto inteiro e afunda para
// qualquer documento grande, devolvendo zero justamente onde há mais conteúdo.
func (r *KnowledgeRepo) SearchMemory(ctx context.Context, q knowledge.MemoryQuery) ([]knowledge.ScoredArtifact, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}

	var rows pgx.Rows
	var err error
	if len(q.Embedding) > 0 {
		rows, err = r.pool.Query(ctx, `
			SELECT `+knowledgeCols+`, (1 - (k.embedding <=> $3::vector))::float4
			  FROM knowledge_artifacts k
			 WHERE k.account_id = $1 AND k.kind = 'memory'
			   AND k.embedding IS NOT NULL AND `+scopeReach+`
			 ORDER BY k.embedding <=> $3::vector
			 LIMIT $4`, q.AccountID, q.ProjectID, vectorLiteral(q.Embedding), limit)
	} else {
		rows, err = r.pool.Query(ctx, `
			SELECT `+knowledgeCols+`, word_similarity($3, k.name || ' ' || k.body)::float4
			  FROM knowledge_artifacts k
			 WHERE k.account_id = $1 AND k.kind = 'memory'
			   AND $3 <% (k.name || ' ' || k.body) AND `+scopeReach+`
			 ORDER BY word_similarity($3, k.name || ' ' || k.body) DESC, k.id
			 LIMIT $4`, q.AccountID, q.ProjectID, q.Text, limit)
	}
	if err != nil {
		return nil, Translate(err, "memória")
	}
	defer rows.Close()

	var out []knowledge.ScoredArtifact
	for rows.Next() {
		var score float32
		a, err := scanArtifact(rows, &score)
		if err != nil {
			return nil, Translate(err, "memória")
		}
		out = append(out, knowledge.ScoredArtifact{Artifact: *a, Score: score})
	}
	return out, rows.Err()
}

// Put grava o artefato, a marca de idempotência e o evento na MESMA transação.
//
// O upsert é sobre (account_id, kind, scope_id, name): regravar o mesmo nome no
// mesmo escopo BUMPA a versão. Conhecimento é conteúdo, e conteúdo é
// versionado — alguém vai precisar saber com qual versão do índice aquela
// demanda rodou.
func (r *KnowledgeRepo) Put(ctx context.Context, a *knowledge.Artifact, id knowledge.Idempotency) (*knowledge.Artifact, error) {
	var saved *knowledge.Artifact
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// A reserva acontece DENTRO da transação: duas escritas simultâneas
		// com a mesma chave disputam a linha de idempotência, e a segunda
		// espera em vez de gravar uma versão a mais.
		if resposta, pronto, err := reserveIdempotency(ctx, tx, id); err != nil {
			return err
		} else if pronto {
			var gravado struct {
				ArtifactID string `json:"artifact_id"`
			}
			if err := json.Unmarshal(resposta, &gravado); err == nil && gravado.ArtifactID != "" {
				saved, err = loadArtifact(ctx, tx, a.Scope.AccountID, gravado.ArtifactID)
				return err
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO knowledge_artifacts
			   (account_id, scope, workspace_id, project_id, kind, name,
			    body, object_ref, size_bytes, est_tokens, embedding, meta, created_by)
			VALUES ($1, $2::knowledge_scope, NULLIF($3,'')::uuid, NULLIF($4,'')::uuid,
			        $5::knowledge_kind, $6, $7, $8, $9, $10, NULLIF($11,'')::vector,
			        $12, NULLIF($13,'')::uuid)
			ON CONFLICT (account_id, kind, scope_id, name) DO UPDATE
			   SET body       = EXCLUDED.body,
			       object_ref = EXCLUDED.object_ref,
			       size_bytes = EXCLUDED.size_bytes,
			       est_tokens = EXCLUDED.est_tokens,
			       -- Vetor novo só substitui o antigo se existir: apagar o
			       -- embedding porque o serviço estava fora tiraria a memória
			       -- da busca semântica em silêncio.
			       embedding  = COALESCE(EXCLUDED.embedding, knowledge_artifacts.embedding),
			       meta       = EXCLUDED.meta,
			       version    = knowledge_artifacts.version + 1,
			       updated_at = now()
			RETURNING `+knowledgeColsUpsert,
			a.Scope.AccountID, string(a.Scope.Level), a.Scope.WorkspaceID, a.Scope.ProjectID,
			string(a.Kind), a.Name, a.Body, a.ObjectRef, a.SizeBytes, a.EstTokens,
			vectorLiteral(a.Embedding), mustJSON(a.Meta), a.CreatedBy)

		var err error
		saved, err = scanArtifact(row)
		if err != nil {
			return Translate(err, "artefato de conhecimento")
		}

		if err := Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID(), Aggregate: "knowledge", AggregateID: saved.ID,
			Type: "dop.knowledge.artifact.put",
			Payload: mustJSON(map[string]any{
				"kind": saved.Kind, "name": saved.Name, "scope": saved.Scope.Level,
				"project_id": saved.Scope.ProjectID, "version": saved.Version,
				// A referência, nunca o conteúdo: o log de eventos é lido por
				// muita gente e não é lugar de guardar documento.
				"externalized": saved.Externalized(), "size_bytes": saved.SizeBytes,
				"est_tokens": saved.EstTokens,
			}),
		}); err != nil {
			return err
		}
		return CompleteTx(ctx, tx, id.Key, mustJSON(map[string]any{"artifact_id": saved.ID}))
	})
	return saved, err
}

// RecordContextBuild publica a MEDIÇÃO da montagem (ADR-0009 §3, ADR-0012 §1).
//
// Não muda estado: emite. Ainda assim vai por InTx, porque Emit grava evento e
// outbox e os dois precisam entrar juntos — é o mesmo mecanismo, não um atalho.
// O evento é do agregado DEMANDA: quem investiga um agente que não sabia o que
// devia saber lê a timeline da demanda, não a do conhecimento.
func (r *KnowledgeRepo) RecordContextBuild(ctx context.Context, accountID, demandID string, m knowledge.PackageMetrics) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		return Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "demand", AggregateID: demandID,
			Type: "dop.knowledge.context.built", OccurredAt: m.At,
			Payload: mustJSON(map[string]any{
				"estimated_tokens": m.EstimatedTokens, "budget": m.Budget,
				"rules": m.Rules, "findings": m.Findings,
				"index": m.Index, "memories": m.Memories,
				// Truncado é o número que vira alerta: teto batendo com
				// frequência é demanda grande demais ou memória mal podada.
				"truncated": m.Dropped.Any(),
				"dropped": map[string]any{
					"rules": m.Dropped.Rules, "findings": m.Dropped.Findings,
					"index": m.Dropped.Index, "memories": m.Dropped.Memories,
				},
			}),
		})
	})
}

// ── auxiliares ───────────────────────────────────────────────────────────────

func collectArtifacts(rows pgx.Rows, what string) ([]knowledge.Artifact, error) {
	var out []knowledge.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, Translate(err, what)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func loadArtifact(ctx context.Context, tx pgx.Tx, accountID, id string) (*knowledge.Artifact, error) {
	a, err := scanArtifact(tx.QueryRow(ctx, `
		SELECT `+knowledgeCols+`
		  FROM knowledge_artifacts k
		 WHERE k.account_id = $1 AND k.id = $2::uuid`, accountID, id))
	if err != nil {
		return nil, Translate(err, "artefato de conhecimento")
	}
	return a, nil
}

// reserveIdempotency reserva a chave DENTRO da transação do caso de uso.
//
// Mesma semântica de postgres.Idempotency.Begin, mas sobre a tx: a marca de
// concluído só passa a existir se a escrita inteira for confirmada, e a chave
// repetida com CONTEÚDO diferente é conflito — senão um bug de cliente
// reaproveitando chave viraria corrupção silenciosa da base de conhecimento.
func reserveIdempotency(ctx context.Context, tx pgx.Tx, id knowledge.Idempotency) ([]byte, bool, error) {
	if id.Key == "" {
		return nil, false, nil // sem chave: segue sem proteção
	}
	var hashGravado string
	var resposta []byte
	var concluido *string
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency (key, request_hash, expires_at)
		VALUES ($1, $2, now() + interval '24 hours')
		ON CONFLICT (key) DO UPDATE SET key = EXCLUDED.key
		RETURNING request_hash, response, completed_at::text`,
		id.Key, id.RequestHash).Scan(&hashGravado, &resposta, &concluido)
	if err != nil {
		return nil, false, errs.Wrap(errs.KindInternal, err, "falha no controle de idempotência")
	}
	if hashGravado != id.RequestHash {
		return nil, false, errs.Conflict("a chave de idempotência já foi usada com outro conteúdo")
	}
	return resposta, concluido != nil && len(resposta) > 0, nil
}

// vectorLiteral serializa o embedding no formato textual do pgvector.
//
// Vai como TEXTO e é convertido por cast na query ($n::vector): o driver não
// conhece o tipo `vector`, e registrar um codec só para isto acoplaria o pool
// inteiro a uma extensão. String vazia vira NULL pelo NULLIF — artefato sem
// embedding é o caso normal quando não há Embedder ligado.
func vectorLiteral(v []float32) string {
	if len(v) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

var _ knowledge.Repository = (*KnowledgeRepo)(nil)
