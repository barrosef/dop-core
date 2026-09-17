-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- The agent's consumption metrics (P-23 phase 1).
--
-- ── Why a schema of its own, next to `cost` ─────────────────────────────────
--
-- `cost` is OPERATIONAL: consumption per demand, so a budget can be exceeded
-- and somebody warned (ADR-0008). What is here is ANALYTICAL: one row per TURN,
-- wide, raw, to be studied — which turn burned what, which tool, how much of it
-- was cache. Two different questions, two shapes; putting the analysis into the
-- operational table would make every budget read carry the weight of a study.
--
-- ── Why we do not produce this data ─────────────────────────────────────────
--
-- Claude Code ALREADY writes it, per turn, to a session file: input, output,
-- cache creation, cache read, the model, the stop reason and the turn tree.
-- Producing our own version would be a second, poorer truth. So we COLLECT it
-- and store it in a shape we can query.
--
-- The cache split is what makes this worth storing at all. In a real session:
-- 2.55 billion tokens read from cache against 23 thousand of raw input. A table
-- that recorded only "input_tokens" would tell a story that is off by five
-- orders of magnitude.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TABLE agent_sessions (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  -- The demand may be absent: a session raised by hand, for measurement, is
  -- exactly what phase 1 is made of.
  demand_id     uuid REFERENCES demands(id) ON DELETE SET NULL,
  project_id    uuid REFERENCES projects(id) ON DELETE SET NULL,
  -- external_id is Claude Code's own sessionId. It is what makes the collection
  -- idempotent: re-reading the same file finds the same session.
  external_id   text NOT NULL,
  -- Where the work happened, as the tool saw it.
  cwd           text NOT NULL DEFAULT '',
  git_branch    text NOT NULL DEFAULT '',
  tool_version  text NOT NULL DEFAULT '',
  started_at    timestamptz,
  ended_at      timestamptz,
  -- byte_offset is the collector's cursor: the session file grows while the
  -- agent works, and re-reading 40 MB on every pass is not collection, it is a
  -- denial of service against ourselves.
  byte_offset   bigint NOT NULL DEFAULT 0,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, external_id)
);

CREATE INDEX agent_sessions_demand_idx ON agent_sessions (demand_id);

CREATE TABLE agent_turns (
  id            bigserial PRIMARY KEY,
  session_id    uuid NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
  -- uuid/parent_uuid are the tool's own: together they are the turn TREE, which
  -- is how a chain of tool calls is told apart from a fresh question.
  uuid          text NOT NULL,
  parent_uuid   text NOT NULL DEFAULT '',
  occurred_at   timestamptz NOT NULL,
  model         text NOT NULL DEFAULT '',
  service_tier  text NOT NULL DEFAULT '',
  stop_reason   text NOT NULL DEFAULT '',
  -- A subagent's turn. Without this the cost of delegation is invisible,
  -- averaged into the main thread's.
  sidechain     boolean NOT NULL DEFAULT false,

  input_tokens          bigint NOT NULL DEFAULT 0,
  output_tokens         bigint NOT NULL DEFAULT 0,
  cache_creation_tokens bigint NOT NULL DEFAULT 0,
  cache_read_tokens     bigint NOT NULL DEFAULT 0,

  -- The counts of what the turn was MADE of. "Which tool burned what" is half
  -- the answer about waste, and it is answerable only from here.
  tool_uses     int NOT NULL DEFAULT 0,
  thinking      int NOT NULL DEFAULT 0,
  texts         int NOT NULL DEFAULT 0,
  -- tools names what was called, in order: `["Bash","Read","Bash"]`.
  tools         jsonb NOT NULL DEFAULT '[]',
  -- Everything else the tool reported and we have not given a column to yet —
  -- cache_creation's TTL split, output_tokens_details, iterations, speed. It is
  -- kept whole on purpose: the day a question needs it, the data is already
  -- there instead of starting to be collected then.
  raw_usage     jsonb NOT NULL DEFAULT '{}',

  UNIQUE (session_id, uuid)
);

CREATE INDEX agent_turns_session_idx  ON agent_turns (session_id, occurred_at);
CREATE INDEX agent_turns_model_idx    ON agent_turns (model);
CREATE INDEX agent_turns_occurred_idx ON agent_turns (occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS agent_turns;
DROP TABLE IF EXISTS agent_sessions;
