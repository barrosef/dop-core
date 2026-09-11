-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- The rate limit for the verification message (spec SP-0 US-2.5).
--
-- Why a table of its own, and not a column on `users`: AT THIS MOMENT THERE IS
-- NO USER. `EnsureUser` refuses a password credential whose e-mail was never
-- verified (D-5) and refuses it BEFORE creating anything — so the person asking
-- for the message has a Firebase credential and nothing on this side. A limit
-- keyed on a user, an account or a membership would have nothing to match on.
--
-- The key is therefore the e-mail, which is the only stable thing we hold. The
-- subject is stored beside it for forensics, not for lookup: two Firebase
-- credentials on the same address is exactly the shape of somebody probing.
--
-- WHAT THIS IS NOT: it is not a log of sends and nothing reads it for history.
-- It answers one question — "how many times, and how recently, for this
-- address" — and rows older than the window are noise. Hence the retention
-- statement below, written now rather than discovered when the table is large:
-- a rate-limit table with no retention is a table that grows forever from
-- traffic nobody is accountable for.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TABLE email_verification_requests (
  id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email   citext      NOT NULL,
  subject text        NOT NULL,
  sent_at timestamptz NOT NULL DEFAULT now(),

  -- An address is an address. Anything else here is a caller sending junk, and
  -- the cheapest place to refuse it is the column.
  CONSTRAINT email_verification_email_is_an_address CHECK (position('@' IN email) > 1)
);

-- The only query this table serves: how many, and how recent, for one address.
CREATE INDEX email_verification_requests_by_email
  ON email_verification_requests (email, sent_at DESC);

-- Retention: swept by the same routine that trims the other volatile tables.
-- Anything past the widest rate-limit window has no reader.
COMMENT ON TABLE email_verification_requests IS
  'Rate limit for the e-mail verification message. Rows older than 24h have no reader and may be deleted.';

-- +goose Down
DROP TABLE email_verification_requests;
