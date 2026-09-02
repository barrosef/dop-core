-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- The execution substrate: the sandbox where the demand runs.
--
-- One active demand, one sandbox (the substrate spec §1). Two things live
-- inside it — the EXECUTION, ephemeral, and the WORKSPACE, which is the work.
-- Suspending drops the first and preserves the second; destroying takes both
-- and does not come back. That difference is the reason the trigger at the end
-- of this file exists.
-- ════════════════════════════════════════════════════════════════════════════

-- Declared, never presumed: it is these three, and the column is born WITH NO
-- DEFAULT.
CREATE TYPE isolation_tier AS ENUM ('hardware', 'kernel_emulated', 'namespace');

CREATE TYPE sandbox_state AS ENUM ('provisioning', 'active', 'suspended', 'destroyed');

CREATE TABLE sandboxes (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id     uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  -- No FK to demands: the demands table is born in another migration, written
  -- in parallel. Once it exists, the link is a one-line ALTER — and inventing
  -- here a table that is not mine would create two truths about what a demand
  -- is.
  demand_id      uuid NOT NULL,
  state          sandbox_state NOT NULL DEFAULT 'provisioning',
  -- NO DEFAULT, and that is this column's entire point: the isolation tier is
  -- DECLARED, never presumed. A default here would be the database choosing, on
  -- the client's behalf, how much isolation their workload deserves — and that
  -- choice's error would only show up on the day of the incident.
  tier           isolation_tier NOT NULL,
  namespace      text NOT NULL,
  -- The ports published by the demand's stack. jsonb because the list comes
  -- from the substrate and changes with the inner compose, with no migration.
  endpoints      jsonb NOT NULL DEFAULT '[]',
  -- Every write carries the key: repeating the call must not duplicate a microVM.
  idempotency_key text,
  last_active_at timestamptz NOT NULL DEFAULT now(),
  suspended_at   timestamptz,
  destroyed_at   timestamptz,
  created_by     uuid REFERENCES users(id),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT sandbox_destruido_tem_data
    CHECK (state <> 'destroyed' OR destroyed_at IS NOT NULL)
);

-- A repeat of the same key resolves to the SAME sandbox. A partial index
-- because a call with no key (an internal flow) does not compete for
-- uniqueness.
CREATE UNIQUE INDEX sandboxes_idempotency_uniq
  ON sandboxes (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- An active demand has ONE sandbox. Alive is everything that is not destroyed:
-- the history of destroyed ones stays, and a demand may be reprovisioned
-- later.
CREATE UNIQUE INDEX sandboxes_demanda_viva_uniq
  ON sandboxes (demand_id) WHERE state <> 'destroyed';

CREATE INDEX sandboxes_account_state_idx ON sandboxes (account_id, state);

-- The saving sweeper's index: only the active ones matter to it.
CREATE INDEX sandboxes_ociosos_idx
  ON sandboxes (last_active_at) WHERE state = 'active';

-- Invariant: destroyed is ABSORBING.
--
-- Checked by a trigger, and not only in the service, for the same reason as
-- migration 0001's owner guard: it is a rule NO operation may violate, not even
-- through a path nobody foresaw. The destruction took the workspace with it —
-- resurrecting the row would make the system assert that work exists which no
-- longer does.
CREATE OR REPLACE FUNCTION assert_sandbox_destruicao_irreversivel() RETURNS trigger AS $$
BEGIN
  IF OLD.state = 'destroyed' AND NEW.state <> 'destroyed' THEN
    RAISE EXCEPTION 'sandbox destruído não retoma: a destruição levou o workspace junto';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER sandboxes_destruicao_guard
  BEFORE UPDATE ON sandboxes
  FOR EACH ROW EXECUTE FUNCTION assert_sandbox_destruicao_irreversivel();

-- +goose Down
DROP TRIGGER IF EXISTS sandboxes_destruicao_guard ON sandboxes;
DROP FUNCTION IF EXISTS assert_sandbox_destruicao_irreversivel();
DROP TABLE IF EXISTS sandboxes CASCADE;
DROP TYPE IF EXISTS sandbox_state;
DROP TYPE IF EXISTS isolation_tier;
