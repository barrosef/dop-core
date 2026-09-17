-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Delivery: evidence of green, pull requests, a per-repository merge queue and
-- coordination directives (ADR-0005, ADR-0005, ADR-0011).
--
-- Three of this domain's invariants are too expensive to live only in the
-- application code, and so they sit here as a constraint or a trigger:
--
--   1. NO GREEN, NO PR — there is no row in pull_requests whose commit has no
--      approved evidence (the `assert_pr_tem_verde` trigger);
--   2. THE QUEUE HAS A TOTAL ORDER — (priority, seq) with seq unique per
--      repository; two entries never tie ambiguously;
--   3. A DIRECTIVE COORDINATES, IT NEVER PAUSES — the vocabulary of actions is
--      a closed enum and the trigger refuses any instruction outside it
--      (ADR-0011 §5).
--
-- About demand_id: references to the demand are by UUID, with no FK. The
-- `demands` table belongs to another migration, written in parallel; tying an
-- FK here would create an ordering dependency between independent pieces of
-- work. The account stays tied by an FK on every table — multi-tenant isolation
-- is a constraint.
-- ════════════════════════════════════════════════════════════════════════════

-- ── evidence of green (ADR-0005) ────────────────────────────────────────────

-- The critic's verdict is a kind of run like the others, on purpose: it has a
-- commit, an outcome and a trace. An adjective hanging off the PR cannot be
-- audited.
CREATE TYPE verification_kind AS ENUM ('acceptance', 'unit', 'e2e', 'critic');

-- There is no 'unknown': a run with no outcome is not evidence, it is noise.
CREATE TYPE verification_outcome AS ENUM ('passed', 'failed', 'errored');

CREATE TABLE verification_runs (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  demand_id    uuid NOT NULL,
  repo_id      uuid NOT NULL REFERENCES project_repos(id) ON DELETE CASCADE,
  -- The field that gives everything else its value: evidence that does not say
  -- WHICH code ran can be used to prove anything, and therefore proves nothing.
  commit_sha   text NOT NULL CHECK (commit_sha <> ''),
  kind         verification_kind NOT NULL,
  suite        text NOT NULL CHECK (suite <> ''),   -- WHAT ran
  outcome      verification_outcome NOT NULL,
  total        int NOT NULL DEFAULT 0 CHECK (total  >= 0),
  passed       int NOT NULL DEFAULT 0 CHECK (passed >= 0),
  failed       int NOT NULL DEFAULT 0 CHECK (failed >= 0),
  -- The trace: where it ran and where the log ended up (an ObjectStore pointer).
  sandbox_id   text NOT NULL DEFAULT '',
  log_ref      text NOT NULL DEFAULT '',
  detail       jsonb NOT NULL DEFAULT '{}',          -- the critic's verdict, reasons
  attempts     int NOT NULL DEFAULT 1 CHECK (attempts > 0),
  started_at   timestamptz,
  ended_at     timestamptz NOT NULL DEFAULT now(),
  idempotency_key text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  -- "It passed" with nowhere to check is a word, not evidence.
  CONSTRAINT evidencia_tem_rastro CHECK (sandbox_id <> '' OR log_ref <> ''),
  -- Approved with a failure is incoherent; letting it through would let a false
  -- green in.
  CONSTRAINT verde_nao_tem_falha  CHECK (outcome <> 'passed' OR failed = 0),
  -- Running the same suite again on the same commit UPDATES the row and
  -- increments attempts: how many times it was tried is part of the evidence.
  UNIQUE (account_id, demand_id, repo_id, commit_sha, kind, suite)
);

-- The hot path's index: "is this commit green?" is the question opening the PR
-- and entering the queue ask every single time.
CREATE INDEX verification_runs_evidencia_idx
  ON verification_runs (account_id, demand_id, repo_id, commit_sha);

-- ── pull requests ───────────────────────────────────────────────────────────

CREATE TABLE pull_requests (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  demand_id     uuid NOT NULL,
  repo_id       uuid NOT NULL REFERENCES project_repos(id) ON DELETE CASCADE,
  repo_name     text NOT NULL DEFAULT '',
  source_branch text NOT NULL CHECK (source_branch <> ''),
  target_branch text NOT NULL DEFAULT 'main',
  -- The head commit: it is the one the evidence is demanded of, today and on
  -- every re-verification by the queue.
  head_commit   text NOT NULL CHECK (head_commit <> ''),
  url           text NOT NULL DEFAULT '',
  external_id   text NOT NULL DEFAULT '',   -- the number/id at the provider
  merged        boolean NOT NULL DEFAULT false,
  has_conflict  boolean NOT NULL DEFAULT false,
  reviewers     jsonb NOT NULL DEFAULT '[]',
  idempotency_key text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  -- One PR per demand in each repository. It is what EnqueueMerge's contract
  -- assumes when it addresses the PR by (repo_id, demand_id) — if there is ever
  -- one PR per target branch, the contract changes along with this constraint.
  UNIQUE (repo_id, demand_id)
);
CREATE INDEX pull_requests_conta_idx  ON pull_requests (account_id, merged);
CREATE INDEX pull_requests_demanda_idx ON pull_requests (demand_id);
CREATE UNIQUE INDEX pull_requests_idem_idx
  ON pull_requests (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- ADR-0005 as a TRIGGER. The service already refuses earlier, with a message
-- that says what is missing; here is the backstop that holds for the paths
-- nobody foresaw — a backfill, an operations script, a call-ordering bug. A
-- business rule NO operation may violate does not live in the application
-- alone.
CREATE OR REPLACE FUNCTION assert_pr_tem_verde() RETURNS trigger AS $$
DECLARE acceptance int; critic int; red int;
BEGIN
  SELECT count(*) FILTER (WHERE kind = 'acceptance' AND outcome = 'passed'),
         count(*) FILTER (WHERE kind = 'critic'     AND outcome = 'passed'),
         count(*) FILTER (WHERE outcome <> 'passed')
    INTO acceptance, critic, red
    FROM verification_runs
   WHERE account_id = NEW.account_id
     AND demand_id  = NEW.demand_id
     AND repo_id    = NEW.repo_id
     AND commit_sha = NEW.head_commit;

  IF acceptance = 0 THEN
    RAISE EXCEPTION 'no green, no PR: no passed acceptance run for commit %', NEW.head_commit;
  END IF;
  IF critic = 0 THEN
    RAISE EXCEPTION 'no green, no PR: the critic''s opinion for commit % is missing', NEW.head_commit;
  END IF;
  IF red > 0 THEN
    RAISE EXCEPTION 'no green, no PR: % run(s) not passed on commit %', red, NEW.head_commit;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

-- On the UPDATE of head_commit too: the branch moving on must not smuggle
-- unverified code into a PR that already exists.
CREATE TRIGGER pull_requests_verde_guard
  BEFORE INSERT OR UPDATE OF head_commit ON pull_requests
  FOR EACH ROW EXECUTE FUNCTION assert_pr_tem_verde();

-- ── per-repository merge queue (ADR-0005) ───────────────────────────────────

CREATE TYPE merge_queue_state AS ENUM
  ('queued', 'rebasing', 'verifying', 'merged', 'conflict');

CREATE TABLE merge_queue_entries (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id      uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  repo_id         uuid NOT NULL REFERENCES project_repos(id) ON DELETE CASCADE,
  demand_id       uuid NOT NULL,
  pull_request_id uuid NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
  -- The arrival sequence WITHIN the repository. Assigned under a lock on the
  -- repository's row, it is the tie-break that guarantees a total order.
  seq             bigint NOT NULL CHECK (seq > 0),
  -- Where the preferred-order directive acts (ADR-0011 §6). Lower goes first.
  -- Reordering pauses nobody: whoever lost their turn keeps running.
  priority        int NOT NULL DEFAULT 100,
  state           merge_queue_state NOT NULL DEFAULT 'queued',
  overlapping_files text[] NOT NULL DEFAULT '{}',   -- the techlead's detection
  conflict        jsonb,
  idempotency_key text,
  enqueued_at     timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  -- A PR does not enter the same repository's queue twice. A constraint, not
  -- code discipline: two concurrent enqueues of the same PR are exactly the
  -- retry a client makes when the answer is lost.
  UNIQUE (repo_id, pull_request_id),
  -- The queue's order is (priority, seq) and seq is unique per repository:
  -- there is no pair of entries whose relative order depends on who ran the
  -- SELECT.
  UNIQUE (repo_id, seq),
  -- A conflict escalated with no report is a raw alarm: the attention box needs
  -- the context for the human to decide without archaeology (ADR-0005 §2).
  CONSTRAINT conflito_tem_relato CHECK (state <> 'conflict' OR conflict IS NOT NULL)
);
CREATE INDEX merge_queue_ordem_idx
  ON merge_queue_entries (account_id, repo_id, priority, seq);
CREATE UNIQUE INDEX merge_queue_idem_idx
  ON merge_queue_entries (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- ── coordination directives (ADR-0011) ──────────────────────────────────────

-- The vocabulary is CLOSED and has no 'pause', 'block' or 'suspend'. The golden
-- rule ("an identified cross-cutting concern NEVER pauses a demand") is not the
-- care of whoever writes the code: it is the absence of a value in the type.
CREATE TYPE directive_kind AS ENUM
  ('cherry_pick', 'merge_order', 'file_partition', 'cross_verify');

CREATE TYPE directive_status AS ENUM ('proposed', 'decided', 'superseded');

CREATE TABLE directives (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  kind         directive_kind NOT NULL,
  summary      text NOT NULL CHECK (summary <> ''),
  payload      jsonb NOT NULL DEFAULT '{}',       -- the techlead's signals
  affected_demands uuid[] NOT NULL DEFAULT '{}',
  -- Ready-made options, each with the instructions it fires.
  options      jsonb NOT NULL DEFAULT '[]',
  recommended  text NOT NULL,
  status       directive_status NOT NULL DEFAULT 'proposed',
  decided_option text,
  rationale    text,
  decided_by   uuid REFERENCES users(id),
  decided_at   timestamptz,
  idempotency_key text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  -- A prompt for a decision, not an alarm: with a single option there is
  -- nothing to decide.
  CONSTRAINT diretriz_tem_opcoes CHECK (jsonb_array_length(options) >= 2),
  -- Deciding RECORDS who decided and why. Without the reason stored, nobody
  -- understands three weeks later why demand 2 waited for demand 1.
  CONSTRAINT decisao_tem_autor_e_motivo CHECK (
    status <> 'decided' OR (
      decided_option IS NOT NULL AND decided_by IS NOT NULL
      AND decided_at IS NOT NULL AND COALESCE(btrim(rationale), '') <> ''))
);
CREATE INDEX directives_projeto_idx ON directives (account_id, project_id, status);
CREATE UNIQUE INDEX directives_idem_idx
  ON directives (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- ADR-0011 §5's golden rule as a trigger, because a CHECK cannot do a
-- subquery. Two things checked together: the recommendation is one of the
-- options (recommending what is not in the list is the silent version of
-- recommending nothing), and every instruction of every option belongs to the
-- COORDINATION vocabulary. A directive telling somebody to "pause demand 1"
-- never reaches the database.
CREATE OR REPLACE FUNCTION assert_diretriz_coordena_sem_pausar() RETURNS trigger AS $$
DECLARE acao text;
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM jsonb_array_elements(NEW.options) o WHERE o->>'key' = NEW.recommended
  ) THEN
    RAISE EXCEPTION 'the recommendation % is not among the directive''s options', NEW.recommended;
  END IF;

  FOR acao IN
    SELECT i->>'action'
      FROM jsonb_array_elements(NEW.options) o,
           jsonb_array_elements(COALESCE(o->'instructions', '[]'::jsonb)) i
  LOOP
    IF acao IS NULL OR acao NOT IN
       ('cherry_pick', 'merge_order', 'file_partition', 'cross_verify') THEN
      RAISE EXCEPTION
        'a directive instruction outside the coordination vocabulary: % — a directive coordinates, it never pauses a demand (ADR-0011)',
        COALESCE(acao, '(vazia)');
    END IF;
  END LOOP;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER directives_coordenacao_guard
  BEFORE INSERT OR UPDATE OF options, recommended ON directives
  FOR EACH ROW EXECUTE FUNCTION assert_diretriz_coordena_sem_pausar();

-- +goose Down
DROP TRIGGER IF EXISTS directives_coordenacao_guard ON directives;
DROP FUNCTION IF EXISTS assert_diretriz_coordena_sem_pausar();
DROP TABLE IF EXISTS directives CASCADE;
DROP TYPE IF EXISTS directive_status, directive_kind;

DROP TABLE IF EXISTS merge_queue_entries CASCADE;
DROP TYPE IF EXISTS merge_queue_state;

DROP TRIGGER IF EXISTS pull_requests_verde_guard ON pull_requests;
DROP FUNCTION IF EXISTS assert_pr_tem_verde();
DROP TABLE IF EXISTS pull_requests CASCADE;

DROP TABLE IF EXISTS verification_runs CASCADE;
DROP TYPE IF EXISTS verification_outcome, verification_kind;
