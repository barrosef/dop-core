-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- THE DEMAND (ADR-0006, ADR-0010, ADR-0014)
--
-- Where the demand's events live: in `events`, the table that already exists.
--
-- A demand is an append-only log and its state is a projection of it
-- (ADR-0006). The whole platform's log is already `events` — partitioned by
-- month, with an outbox, a relay and replay ready. Giving the demand a log of
-- its own would duplicate the machinery (a second outbox, a second relay, a
-- second cursor), would break WatchDemand — which reuses the `event` domain's
-- single fan-out — and would create two possible pasts for the same audit
-- question. So: no event table here. Every demand event goes into `events` with
-- aggregate='demand' and aggregate_id = the demand's id, INCLUDING a thread
-- message and a finding; whoever needs to slice by thread reads
-- `payload->>'thread_id'`.
--
-- What the tables below keep is the PROJECTED STATE — the read the screen and
-- the stage machine need in O(1), without rereading the log. They are written
-- in the SAME transaction as the event (ADR-0019), and not by an asynchronous
-- consumer: the next transition's decision depends on the current state, and
-- deciding on a stale projection is deciding on the past. That is why there is
-- no asynchronous demand projection — there would be two writers for the same
-- rows.
--
-- Rebuilding: deleting these tables and reprocessing the account's `events`, in
-- occurred_at order, rebuilds everything — each payload carries the complete
-- delta (`dop.demand.started` brings the flow's snapshot and the stage keys;
-- `stage.advanced` and `gate.decided` bring the stage, the from/to and the
-- resulting status; `thread.created` brings the brief; `finding.published`
-- brings the whole finding).
--
-- A thread message has NO table: it is only an event. Reading the conversation's
-- history is the `timeline` projection, which already indexes (aggregate,
-- aggregate_id, occurred_at) — creating a message table would mean writing the
-- same text twice to serve a query that already exists.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TABLE demands (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id      uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  -- the card's key at the provider: SUOPT-1315
  external_key    text NOT NULL,
  title           text NOT NULL,
  -- the card kind and the provider's status are dynamic, the provider's (ADR-0013)
  card_type       text NOT NULL DEFAULT '',
  provider_status text NOT NULL DEFAULT '',
  dop_status      text NOT NULL DEFAULT 'new'
                  CHECK (dop_status IN ('new','doing','done','delivered')),

  -- ── the FROZEN flow (ADR-0014 §4) ──
  -- flow_id is text and has NO FK on purpose: the snapshot has to survive the
  -- flow being edited, promoted or deleted. An FK with CASCADE would delete the
  -- demand along with it; an FK with RESTRICT would stop the account from
  -- cleaning up its catalogue. What drives the demand from here on is
  -- flow_snapshot, not the live flow.
  flow_id         text NOT NULL DEFAULT '',
  flow_version    int  NOT NULL DEFAULT 0,
  flow_snapshot   jsonb NOT NULL DEFAULT '{}',

  -- an actor, not a user: whoever starts it may be a human, an agent or the
  -- system (the task manager's sync). Hence text, with no FK to users.
  created_by      text NOT NULL DEFAULT '',
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),

  -- One demand per provider card. It is this constraint that makes a repeated
  -- StartDemand return the existing demand instead of freezing the flow again.
  UNIQUE (project_id, external_key)
);

CREATE INDEX demands_account_idx ON demands (account_id, created_at DESC);
CREATE INDEX demands_project_idx ON demands (project_id, created_at DESC);
CREATE INDEX demands_status_idx  ON demands (account_id, dop_status);

-- The demand's stages: instances of the frozen mould. The mould stays in
-- flow_snapshot; what lives here is the PROGRESS.
CREATE TABLE demand_stages (
  demand_id     uuid NOT NULL REFERENCES demands(id) ON DELETE CASCADE,
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  key           text NOT NULL,
  name          text NOT NULL DEFAULT '',
  -- the platform's CLOSED vocabulary: a new kind is evolution, not data
  type          text NOT NULL
                CHECK (type IN ('context','spec','plan','implementation','test',
                                'human_validation','finalization','generic')),
  gate          text NOT NULL DEFAULT 'none' CHECK (gate IN ('none','human')),
  status        text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','running','blocked','done')),
  position      int  NOT NULL,
  artifacts     jsonb NOT NULL DEFAULT '[]',
  started_at    timestamptz,
  finished_at   timestamptz,
  -- NULL = nobody has decided yet; distinct from "rejected"
  gate_approved boolean,
  gate_comment  text NOT NULL DEFAULT '',
  PRIMARY KEY (demand_id, key)
);

CREATE UNIQUE INDEX demand_stages_position_uniq ON demand_stages (demand_id, position);
CREATE INDEX demand_stages_account_idx ON demand_stages (account_id, status);

-- One thread per agent — the dev talks without mixing timelines (ADR-0010).
CREATE TABLE demand_threads (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  demand_id     uuid NOT NULL REFERENCES demands(id) ON DELETE CASCADE,
  key           text NOT NULL,          -- main, db-forensics, logs
  -- the subagent's brief (ADR-0010 §2): without it, a subagent is a black box
  purpose       text NOT NULL DEFAULT '',
  tools         text[] NOT NULL DEFAULT '{}',
  model         text NOT NULL DEFAULT '',
  effort        text NOT NULL DEFAULT '',
  budget_micros bigint NOT NULL DEFAULT 0,
  -- the thread's cycle: open → active → blocked → concluded
  state         text NOT NULL DEFAULT 'aberta'
                CHECK (state IN ('aberta','ativa','bloqueada','concluida')),
  created_by    text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (demand_id, key),
  -- makes the finding's composite FK below possible
  UNIQUE (id, demand_id)
);

-- The attention box's query: the account's blocked threads, oldest first. A
-- partial index because only what is blocked matters to the queue.
CREATE INDEX demand_threads_blocked_idx ON demand_threads (account_id, updated_at)
  WHERE state = 'bloqueada';
CREATE INDEX demand_threads_demand_idx ON demand_threads (demand_id);

-- The demand's board of findings (ADR-0010 §4).
--
-- A finding is STATE, not only narrative: it is what unlocks the thread's
-- conclusion, and that decision cannot depend on the asynchronous projection,
-- which may not have seen a publication from a second ago.
CREATE TABLE demand_findings (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  demand_id  uuid NOT NULL REFERENCES demands(id) ON DELETE CASCADE,
  thread_id  uuid NOT NULL,
  title      text NOT NULL,
  payload    jsonb NOT NULL DEFAULT '{}',
  created_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  -- A COMPOSITE FK: it guarantees in the database that the finding's thread
  -- belongs to the SAME demand. The demand is the security boundary
  -- (ADR-0010 §6); a finding crossing demands would be a context leak, and that
  -- is an invariant, not a validation.
  FOREIGN KEY (thread_id, demand_id)
    REFERENCES demand_threads (id, demand_id) ON DELETE CASCADE
);

CREATE INDEX demand_findings_demand_idx ON demand_findings (demand_id, created_at DESC);
CREATE INDEX demand_findings_thread_idx ON demand_findings (thread_id);

-- +goose StatementBegin
-- A thread does not die in silence.
--
-- Concluding requires a published finding (the conversation-and-attention spec
-- §1). The rule is in the domain too, with a better message; here it is an
-- INVARIANT — no path, not even one nobody foresaw, closes an investigation
-- without leaving a durable record. The same pattern as "every account has an
-- active owner".
CREATE OR REPLACE FUNCTION demand_thread_requires_finding() RETURNS trigger AS $$
BEGIN
  IF NEW.state = 'concluida' AND OLD.state IS DISTINCT FROM 'concluida' THEN
    IF NOT EXISTS (SELECT 1 FROM demand_findings f WHERE f.thread_id = NEW.id) THEN
      RAISE EXCEPTION
        'a thread % não pode ser concluída sem achado publicado', NEW.key;
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER demand_thread_conclusion_guard
  BEFORE UPDATE ON demand_threads
  FOR EACH ROW EXECUTE FUNCTION demand_thread_requires_finding();

-- +goose Down
DROP TRIGGER IF EXISTS demand_thread_conclusion_guard ON demand_threads;
DROP FUNCTION IF EXISTS demand_thread_requires_finding();
DROP TABLE IF EXISTS demand_findings, demand_threads, demand_stages, demands CASCADE;
