-- +goose Up
-- The onboarding journey (spec 2026-09-20): what the journey records on the
-- person and on the personal account, and the two catalogues it reads.
--
-- The catalogues' rows are NOT here: they are data an environment loads, and
-- they live in seed/catalog.sql (ADR-0024 §3), applied after the migrations
-- and re-applied to edit.

CREATE TABLE plan_catalog (
  key       text PRIMARY KEY,
  name      text NOT NULL,
  tagline   text NOT NULL DEFAULT '',
  features  jsonb NOT NULL DEFAULT '[]',
  sort      int  NOT NULL DEFAULT 0,
  active    boolean NOT NULL DEFAULT true
);

-- category mirrors resource.Category (git | task_manager): a provider row is
-- what the cockpit shows as a tile and what the core knows how to check.
-- `operated` is the honest flag — false means the credential is stored and
-- nothing is done with it yet.
CREATE TABLE provider_catalog (
  key             text PRIMARY KEY,
  category        text NOT NULL CHECK (category IN ('git', 'task_manager')),
  name            text NOT NULL,
  credential_kind text NOT NULL DEFAULT 'token',
  permissions     text[] NOT NULL DEFAULT '{}',
  needs_base_url  boolean NOT NULL DEFAULT false,
  operated        boolean NOT NULL DEFAULT false,
  docs_url        text NOT NULL DEFAULT '',
  brand_color     text NOT NULL DEFAULT '',
  sort            int  NOT NULL DEFAULT 0,
  active          boolean NOT NULL DEFAULT true
);

ALTER TABLE users
  ADD COLUMN birth_date        date,
  ADD COLUMN locale            text,
  ADD COLUMN timezone          text,
  ADD COLUMN phone             text,
  -- Written by the second factor's confirmation of a factor whose destination
  -- is this phone, and by nothing else. Cleared whenever the phone changes.
  ADD COLUMN phone_verified_at timestamptz,
  -- {step: done|skipped}; the vocabulary of steps is the domain's.
  ADD COLUMN onboarding        jsonb NOT NULL DEFAULT '{}',
  ADD COLUMN onboarded_at      timestamptz;

ALTER TABLE accounts
  ADD COLUMN plan_key text REFERENCES plan_catalog(key);

-- A workspace key is unique within its account. It was a convention until the
-- personal workspace (D-3) needed it as a constraint: two logins racing to
-- create it must produce one row, and only the index can promise that.
CREATE UNIQUE INDEX workspaces_account_key_uniq ON workspaces (account_id, key) WHERE key IS NOT NULL;

-- +goose Down
DROP INDEX workspaces_account_key_uniq;
ALTER TABLE accounts DROP COLUMN plan_key;
ALTER TABLE users
  DROP COLUMN birth_date, DROP COLUMN locale, DROP COLUMN timezone,
  DROP COLUMN phone, DROP COLUMN phone_verified_at,
  DROP COLUMN onboarding, DROP COLUMN onboarded_at;
DROP TABLE provider_catalog;
DROP TABLE plan_catalog;
