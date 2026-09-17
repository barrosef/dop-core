-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Reacting to an event is DATA (P-29).
--
-- Two invariants shape this schema:
--
--  1. A ROW HOLDS NO CODE. `when` and the action parameters are plain maps —
--     no expression, no function, no branch. The moment a row holds an
--     expression, swapping the policy stops being a loader and becomes a
--     rewrite, which is what this change exists to prevent.
--  2. RULES ACCUMULATE. There is no unique index making one rule win per level,
--     because every rule in the chain applies. An account switching off an
--     inherited rule does it by naming its id in `disables` — an act, never a
--     side effect of writing another rule.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TYPE reaction_trigger AS ENUM ('event', 'schedule');

CREATE TABLE reaction_rules (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- NULL at the platform level, which has no owner and applies to every
  -- account. Same shape as `flows`.
  account_id  uuid REFERENCES accounts(id) ON DELETE CASCADE,
  owner_scope text NOT NULL,
  owner_id    uuid,
  trigger     reaction_trigger NOT NULL DEFAULT 'event',
  event_type  text,
  -- Equality only. A field to a value, every entry must match.
  when_match  jsonb NOT NULL DEFAULT '{}'::jsonb,
  actions     jsonb NOT NULL DEFAULT '[]'::jsonb,
  disables    uuid[] NOT NULL DEFAULT '{}',
  enabled     boolean NOT NULL DEFAULT true,
  -- The row's REASON, not a restatement of what it does. Nobody can disagree
  -- with "invite.created sends an email"; anybody can disagree with the reason.
  -- Without it the policy is not auditable.
  why         text NOT NULL,
  -- Every write carries an idempotency key (ADR-0013): repeating the creation
  -- collides on reaction_rules_idempotency below and returns the rule already
  -- created, instead of a twin. Same column as `flows`; the uniqueness is
  -- scoped differently, and the index says why.
  idempotency_key text NOT NULL,
  created_by  uuid REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT reaction_scope_valid CHECK (owner_scope IN ('platform','account','workspace','project')),
  -- The platform's rules and an account's are mutually exclusive, exactly as in
  -- `flows`: either it has an owner and an account, or it is level 0 and has
  -- neither.
  CONSTRAINT reaction_platform_has_no_owner CHECK (
    (owner_scope =  'platform' AND account_id IS NULL     AND owner_id IS NULL) OR
    (owner_scope <> 'platform' AND account_id IS NOT NULL AND owner_id IS NOT NULL)
  ),
  CONSTRAINT reaction_event_has_type CHECK (trigger <> 'event' OR event_type IS NOT NULL),
  -- cardinality(), not array_length(disables, 1): array_length returns NULL
  -- (not 0) for an empty array, and NULL OR FALSE is NULL — which a CHECK
  -- treats as a pass. That trap let a rule with `actions = []` and
  -- `disables = {}` insert cleanly, refusing nothing. cardinality() returns
  -- 0 for an empty array, so the OR can actually reach FALSE.
  CONSTRAINT reaction_does_something CHECK (
    jsonb_array_length(actions) > 0 OR cardinality(disables) > 0
  ),
  -- An account-scoped rule's owner_id must BE the account, not merely belong
  -- to it. The chain lookup keys on (event_type, owner_scope, owner_id): were
  -- owner_id allowed to name a DIFFERENT account, that other account's chain
  -- would fetch this rule and run its actions against a tenant that never
  -- wrote it — the same class of cross-tenant leak the flowByKey account
  -- filter already cost this repo once. Same shape as flows'
  -- flow_conta_e_dona_de_si.
  CONSTRAINT reaction_account_owns_itself CHECK (
    owner_scope <> 'account' OR owner_id = account_id
  )
);

-- The idempotency key is unique WITHIN an account, not globally as in `flows`.
-- Global uniqueness is where flows' cross-tenant leak came from: the key is
-- client-supplied and passed through verbatim, so account B sending a key
-- account A had already used collides with a row B is not allowed to see, and
-- the conflict path hands it over. Scoping the uniqueness means that collision
-- cannot happen at all — B's key is B's — and the adapter's conflict lookup
-- filters by account anyway: one guard is the invariant, the other is what
-- stops a future query from reading around it.
--
-- COALESCE with the nil uuid because NULL never collides in a unique index, and
-- the platform level (account_id IS NULL) needs its keys to collide with each
-- other. Same shape, and the same reason, as flows_um_por_nivel.
CREATE UNIQUE INDEX reaction_rules_idempotency ON reaction_rules
  (COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid), idempotency_key);

-- The resolution's query: every rule of the chain for one event type.
CREATE INDEX reaction_rules_lookup ON reaction_rules (event_type, owner_scope, owner_id)
  WHERE enabled;

-- The idempotency gate. A row is written ONLY on success, so a redelivery
-- re-runs exactly what failed and skips what did not.
--
-- rule_ref is the rule's id, or `<flow_id>/<version>/<stage_key>/<moment>` when
-- the decider was a stage. One column and not two because the executor does not
-- care which decided — it cares that this action, for this event, already ran.
CREATE TABLE applied_actions (
  event_id    uuid NOT NULL,
  rule_ref    text NOT NULL,
  action_name text NOT NULL,
  applied_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (event_id, rule_ref, action_name)
);

-- +goose Down
DROP TABLE applied_actions;
DROP TABLE reaction_rules;
DROP TYPE reaction_trigger;
