-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- What the platform has learned about its own failures (spec 2026-09-13 §3).
--
-- The signature is (consumer, code) and NOT (consumer, message). `errs.Error`
-- carries a stable Code separate from the developer-facing message precisely
-- because the message is free text that changes; comparing messages would make
-- the learning brittle, comparing codes makes it exact.
--
-- `classified_by` is what protects a human's judgement. A mark somebody placed
-- by hand survives an accidental success, because they marked it for a reason
-- the system cannot see.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TABLE error_signatures (
  consumer        text NOT NULL,
  code            text NOT NULL,
  classification  text NOT NULL,
  exhausted_count integer NOT NULL DEFAULT 0,
  last_success_at timestamptz,
  classified_by   text NOT NULL,
  first_seen      timestamptz NOT NULL DEFAULT now(),
  last_seen       timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (consumer, code),

  CONSTRAINT error_signature_classification_is_known
    CHECK (classification IN ('recoverable', 'irrecoverable', 'unknown')),
  CONSTRAINT error_signature_origin_is_known
    CHECK (classified_by IN ('seed', 'learned', 'human'))
);

-- Retention: none, deliberately, and that is the exception worth stating. This
-- table is SMALL — one row per (consumer, code) pair the platform has ever
-- seen, not one per failure — and its value is precisely that it remembers
-- across time. Trimming it would throw away the learning it exists to hold.
COMMENT ON TABLE error_signatures IS
  'What the platform learned about its failures. One row per (consumer, code); grows with the VOCABULARY of failures, not with their number. No retention on purpose.';

-- +goose Down
DROP TABLE error_signatures;
