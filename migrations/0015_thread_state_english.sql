-- The thread state stops speaking Portuguese.
--
-- `demand_threads.state` held 'aberta'/'ativa'/'bloqueada'/'concluida'. Those
-- are values the CODE compares against, so under the house language rule they
-- belong in English like the rest of it. They are not user-facing text: what a
-- person reads is the cockpit's label, resolved from a translation catalogue.
--
-- The order matters: drop the constraint, rewrite the rows, then put the
-- constraint back. Rewriting first would trip the old CHECK.

ALTER TABLE demand_threads DROP CONSTRAINT IF EXISTS demand_threads_state_check;
ALTER TABLE demand_threads ALTER COLUMN state DROP DEFAULT;

UPDATE demand_threads SET state = CASE state
  WHEN 'aberta'    THEN 'open'
  WHEN 'ativa'     THEN 'active'
  WHEN 'bloqueada' THEN 'blocked'
  WHEN 'concluida' THEN 'concluded'
  ELSE state
END;

ALTER TABLE demand_threads ALTER COLUMN state SET DEFAULT 'open';
ALTER TABLE demand_threads ADD CONSTRAINT demand_threads_state_check
  CHECK (state IN ('open','active','blocked','concluded'));

-- The partial index feeds the attention box query (blocked threads, oldest
-- first). Its predicate is part of the index: with the old value it would stop
-- matching any row and the query would silently fall back to a sequential scan.
DROP INDEX IF EXISTS demand_threads_blocked_idx;
CREATE INDEX demand_threads_blocked_idx ON demand_threads (account_id, updated_at)
  WHERE state = 'blocked';

-- +goose StatementBegin
-- The thread does not die in silence.
--
-- Concluding requires a published finding. The rule also lives in the domain,
-- with a better message; here it is an INVARIANT — no path, not even one nobody
-- anticipated, closes an investigation without leaving a durable record. Same
-- pattern as "every account has an active owner".
CREATE OR REPLACE FUNCTION demand_thread_requires_finding() RETURNS trigger AS $$
BEGIN
  IF NEW.state = 'concluded' AND OLD.state IS DISTINCT FROM 'concluded' THEN
    IF NOT EXISTS (SELECT 1 FROM demand_findings f WHERE f.thread_id = NEW.id) THEN
      RAISE EXCEPTION
        'thread % cannot be concluded without a published finding', NEW.key;
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
