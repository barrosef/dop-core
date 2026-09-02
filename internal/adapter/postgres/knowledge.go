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

// KnowledgeRepo implements knowledge.Repository. It is the ONLY place with
// knowledge SQL — the domain never sees a query.
//
// Two things hold for this whole file:
//
//   - account_id opens EVERY WHERE clause, the search ones included. Knowledge
//     crossing from one account into another is the worst defect conceivable on
//     this platform, and the isolation is a constraint of the query, not trust
//     in the caller;
//   - the write stores state and event in the SAME transaction, through InTx +
//     Emit, with the idempotency mark closed inside it (ADR-0017/0019).
type KnowledgeRepo struct{ pool *pgxpool.Pool }

func NewKnowledgeRepo(pool *pgxpool.Pool) *KnowledgeRepo { return &KnowledgeRepo{pool: pool} }

// The embedding column is left out of the read on purpose: it is 1536 floats
// per row that nobody consumes outside the index — bringing them would mean
// paying for the whole vector on every package assembly.
const knowledgeCols = `k.id, k.account_id, k.scope::text,
	COALESCE(k.workspace_id::text,''), COALESCE(k.project_id::text,''),
	k.kind::text, k.name, k.version, k.body, k.object_ref, k.size_bytes,
	k.est_tokens, k.meta, COALESCE(k.created_by::text,''), k.created_at, k.updated_at`

// knowledgeColsUpsert is the same list without the table's alias: the upsert's
// RETURNING sees the table by name, not by the SELECT's alias.
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

// scopeReach is INHERITANCE's predicate, written ONCE.
//
// What reaches the project is everything in the project, in the workspace that
// contains it and in the account. The workspace is resolved by a subquery
// filtered by the account — without that filter, a project id from another
// account would bring that account's workspace rules. $1 is the account; $2 is
// the project (empty = the account scope only).
const scopeReach = `(
	   k.scope = 'account'
	OR (k.scope = 'project'   AND k.project_id = NULLIF($2,'')::uuid)
	OR (k.scope = 'workspace' AND k.workspace_id = (
	      SELECT p.workspace_id FROM projects p
	       WHERE p.id = NULLIF($2,'')::uuid AND p.account_id = $1))
)`

// RulesFor brings the CANDIDATES; the inheritance's precedence is resolved in
// the domain (knowledge.ResolveRules), so that the "the most specific wins" rule
// exists in a single place.
func (r *KnowledgeRepo) RulesFor(ctx context.Context, accountID, projectID string) ([]knowledge.Artifact, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+knowledgeCols+`
		  FROM knowledge_artifacts k
		 WHERE k.account_id = $1 AND k.kind = 'rule' AND `+scopeReach+`
		 ORDER BY k.name`, accountID, projectID)
	if err != nil {
		return nil, Translate(err, "rules")
	}
	defer rows.Close()
	return collectArtifacts(rows, "rules")
}

// IndexFor brings the index OF THE REQUESTED REPOSITORIES. An empty list
// returns nothing — and not "everything", which is the mistake that would turn
// the context package into a dump of the whole project.
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
		return nil, Translate(err, "the index")
	}
	defer rows.Close()
	return collectArtifacts(rows, "the index")
}

// IndexOf returns (nil, nil) when there is no map: whether the absence is an
// error is the use case's decision.
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
		return nil, Translate(err, "the index")
	}
	return a, nil
}

// SearchMemory has two paths, and the domain chooses which by supplying (or not)
// a vector in the query.
//
// SEMANTIC: cosine distance with the HNSW index. The score returned is
// 1 - distance, so that "higher is better" holds on both paths — a score that
// flips meaning depending on the path is an invitation to an ordering bug.
//
// LEXICAL: word_similarity, not similarity. The query is short and the memory's
// body is long; similarity() normalizes by the whole text and sinks for any
// large document, returning zero exactly where there is the most content.
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
		return nil, Translate(err, "memory")
	}
	defer rows.Close()

	var out []knowledge.ScoredArtifact
	for rows.Next() {
		var score float32
		a, err := scanArtifact(rows, &score)
		if err != nil {
			return nil, Translate(err, "memory")
		}
		out = append(out, knowledge.ScoredArtifact{Artifact: *a, Score: score})
	}
	return out, rows.Err()
}

// Put writes the artifact, the idempotency mark and the event in the SAME
// transaction.
//
// The upsert is over (account_id, kind, scope_id, name): rewriting the same name
// in the same scope BUMPS the version. Knowledge is content, and content is
// versioned — somebody will need to know which version of the index that demand
// ran with.
func (r *KnowledgeRepo) Put(ctx context.Context, a *knowledge.Artifact, id knowledge.Idempotency) (*knowledge.Artifact, error) {
	var saved *knowledge.Artifact
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// The reservation happens INSIDE the transaction: two simultaneous
		// writes with the same key contend for the idempotency row, and the
		// second waits instead of writing one more version.
		if stored, done, err := reserveIdempotency(ctx, tx, id); err != nil {
			return err
		} else if done {
			var previous struct {
				ArtifactID string `json:"artifact_id"`
			}
			if err := json.Unmarshal(stored, &previous); err == nil && previous.ArtifactID != "" {
				saved, err = loadArtifact(ctx, tx, a.Scope.AccountID, previous.ArtifactID)
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
			       -- A new vector only replaces the old one if it exists:
			       -- erasing the embedding because the service was down would
			       -- take the memory out of semantic search in silence.
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
			return Translate(err, "knowledge artifact")
		}

		if err := Emit(ctx, tx, ports.Event{
			AccountID: saved.AccountID(), Aggregate: "knowledge", AggregateID: saved.ID,
			Type: "dop.knowledge.artifact.put",
			Payload: mustJSON(map[string]any{
				"kind": saved.Kind, "name": saved.Name, "scope": saved.Scope.Level,
				"project_id": saved.Scope.ProjectID, "version": saved.Version,
				// The reference, never the content: the event log is read by a
				// lot of people and is no place to keep a document.
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

// RecordContextBuild publishes the assembly's MEASUREMENT (ADR-0009 §3,
// ADR-0012 §1).
//
// It changes no state: it emits. It still goes through InTx, because Emit writes
// the event and the outbox and the two have to go in together — it is the same
// mechanism, not a shortcut. The event belongs to the DEMAND aggregate: whoever
// investigates an agent that did not know what it should have known reads the
// demand's timeline, not knowledge's.
func (r *KnowledgeRepo) RecordContextBuild(ctx context.Context, accountID, demandID string, m knowledge.PackageMetrics) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		return Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "demand", AggregateID: demandID,
			Type: "dop.knowledge.context.built", OccurredAt: m.At,
			Payload: mustJSON(map[string]any{
				"estimated_tokens": m.EstimatedTokens, "budget": m.Budget,
				"rules": m.Rules, "findings": m.Findings,
				"index": m.Index, "memories": m.Memories,
				// Truncated is the number that becomes an alert: a ceiling hit
				// often means a demand that is too large or a badly pruned
				// memory.
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
		return nil, Translate(err, "knowledge artifact")
	}
	return a, nil
}

// reserveIdempotency reserves the key INSIDE the use case's transaction.
//
// The same semantics as postgres.Idempotency.Begin, but over the tx: the
// completion mark only comes to exist if the whole write is committed, and the
// key repeated with different CONTENT is a conflict — otherwise a client bug
// reusing a key would become silent corruption of the knowledge base.
func reserveIdempotency(ctx context.Context, tx pgx.Tx, id knowledge.Idempotency) ([]byte, bool, error) {
	if id.Key == "" {
		return nil, false, nil // no key: it goes on unprotected
	}
	var storedHash string
	var stored []byte
	var completed *string
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency (key, request_hash, expires_at)
		VALUES ($1, $2, now() + interval '24 hours')
		ON CONFLICT (key) DO UPDATE SET key = EXCLUDED.key
		RETURNING request_hash, response, completed_at::text`,
		id.Key, id.RequestHash).Scan(&storedHash, &stored, &completed)
	if err != nil {
		return nil, false, errs.Wrap(errs.KindInternal, err, "failure in the idempotency control")
	}
	if storedHash != id.RequestHash {
		return nil, false, errs.Conflict("the idempotency key has already been used with different content")
	}
	return stored, completed != nil && len(stored) > 0, nil
}

// vectorLiteral serializes the embedding into pgvector's textual format.
//
// It goes as TEXT and is converted by a cast in the query ($n::vector): the
// driver does not know the `vector` type, and registering a codec just for this
// would couple the whole pool to an extension. An empty string becomes NULL
// through the NULLIF — an artifact with no embedding is the normal case when
// there is no Embedder wired.
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
