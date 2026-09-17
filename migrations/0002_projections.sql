-- +goose Up
-- Projections — reads derived from the event log (ADR-0004).
--
-- They can be DELETED and rebuilt from `events` with no loss: they hold no
-- truth, they only present it in a convenient shape.

CREATE TABLE timeline (
  event_id     uuid PRIMARY KEY,
  account_id   uuid,
  aggregate    text NOT NULL,
  aggregate_id text NOT NULL,
  type         text NOT NULL,
  payload      jsonb NOT NULL DEFAULT '{}',
  occurred_at  timestamptz NOT NULL,
  projected_at timestamptz NOT NULL DEFAULT now()
);

-- The query the screen makes: "what happened on this demand, most recent first".
CREATE INDEX timeline_aggregate_idx ON timeline (aggregate, aggregate_id, occurred_at DESC);
CREATE INDEX timeline_account_idx   ON timeline (account_id, occurred_at DESC);
CREATE INDEX timeline_type_idx      ON timeline (type, occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS timeline;
