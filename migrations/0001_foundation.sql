-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Foundation: identity, ownership, resources, hierarchy — and the EVENT SPINE.
--
-- The rule that crosses the whole schema: every domain table carries account_id
-- with an FK. Multi-tenant isolation is a constraint, not an application
-- convention.
-- ════════════════════════════════════════════════════════════════════════════

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS citext;

-- ── identity and ownership ──────────────────────────────────────────────────
CREATE TABLE users (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  subject        text NOT NULL UNIQUE,        -- from the IdentityProvider, normalized
  email          citext,
  email_verified boolean NOT NULL DEFAULT false,
  name           text,
  avatar_url     text,
  providers      text[] NOT NULL DEFAULT '{}',
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_uniq ON users (lower(email)) WHERE email IS NOT NULL;

CREATE TYPE account_kind AS ENUM ('personal', 'organization');

CREATE TABLE accounts (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind          account_kind NOT NULL,
  -- Individuals and organizations share the same name space (ADR-0002)
  handle        text NOT NULL UNIQUE,
  display_name  text NOT NULL,
  legal_id      text,                         -- the company's registration number
  legal_name    text,
  verified_domain text,                       -- NULL = not verified
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_tem_legal_id CHECK (kind <> 'organization' OR legal_id IS NOT NULL)
);

CREATE TYPE member_role AS ENUM ('owner', 'admin', 'developer', 'viewer');

CREATE TABLE memberships (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  role       member_role NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, account_id)
);
CREATE INDEX memberships_account_idx ON memberships (account_id);

-- Invariant: every account has at least one active owner. Checked by a trigger
-- because it is a business rule NO operation may violate (spec §5).
CREATE OR REPLACE FUNCTION assert_account_has_owner() RETURNS trigger AS $$
DECLARE owners int;
BEGIN
  SELECT count(*) INTO owners
    FROM memberships
   WHERE account_id = COALESCE(OLD.account_id, NEW.account_id)
     AND role = 'owner'
     AND (TG_OP <> 'DELETE' OR id <> OLD.id);
  IF owners = 0 THEN
    RAISE EXCEPTION 'the account would be left with no active owner';
  END IF;
  RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER memberships_owner_guard
  AFTER UPDATE OR DELETE ON memberships
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION assert_account_has_owner();

CREATE TYPE invite_status AS ENUM ('pending', 'accepted', 'expired', 'revoked');

CREATE TABLE invites (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  email        citext NOT NULL,
  role         member_role NOT NULL,
  grants       jsonb NOT NULL DEFAULT '[]',   -- composed IN THE INVITE, with no default
  token_hash   text NOT NULL UNIQUE,
  status       invite_status NOT NULL DEFAULT 'pending',
  invited_by   uuid REFERENCES users(id),
  expires_at   timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX invites_account_idx ON invites (account_id, status);

-- ── resources (ADR-0013) ────────────────────────────────────────────────────
CREATE TYPE resource_kind AS ENUM ('integration', 'skill', 'workflow', 'git_flow');

CREATE TABLE resources (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id     uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  kind           resource_kind NOT NULL,
  name           text NOT NULL,
  version        int NOT NULL DEFAULT 1,
  config         jsonb NOT NULL DEFAULT '{}',
  -- an opaque pointer into the SecretStore — the secret NEVER lives here
  credential_ref text,
  status         text NOT NULL DEFAULT 'active',
  created_by     uuid REFERENCES users(id),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, kind, name)
);
CREATE INDEX resources_account_kind_idx ON resources (account_id, kind);

CREATE TABLE resource_grants (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  resource_id uuid NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  level       text NOT NULL CHECK (level IN ('use', 'manage')),
  granted_by  uuid REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (resource_id, user_id)
);

-- ── hierarchy ───────────────────────────────────────────────────────────────
CREATE TABLE workspaces (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name        text NOT NULL,
  key         text,
  description text,
  tags        text[] NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workspaces_account_idx ON workspaces (account_id);

CREATE TABLE projects (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  name         text NOT NULL,
  description  text,
  rules        jsonb NOT NULL DEFAULT '[]',
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX projects_workspace_idx ON projects (workspace_id);

-- The provider belongs to the REPOSITORY, not to the project: GitHub and GitLab
-- coexist.
CREATE TABLE project_repos (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  integration_id uuid NOT NULL REFERENCES resources(id),
  external_id    text NOT NULL,
  name           text NOT NULL,
  default_branch text NOT NULL DEFAULT 'main',
  pr_targets     text[] NOT NULL DEFAULT '{}',
  UNIQUE (project_id, integration_id, external_id)
);

CREATE TABLE project_resources (
  project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  resource_id uuid NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  PRIMARY KEY (project_id, resource_id)
);

-- ════════════════════════════════════════════════════════════════════════════
-- THE SPINE: the event log + the outbox (ADR-0006 and ADR-0019)
--
-- The truth is the log. The dossier, the timeline, the audit trail, the metrics
-- and the attention box are PROJECTIONS — reads, never writes of their own.
-- ════════════════════════════════════════════════════════════════════════════
CREATE TABLE events (
  id           uuid NOT NULL DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL,
  aggregate    text NOT NULL,           -- demand, resource, account...
  aggregate_id text NOT NULL,
  type         text NOT NULL,           -- demand.stage.advanced, cost.recorded...
  payload      jsonb NOT NULL DEFAULT '{}',
  actor_kind   text,
  actor_id     text,
  -- the credential USED in the action: it is what answers "who authorized this push?"
  credential_ref text,
  request_id   text,
  occurred_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX events_aggregate_idx ON events (aggregate, aggregate_id, occurred_at DESC);
CREATE INDEX events_account_idx   ON events (account_id, occurred_at DESC);
CREATE INDEX events_type_idx      ON events (type, occurred_at DESC);

-- The current partition and the next one; the sched mode creates the rest.
CREATE TABLE events_2026_08 PARTITION OF events
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE events_2026_09 PARTITION OF events
  FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE events_2026_10 PARTITION OF events
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

-- Outbox: written in the SAME transaction as the state. It is what gives
-- atomicity with no 2PC.
CREATE TABLE outbox (
  event_id     uuid PRIMARY KEY,
  occurred_at  timestamptz NOT NULL,
  subject      text NOT NULL,          -- the NATS subject derived from the type
  payload      jsonb NOT NULL,
  published_at timestamptz,            -- NULL = pending
  attempts     int NOT NULL DEFAULT 0,
  last_error   text
);
-- A partial index: the relay only sweeps what is left to publish.
CREATE INDEX outbox_pending_idx ON outbox (occurred_at) WHERE published_at IS NULL;

-- Idempotency: repeating a write returns the same answer (ADR-0017).
CREATE TABLE idempotency (
  key          text PRIMARY KEY,
  request_hash text NOT NULL,
  response     jsonb,
  completed_at timestamptz,
  expires_at   timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idempotency_expiry_idx ON idempotency (expires_at);

-- +goose Down
DROP TABLE IF EXISTS idempotency, outbox CASCADE;
DROP TABLE IF EXISTS events CASCADE;
DROP TABLE IF EXISTS project_resources, project_repos, projects, workspaces CASCADE;
DROP TABLE IF EXISTS resource_grants, resources CASCADE;
DROP TABLE IF EXISTS invites, memberships, accounts, users CASCADE;
DROP TYPE IF EXISTS resource_kind, invite_status, member_role, account_kind;
DROP FUNCTION IF EXISTS assert_account_has_owner();
