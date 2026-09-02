-- +goose Up
-- The attention box is a PROJECTION of the event log (ADR-0006), not a system.
--
-- The consequence in the schema: there is no column only a human can change. An
-- item opens through an event and closes through an event; there is no "mark as
-- read". An item that disappears without the problem having been solved is a
-- comfortable lie, and a box that lies becomes a box that is ignored — the
-- spec's risk R-1.
CREATE TABLE attention_items (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  kind         text NOT NULL,
  target_kind  text NOT NULL,
  target_id    text NOT NULL,
  demand_id    uuid,
  title        text NOT NULL,
  summary      text NOT NULL DEFAULT '',
  opened_at    timestamptz NOT NULL,
  resolved_at  timestamptz,
  -- The event that OPENED the item. It is what makes a redelivery harmless:
  -- JetStream's delivery is at-least-once (ADR-0019), and without this key the
  -- same block would become three identical items in the dev's face.
  opened_by_event uuid NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- The projection's idempotency: the same event never opens two items.
CREATE UNIQUE INDEX attention_items_evento_idx ON attention_items (opened_by_event);

-- A target may have only ONE open item of the same kind. Without this, a thread
-- that blocks, unblocks and blocks again would accumulate phantom items — and
-- the box would fill up with things already resolved, which is how it dies.
CREATE UNIQUE INDEX attention_items_alvo_aberto_idx
  ON attention_items (account_id, kind, target_kind, target_id)
  WHERE resolved_at IS NULL;

-- The box's query is always "this account's open items, most urgent first". A
-- partial index because a resolved item is not queried by the screen; it stays
-- to answer how long the dev took.
CREATE INDEX attention_items_abertos_idx
  ON attention_items (account_id, opened_at)
  WHERE resolved_at IS NULL;

CREATE INDEX attention_items_demanda_idx ON attention_items (demand_id)
  WHERE demand_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS attention_items;
