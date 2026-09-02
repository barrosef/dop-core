-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Knowledge: ADR-0009's three layers in ONE table.
--
-- A rule, an index and a memory are the same thing from storage's point of
-- view — versioned text, with a scope and an owner — and differ in their CYCLE
-- (who writes them, when, and how they are searched). Three tables would force
-- repeating the scope, the version, the per-account isolation and the
-- body/object_ref pair three times, and assembling the package — which reads
-- all three — would become three queries with the same WHERE. The kind is a
-- column; what changes per layer is the INDEX, and it is down there that the
-- difference shows up.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TYPE knowledge_kind  AS ENUM ('rule', 'index', 'memory');
CREATE TYPE knowledge_scope AS ENUM ('account', 'workspace', 'project');

CREATE TABLE knowledge_artifacts (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,

  -- Scope and inheritance: an account's rule holds for every project of its
  -- own, and a project may replace it with a rule of the SAME NAME. Without the
  -- three levels, the alternative would be copying the house policy into every
  -- project — which is how a knowledge base rots.
  scope        knowledge_scope NOT NULL,
  workspace_id uuid REFERENCES workspaces(id) ON DELETE CASCADE,
  project_id   uuid REFERENCES projects(id) ON DELETE CASCADE,
  -- The scope's identity in ONE column. It exists so the uniqueness and the
  -- upsert's ON CONFLICT do not depend on a COALESCE repeated in every query —
  -- a duplicated expression is an expression that eventually diverges.
  scope_id     uuid GENERATED ALWAYS AS (COALESCE(project_id, workspace_id, account_id)) STORED,

  kind         knowledge_kind NOT NULL,
  -- name is the natural key within the scope. For kind='index' it is the
  -- REPOSITORY'S NAME: that is how ReadIndex(project, repo) finds the map
  -- without an extra column that would only have a value for one of the three
  -- kinds.
  name         text NOT NULL,
  version      int NOT NULL DEFAULT 1,

  -- The content lives in ONE of the two, never in both:
  --   body       — small, in Postgres, because it is what is indexable (vector
  --                and trigram) and readable without a second network round trip;
  --   object_ref — large, in the ObjectStore through the port, with the row
  --                keeping only the reference. A 4 MB map inside the row would
  --                turn every read of this table into a 4 MB read.
  body         text NOT NULL DEFAULT '',
  object_ref   text NOT NULL DEFAULT '',
  size_bytes   int  NOT NULL DEFAULT 0,
  -- The cost estimated in tokens, measured on WRITE. It is what lets the
  -- package's budget cut without opening each candidate's content (ADR-0012).
  est_tokens   int  NOT NULL DEFAULT 0,

  -- 1536 dimensions: the usual size of general-purpose text embeddings.
  -- Changing model means migrating the column AND reindexing the whole memory —
  -- the old vectors are not comparable with the new ones.
  embedding    vector(1536),

  meta         jsonb NOT NULL DEFAULT '{}',
  created_by   uuid REFERENCES users(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT knowledge_escopo_coerente CHECK (
    (scope = 'account'   AND workspace_id IS NULL     AND project_id IS NULL) OR
    (scope = 'workspace' AND workspace_id IS NOT NULL AND project_id IS NULL) OR
    (scope = 'project'   AND workspace_id IS NULL     AND project_id IS NOT NULL)),
  -- Content in two places is content that diverges.
  CONSTRAINT knowledge_conteudo_em_um_lugar CHECK (NOT (body <> '' AND object_ref <> '')),
  -- A rule is text the agent reads WHOLE, in every package: it cannot sit
  -- behind a reference the assembly would have to go and fetch.
  CONSTRAINT knowledge_regra_e_inline CHECK (kind <> 'rule' OR object_ref = '')
);

-- The artifact's identity. It is also the upsert's target: rewriting the same
-- name in the same scope BUMPS the version instead of creating a duplicate row.
CREATE UNIQUE INDEX knowledge_artifacts_ident_idx
  ON knowledge_artifacts (account_id, kind, scope_id, name);

-- Reading by project (the demand's index, the project's memory). account_id
-- comes first in EVERY index because it comes first in every query: knowledge
-- leaking between accounts is the worst possible defect in this platform.
CREATE INDEX knowledge_artifacts_project_idx
  ON knowledge_artifacts (account_id, project_id, kind)
  WHERE project_id IS NOT NULL;

-- ── semantic search over the memory (pgvector) ──────────────────────────────
--
-- HNSW and not IVFFlat, for a reason that belongs to this domain and not to a
-- benchmark: IVFFlat partitions the space into lists TRAINED on the existing
-- data, and the knowledge base is born EMPTY and grows with every demand that
-- closes. Lists built today would be built on nothing, and recall would fall as
-- the memory arrived — requiring a periodic REINDEX nobody will remember to
-- schedule. HNSW builds the graph incrementally, with no training, and answers
-- well from the very first row.
--
-- The price is real: more memory and slower writes. It fits here because the
-- profile is the opposite of expensive — memory is written when a demand CLOSES
-- (rare), and read when every context package is assembled (constant).
--
-- Partial on kind='memory': only the memory is searched by similarity. A rule
-- and an index are found by identity, and indexing them would cost a bigger
-- graph to answer a question nobody asks.
CREATE INDEX knowledge_artifacts_embedding_idx
  ON knowledge_artifacts USING hnsw (embedding vector_cosine_ops)
  WHERE kind = 'memory';

-- The LEXICAL path of the same search: it is what answers when no embedding
-- service is wired. A trigram over the name and the body — worse than semantic,
-- much better than returning nothing.
CREATE INDEX knowledge_artifacts_trgm_idx
  ON knowledge_artifacts USING gin ((name || ' ' || body) gin_trgm_ops)
  WHERE kind = 'memory';

-- +goose Down
DROP TABLE IF EXISTS knowledge_artifacts;
DROP TYPE IF EXISTS knowledge_scope;
DROP TYPE IF EXISTS knowledge_kind;
