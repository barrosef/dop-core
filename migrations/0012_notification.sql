-- +goose Up
-- The record of communication SENT (ADR-0018).
--
-- This table is not a queue and it is not a projection: it is the RECORD of
-- what was fired, with a state, in the shape the sibling project already proved
-- useful (`sent` / `sent_local` / `error`). Without it, "the invite never
-- arrived" is a question with no answer — e-mail is the only part of the
-- platform whose outcome happens outside it.
--
-- ── Why the idempotency key is COMPOSITE ────────────────────────────────────
--
-- The attention box uses a unique index per EVENT, and it works because there
-- the relationship is 1:1. Here it is not: with declarative reaction (P-29) one
-- event will be able to fire N actions, and a key on the event alone would
-- discard the second as a duplicate. Discarding by idempotency is SILENT by
-- design — nobody would see the second notice disappear. The key is born
-- composite even with a single action today, because changing an idempotency
-- key later means migrating live data.
--
-- The key is exactly (event, rule, action), as the ADR wrote it. The RECIPIENT
-- stays OUT of it on purpose: who the action addresses is the rule's decision,
-- and a digest that goes to two members today and three tomorrow must not be
-- resent to the old two because of the third.
CREATE TABLE notification_deliveries (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,

  -- ── the composite key ──
  -- No FK to `events`: the events table is partitioned by month, and an FK to a
  -- partitioned table would tie dropping an old partition to the send record.
  event_id     uuid NOT NULL,
  rule_name    text NOT NULL,
  action_name  text NOT NULL,

  -- kind is the notification's KIND in the domain's vocabulary (`invite`,
  -- `attention_digest`). The one that translates a kind into a template is the
  -- adapter — here it is a record, not routing.
  kind         text NOT NULL,
  channel      text NOT NULL DEFAULT 'email',

  -- The addresses the action served. An array, and not a row per recipient,
  -- because the unit of idempotency is the ACTION: one item's digest is a
  -- single send, even if the account has five members.
  recipients   text[] NOT NULL DEFAULT '{}',

  -- pending: reserved, with no outcome yet. A row stuck in pending is a VISIBLE
  -- anomaly (the process died between the send and the record) — and on purpose
  -- it is not resent: a duplicate e-mail has no undo, and losing a recorded
  -- notice is less serious than sending two.
  -- sent / sent_local / error: the sibling project's three states.
  state        text NOT NULL DEFAULT 'pending'
               CHECK (state IN ('pending','sent','sent_local','error')),
  provider     text NOT NULL DEFAULT '',
  reference    text NOT NULL DEFAULT '',
  -- The failure message. It NEVER carries a credential: the adapter redacts
  -- before returning the error, and it is the contract suite that proves it.
  error        text NOT NULL DEFAULT '',
  attempts     int  NOT NULL DEFAULT 1,

  -- batch_id groups the rows covered by ONE message. It is what makes the
  -- digest possible (N items in one e-mail) without giving up one row per
  -- (event, rule, action): idempotency stays per item, the message stays one.
  batch_id     uuid,

  created_at   timestamptz NOT NULL DEFAULT now(),
  settled_at   timestamptz
);

-- THE GUARANTEE. JetStream's redelivery is at-least-once (ADR-0014); without
-- this index, the same invite lands three times in the invitee's inbox.
CREATE UNIQUE INDEX notification_deliveries_idem_idx
  ON notification_deliveries (event_id, rule_name, action_name);

-- The digest's sweep asks "what of this account has not become a notice yet".
CREATE INDEX notification_deliveries_conta_idx
  ON notification_deliveries (account_id, created_at DESC);

-- A resend only exists for what FAILED, and the partial index is what makes
-- that sweep cheap when nearly everything went right.
CREATE INDEX notification_deliveries_falhas_idx
  ON notification_deliveries (state, created_at)
  WHERE state = 'error';

-- +goose Down
DROP TABLE IF EXISTS notification_deliveries;
