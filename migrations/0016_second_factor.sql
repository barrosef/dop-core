-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- The second factor (ADR-0027).
--
-- It is the PLATFORM's, not the identity provider's: the seed of a TOTP is a
-- credential and lives in the vault, the code goes out through a channel we
-- already own, and the gate has to answer the same way whichever provider does
-- the first factor.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TYPE second_factor_kind AS ENUM ('totp', 'email', 'sms');
CREATE TYPE second_factor_status AS ENUM ('pending', 'active', 'revoked');

-- A factor belongs to the USER, not to the account: the person carries it
-- between the accounts they are a member of. What belongs to the account is the
-- REQUIREMENT (accounts.require_second_factor, below).
CREATE TABLE second_factors (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind         second_factor_kind NOT NULL,
  status       second_factor_status NOT NULL DEFAULT 'pending',
  -- The person's own name for it ("iPhone", "work e-mail"): with three kinds and
  -- more than one device, the list is unusable without it.
  label        text NOT NULL,
  -- TOTP only: the OPAQUE reference into the vault. The seed never lands here —
  -- a column with a seed is a column that leaks in a backup, in a dump and in
  -- the first careless SELECT.
  secret_ref   text,
  -- email/sms only: where the code goes. Stored WHOLE because it has to be
  -- reachable; the edge is what masks it.
  destination  text,
  -- pending until possession is proven. A factor that is registered without
  -- being proven is a lock whose key nobody has tested — and it is discovered on
  -- the day of the sign-in that fails.
  confirmed_at timestamptz,
  last_used_at timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT totp_has_secret_ref   CHECK (kind <> 'totp' OR secret_ref  IS NOT NULL),
  CONSTRAINT code_has_destination  CHECK (kind =  'totp' OR destination IS NOT NULL),
  CONSTRAINT active_was_confirmed  CHECK (status <> 'active' OR confirmed_at IS NOT NULL)
);
CREATE INDEX second_factors_user_idx ON second_factors (user_id);
-- The same destination twice is not a second factor, it is the same factor
-- registered twice: it would double the SMS bill and give the person two
-- identical rows to tell apart. Revoked ones stay out of the rule — re-enrolling
-- a number that was removed is legitimate.
CREATE UNIQUE INDEX second_factors_live_uniq
  ON second_factors (user_id, kind, coalesce(destination, ''))
  WHERE status <> 'revoked';

-- A challenge is ONE attempt to prove possession — for enrolment or for a
-- step-up. TOTP has no code to store (the proof is computed from the seed), and
-- the row exists all the same: it is what makes the attempt counter and the
-- cool-off uniform across the three kinds.
CREATE TABLE second_factor_challenges (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  factor_id   uuid NOT NULL REFERENCES second_factors(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- 'enrollment' | 'step_up'. A challenge issued to enrol does not activate a
  -- session, and one issued to sign in does not confirm a factor.
  purpose     text NOT NULL,
  -- The HASH of the code, never the code. NULL for TOTP.
  --
  -- It is a plain SHA-256 salted with the factor's id, and that is deliberate:
  -- this is not a password. It is six digits that live for ten minutes, are
  -- single use and die after five attempts. The hash exists so that reading the
  -- table does not hand over a LIVE code, and the salt stops one rainbow table
  -- from serving every row.
  code_hash   bytea,
  attempts    int NOT NULL DEFAULT 0,
  consumed_at timestamptz,
  expires_at  timestamptz NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX second_factor_challenges_factor_idx ON second_factor_challenges (factor_id, created_at DESC);
CREATE INDEX second_factor_challenges_expiry_idx ON second_factor_challenges (expires_at) WHERE consumed_at IS NULL;

-- Ten single-use codes, the way back when the factor is lost.
--
-- Without them a lost phone becomes a support ticket, and support becomes the
-- bypass: an attacker who convinces a human is worth more than one who breaks
-- TOTP. These ARE high-entropy secrets, so the hash is over the whole value.
CREATE TABLE recovery_codes (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_hash  bytea NOT NULL,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX recovery_codes_uniq ON recovery_codes (user_id, code_hash);
CREATE INDEX recovery_codes_user_idx ON recovery_codes (user_id) WHERE used_at IS NULL;

-- The step-up: the session that has already answered, and until when.
--
-- Keyed by (user, session) and not by user alone: two open sessions are two
-- doors, and one of them answering must not open the other. The session's
-- identifier comes from the edge, in the metadata, like x-actor-id — with the
-- limit that P-18 describes and this feature makes load-bearing.
CREATE TABLE step_ups (
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id  text NOT NULL,
  method      text NOT NULL,          -- totp | email | sms | recovery_code
  verified_at timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  PRIMARY KEY (user_id, session_id)
);
CREATE INDEX step_ups_expiry_idx ON step_ups (expires_at);

-- ── the account's policy ────────────────────────────────────────────────────
--
-- The account is the boundary of isolation (ADR-0002), so it is the boundary of
-- the requirement: an organization requires a factor of its members, and a
-- member with none still operates their PERSONAL account.
ALTER TABLE accounts
  ADD COLUMN IF NOT EXISTS require_second_factor boolean NOT NULL DEFAULT false,
  -- Which kinds this account accepts. SMS is the weakest of the three (SIM
  -- swap) and the only one that costs money per attempt; an account that does
  -- not want it takes it out of the list without the platform having to grow a
  -- second flag for every future kind.
  ADD COLUMN IF NOT EXISTS allowed_second_factors second_factor_kind[] NOT NULL
      DEFAULT ARRAY['totp','email','sms']::second_factor_kind[];

ALTER TABLE accounts
  ADD CONSTRAINT allowed_second_factors_not_empty
  CHECK (array_length(allowed_second_factors, 1) >= 1);

-- +goose Down
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS allowed_second_factors_not_empty;
ALTER TABLE accounts DROP COLUMN IF EXISTS allowed_second_factors;
ALTER TABLE accounts DROP COLUMN IF EXISTS require_second_factor;
DROP TABLE IF EXISTS step_ups;
DROP TABLE IF EXISTS recovery_codes;
DROP TABLE IF EXISTS second_factor_challenges;
DROP TABLE IF EXISTS second_factors;
DROP TYPE IF EXISTS second_factor_status;
DROP TYPE IF EXISTS second_factor_kind;
