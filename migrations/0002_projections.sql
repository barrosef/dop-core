-- +goose Up
-- Projeções — leituras derivadas do log de eventos (ADR-0006).
--
-- Podem ser APAGADAS e reconstruídas a partir de `events` sem perda: elas não
-- guardam verdade, apenas a apresentam numa forma conveniente.

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

-- A consulta que a tela faz: "o que aconteceu nesta demanda, mais recente primeiro".
CREATE INDEX timeline_aggregate_idx ON timeline (aggregate, aggregate_id, occurred_at DESC);
CREATE INDEX timeline_account_idx   ON timeline (account_id, occurred_at DESC);
CREATE INDEX timeline_type_idx      ON timeline (type, occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS timeline;
