-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Cost: measuring LLM usage, a budget per scope (ADR-0011/0012).
--
-- Three tables, and each exists for a different reason:
--
--   cost_usage      — what was consumed. It is the platform's FASTEST-growing
--                     table: N agents × long sessions × one record per turn.
--                     Partitioned by month, like `events`.
--   cost_usage_keys — the non-duplication guard. Separate on purpose; the
--                     justification sits above the table.
--   cost_budgets    — the ceiling and the accumulated spend per scope. Few
--                     rows, a lot of UPDATE: it stays off the partition path.
-- ════════════════════════════════════════════════════════════════════════════

-- ── why an integer, and not numeric or float ────────────────────────────────
-- A monetary value in MICROS (10^-6 of the currency's unit), bigint, the same
-- as the contract's Money.amount_micros (api/proto/dop/v1/common.proto).
--
-- float/double is out of the question: summing millions of rows in base-2
-- binary accumulates error, and a budget that is wrong by cents stops being a
-- budget. numeric would be exact, but costs more per sum, does not fit in a
-- register and — the deciding point — the contract ALREADY speaks in micros:
-- converting at the edge would create two vocabularies of money in the same
-- system.
--
-- Micros give generous headroom: one output token on a strong model costs on
-- the order of 75 micros of a dollar; a bigint in micros only overflows above
-- 9.2 trillion dollars. And the currency travels alongside, because summing
-- micros of different currencies is exactly the bug a bare `int64` does not
-- prevent.

CREATE TABLE cost_usage (
  id             uuid NOT NULL DEFAULT gen_random_uuid(),
  account_id     uuid NOT NULL,
  -- No FK to demands: that table is born in another migration, and cost has to
  -- be measurable for work that belongs to no demand (a nightly job, batch
  -- indexing — ADR-0012 §5). NULL here means "the account's consumption".
  demand_id      uuid,
  thread_id      text NOT NULL DEFAULT '',
  model          text NOT NULL,
  input_tokens          bigint NOT NULL DEFAULT 0,
  output_tokens         bigint NOT NULL DEFAULT 0,
  -- The two cache fields are the ModelRouter's calibration material (P-7) and
  -- the detector of ADR-0012's silent invalidator: reading a cached prefix
  -- costs ~0.1× of the input, and a zeroed cache_read on a repeated turn with a
  -- stable prefix is an ALERT, not a curiosity.
  cache_read_tokens     bigint NOT NULL DEFAULT 0,
  cache_creation_tokens bigint NOT NULL DEFAULT 0,
  cost_micros    bigint NOT NULL DEFAULT 0,
  currency       text   NOT NULL DEFAULT 'USD',
  occurred_at    timestamptz NOT NULL DEFAULT now(),
  recorded_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT cost_usage_nao_negativo CHECK (
    input_tokens >= 0 AND output_tokens >= 0 AND
    cache_read_tokens >= 0 AND cache_creation_tokens >= 0 AND
    cost_micros >= 0),
  -- The partition key goes into the PK because Postgres requires it — and that
  -- is what makes a UNIQUE on idempotency_key alone impossible HERE (see
  -- below).
  PRIMARY KEY (id, occurred_at),
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
) PARTITION BY RANGE (occurred_at);

-- The three queries that exist: the account's statement, the demand's statement
-- and the per-model calibration. All by account — isolation is a constraint.
CREATE INDEX cost_usage_account_idx ON cost_usage (account_id, occurred_at DESC);
CREATE INDEX cost_usage_demand_idx  ON cost_usage (account_id, demand_id, occurred_at DESC)
  WHERE demand_id IS NOT NULL;
CREATE INDEX cost_usage_model_idx   ON cost_usage (account_id, model, occurred_at DESC);

-- The current partition and the following ones; the sched mode creates the
-- rest, as it already does for `events`. Partitioning here is what makes it
-- possible to DELETE old history with a partition DROP (instantaneous) rather
-- than a DELETE of millions of rows.
CREATE TABLE cost_usage_2026_08 PARTITION OF cost_usage
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE cost_usage_2026_09 PARTITION OF cost_usage
  FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE cost_usage_2026_10 PARTITION OF cost_usage
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

-- ── the non-duplication guard ───────────────────────────────────────────────
-- Why a table just for the key, instead of a UNIQUE on cost_usage:
--
-- Every UNIQUE on a partitioned table HAS to contain the partition key. A
-- `UNIQUE (idempotency_key, occurred_at)` would only stop a repeat arriving
-- with the SAME instant — and the instant is filled in by the clock when the
-- caller does not send it, so the retry would arrive with another one. The
-- guarantee would die in silence, and a budget with a doubled record is
-- fiction.
--
-- This table is flat and the UNIQUE is real. It is small per row (the key plus
-- two pointers), grows alongside cost_usage and is pruned at the same cadence
-- at which the old partitions drop — occurred_at is here for that.
--
-- The key is scoped by account: a key leaked from one tenant does not probe
-- another, and two independent clients may use the same UUID without
-- colliding.
CREATE TABLE cost_usage_keys (
  account_id      uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  idempotency_key text NOT NULL,
  -- No FK: the target's PK is composite (id, occurred_at) because of the partition.
  usage_id        uuid NOT NULL,
  occurred_at     timestamptz NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, idempotency_key)
);
CREATE INDEX cost_usage_keys_pruning_idx ON cost_usage_keys (occurred_at);

-- ── budget ──────────────────────────────────────────────────────────────────
-- The scope is (scope, scope_id): 'account' with the account's id, 'demand'
-- with the demand's id. Two scopes and no more: the per-thread slice is derived
-- from the subagent's brief (ADR-0010), it is not a row here.
--
-- spent_micros is accumulated, not recomputed: summing cost_usage on every
-- record would become a scan of the system's largest table on the hot path. The
-- accumulated value is rebuildable from cost_usage should it ever diverge — the
-- truth is still the log.
CREATE TABLE cost_budgets (
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  scope        text NOT NULL CHECK (scope IN ('account', 'demand')),
  scope_id     text NOT NULL,
  -- 0 = NO CEILING. The absence of a budget must not become a zero budget, or
  -- every new account would be born paused on its first token.
  limit_micros bigint NOT NULL DEFAULT 0 CHECK (limit_micros >= 0),
  spent_micros bigint NOT NULL DEFAULT 0 CHECK (spent_micros >= 0),
  currency     text   NOT NULL DEFAULT 'USD',
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, scope, scope_id)
);

-- +goose Down
DROP TABLE IF EXISTS cost_budgets;
DROP TABLE IF EXISTS cost_usage_keys;
DROP TABLE IF EXISTS cost_usage CASCADE;
