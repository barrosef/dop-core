-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Where an event ends when nobody could process it (spec 2026-09-13 §4).
--
-- Terminal and read by a person or an agent. There is no panel yet, and this
-- table exists before it on purpose: a table nobody reads is still better than
-- an event nobody kept.
--
-- The WHOLE attempt history travels in `attempts`, DLQ rounds included, because
-- the question this table answers is "what happened, exactly" — and an answer
-- that needs a second query to be useful is an answer nobody looks up.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TABLE event_errors (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id       text NOT NULL,
  consumer       text NOT NULL,
  account_id     uuid,
  event_type     text NOT NULL,
  aggregate      text NOT NULL,
  aggregate_id   text NOT NULL,
  -- The readable handle, as it was AT THE TIME. It may disagree with the
  -- aggregate's current name, and that is correct: this is a log of facts.
  aggregate_key  text,
  actor_kind     text,
  actor_id       text,
  request_id     text,
  attempts       jsonb NOT NULL DEFAULT '[]',
  classification text NOT NULL,
  last_code      text,
  last_message   text,
  state          text NOT NULL DEFAULT 'open',
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT event_error_state_is_known
    CHECK (state IN ('open', 'retrying', 'resolved', 'given_up')),
  CONSTRAINT event_error_classification_is_known
    CHECK (classification IN ('recoverable', 'irrecoverable', 'unknown'))
);

-- The two questions a panel asks: what is open, and what happened to this
-- event.
CREATE INDEX event_errors_open_idx ON event_errors (state, created_at DESC)
  WHERE state IN ('open', 'retrying');
CREATE INDEX event_errors_by_event_idx ON event_errors (event_id);

-- Retention, written now rather than discovered when the table is large: a
-- table fed by failures grows fastest exactly when things are worst. Resolved
-- and given-up rows past 90 days have no reader.
COMMENT ON TABLE event_errors IS
  'Events nobody could process. Terminal. Rows in state resolved or given_up older than 90 days may be deleted; open and retrying rows are never swept.';

-- +goose Down
DROP TABLE event_errors;
