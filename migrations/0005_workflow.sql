-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- A dynamic, typed and inheritable workflow (ADR-0014).
--
-- Two decisions shape this schema, and both are invariants — not conventions:
--
--  1. A VERSION IS IMMUTABLE. `flows` keeps the flow's IDENTITY (who owns it,
--     which is the current version) and `flow_versions` keeps the CONTENT, one
--     row per version, which never changes after it is written. A demand under
--     way freezes (flow_id, version) when it starts: changing the row in place
--     would rewrite its past and the history would stop explaining what
--     actually happened. The trigger at the end of the file refuses the UPDATE.
--
--  2. ONE FLOW PER LEVEL. The chain platform ◁ account ◁ workspace ◁ project ◁
--     demand has to yield ONE effective flow. Two competing flows at the same
--     level would turn the resolution into an arbitrary choice — so the unique
--     index stops them from existing. Evolving a flow is versioning it; having
--     another flow is declaring one at a different level.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TYPE flow_scope AS ENUM ('platform', 'account', 'workspace', 'project', 'demand');

CREATE TABLE flows (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- NULL only in the platform catalogue (level 0), which has no owner and holds
  -- for every account. Everything else carries an account with an FK —
  -- multi-tenant isolation is a constraint, not a convention.
  account_id      uuid REFERENCES accounts(id) ON DELETE CASCADE,
  owner_scope     flow_scope NOT NULL,
  -- A POLYMORPHIC owner: it points at an account, a workspace, a project or a
  -- demand depending on the scope. No FK because there is no single table to
  -- reference — the domain is what checks the lineage, against the account's
  -- tree, before writing (see workflow.Service.resolveOwner).
  owner_id        uuid,
  current_version int NOT NULL DEFAULT 1,
  -- Every write carries an idempotency key (ADR-0017): repeating the creation
  -- collides here and returns the flow already created, instead of a twin.
  idempotency_key text NOT NULL UNIQUE,
  created_by      uuid REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),

  -- The platform catalogue and an account flow are mutually exclusive: either
  -- it has an owner and an account, or it is level 0 and has neither. The
  -- in-between state ("a workspace flow with no account") means nothing, and so
  -- it does not exist.
  CONSTRAINT flow_plataforma_sem_dono CHECK (
    (owner_scope =  'platform' AND account_id IS NULL     AND owner_id IS NULL) OR
    (owner_scope <> 'platform' AND account_id IS NOT NULL AND owner_id IS NOT NULL)
  ),
  -- An account flow owns itself: owner_id IS the account. Keeping another id
  -- here would open the door to a flow hanging off the wrong account.
  CONSTRAINT flow_conta_e_dona_de_si CHECK (
    owner_scope <> 'account' OR owner_id = account_id
  )
);

-- The "one flow per level" rule, as an index. COALESCE because NULL does not
-- collide in a unique index and the platform catalogue could be born twice.
CREATE UNIQUE INDEX flows_um_por_nivel
  ON flows (owner_scope, COALESCE(owner_id, '00000000-0000-0000-0000-000000000000'::uuid));

-- The resolution's query: the flows of the chain's five levels, in one scan.
CREATE INDEX flows_account_scope_idx ON flows (account_id, owner_scope);

CREATE TABLE flow_versions (
  flow_id         uuid NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
  version         int  NOT NULL,
  -- The name and description are FROZEN along with the stages: renaming a flow
  -- is also rewriting the past of whoever already adopted it.
  name            text NOT NULL,
  description     text,
  -- The stages, in order, as the author composed them. JSONB and not a child
  -- table because the version is an immutable DOCUMENT read whole: normalizing
  -- would bring the cost of a join and the temptation to edit a single stage —
  -- which is precisely what must not happen.
  stages          jsonb NOT NULL,
  idempotency_key text NOT NULL UNIQUE,
  created_by      uuid REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),

  -- (flow_id, version) is the key the demand freezes — and it is here that the
  -- demand domain's FK should point when it is born.
  PRIMARY KEY (flow_id, version),
  CONSTRAINT flow_version_positiva CHECK (version > 0),
  CONSTRAINT flow_version_tem_etapa CHECK (jsonb_typeof(stages) = 'array' AND jsonb_array_length(stages) > 0)
);

-- current_version points at a version that EXISTS. Without this, an application
-- bug would leave the flow pointing at nothing and the screen would open blank.
ALTER TABLE flows ADD CONSTRAINT flows_versao_corrente_existe
  FOREIGN KEY (id, current_version) REFERENCES flow_versions(flow_id, version)
  DEFERRABLE INITIALLY DEFERRED;

-- The freezing invariant, in the database.
--
-- An UPDATE is always refused: changing a flow means writing the next version,
-- and no path — not even one nobody foresaw — may rewrite the document a
-- running demand is following.
--
-- A DELETE is refused for as long as the flow exists. The exception is the
-- cascade: when the account or the whole flow goes, Postgres deletes the parent
-- BEFORE propagating to the children, and the orphaned version may go with it.
-- Without that distinction, deleting an account would become impossible — the
-- protection would turn into a bug.
CREATE OR REPLACE FUNCTION assert_flow_version_congelada() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF EXISTS (SELECT 1 FROM flows WHERE id = OLD.flow_id) THEN
      RAISE EXCEPTION 'versão de fluxo não se apaga: a demanda que a congelou perderia o próprio histórico';
    END IF;
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'versão de fluxo é imutável: alterar um fluxo GERA versão nova (ADR-0014 §4)';
END $$ LANGUAGE plpgsql;

CREATE TRIGGER flow_versions_imutaveis
  BEFORE UPDATE OR DELETE ON flow_versions
  FOR EACH ROW EXECUTE FUNCTION assert_flow_version_congelada();

-- ── the platform catalogue (ADR-0014 §8) ────────────────────────────────────
-- The chain's level 0, seeded here and not over RPC: it holds for EVERY
-- account, and no account should be able to write it. It is what guarantees
-- ResolveFlow always has a base — a freshly created account can work at once.
-- The two rows are born in the SAME statement on purpose: flows points at the
-- current version and flow_versions points back at the flow — the reference is
-- circular by construction. The deferred FK settles that inside a transaction,
-- and the CTE guarantees there is one, even when the file is applied outside
-- goose (psql in autocommit, which is how the migration is checked on the local
-- cluster).
--
-- The seeded stage keys, names and description are Portuguese CONTENT, not
-- code: they are stored data that the cockpit shows, and localizing them means
-- a per-locale catalogue plus a data migration. Pending, on purpose.
WITH catalog AS (
  INSERT INTO flows (id, account_id, owner_scope, owner_id, current_version, idempotency_key)
  VALUES ('0000f10a-0000-4000-8000-000000000001', NULL, 'platform', NULL, 1, 'seed:platform:flow')
  RETURNING id
)
INSERT INTO flow_versions (flow_id, version, name, description, stages, idempotency_key)
SELECT catalog.id, 1,
  'Fluxo padrão da plataforma',
  'Contexto → spec → plano → implementação → teste → validação humana → finalização. Herdado por toda conta que não declarar o seu.',
  '[
    {"key":"contexto",          "name":"Contexto",          "type":"context",          "artifacts":["document"],  "gate":"none",  "subtypes":[]},
    {"key":"spec",              "name":"Spec",              "type":"spec",             "artifacts":["spec"],      "gate":"none",  "subtypes":[]},
    {"key":"plano",             "name":"Plano",             "type":"plan",             "artifacts":["plan"],      "gate":"none",  "subtypes":[]},
    {"key":"implementacao",     "name":"Implementação",     "type":"implementation",   "artifacts":[],            "gate":"none",  "subtypes":[]},
    {"key":"teste",             "name":"Teste",             "type":"test",             "artifacts":["test_plan"], "gate":"none",  "subtypes":["aaa","e2e","integracao"]},
    {"key":"validacao-humana",  "name":"Validação humana",  "type":"human_validation", "artifacts":["report"],    "gate":"human", "subtypes":[]},
    {"key":"finalizacao",       "name":"Finalização",       "type":"finalization",     "artifacts":["report"],    "gate":"none",  "subtypes":[]}
  ]'::jsonb,
  'seed:platform:flow:1'
FROM catalog;

-- +goose Down
DROP TRIGGER IF EXISTS flow_versions_imutaveis ON flow_versions;
DROP FUNCTION IF EXISTS assert_flow_version_congelada();
ALTER TABLE IF EXISTS flows DROP CONSTRAINT IF EXISTS flows_versao_corrente_existe;
DROP TABLE IF EXISTS flow_versions;
DROP TABLE IF EXISTS flows;
DROP TYPE IF EXISTS flow_scope;
