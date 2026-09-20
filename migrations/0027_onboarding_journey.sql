-- +goose Up
-- The onboarding journey (spec 2026-09-20): what the journey records on the
-- person and on the personal account, and the two catalogues it reads.
--
-- Both catalogues are seeded HERE, as upserts, the way 0005 seeds the platform
-- flow: a re-run edits and never duplicates, and an environment that runs its
-- migrations has nothing else to run.

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

INSERT INTO plan_catalog (key, name, tagline, features, sort) VALUES
  ('free',       'Free',       'Try the platform on your own projects.',
     '["1 workspace","3 projects","community support"]', 10),
  ('start',      'Start',      'For one developer shipping every week.',
     '["unlimited projects","hosted agent","e-mail support"]', 20),
  ('pro',        'Pro',        'For teams that share flows and integrations.',
     '["everything in Start","organizations and members","flow sharing","priority support"]', 30),
  ('enterprise', 'Enterprise', 'For companies with their own rules.',
     '["everything in Pro","self-hosted providers","dedicated support","custom terms"]', 40)
ON CONFLICT (key) DO UPDATE SET
  name = EXCLUDED.name, tagline = EXCLUDED.tagline, features = EXCLUDED.features,
  sort = EXCLUDED.sort, active = true;

-- gitlab_self_hosted is operated: the GitLab adapter takes any base URL, so a
-- self-hosted instance is the same code path with base_url set.
INSERT INTO provider_catalog
  (key, category, name, credential_kind, permissions, needs_base_url, operated, docs_url, brand_color, sort) VALUES
  ('github', 'git', 'GitHub', 'token',
     '{"repo","read:org","read:user"}', false, true,
     'https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens', '#24292F', 10),
  ('gitlab', 'git', 'GitLab', 'token',
     '{"api","read_repository","write_repository"}', false, true,
     'https://docs.gitlab.com/user/profile/personal_access_tokens/', '#FC6D26', 20),
  ('gitlab_self_hosted', 'git', 'GitLab self-hosted', 'token',
     '{"api","read_repository","write_repository"}', true, true,
     'https://docs.gitlab.com/user/profile/personal_access_tokens/', '#FC6D26', 30),
  ('bitbucket', 'git', 'Bitbucket', 'token',
     '{"repository:read","repository:write","pullrequest:write"}', false, false,
     'https://support.atlassian.com/bitbucket-cloud/docs/access-tokens/', '#2684FF', 40),
  ('azure_devops', 'git', 'Azure DevOps', 'token',
     '{"vso.code_write","vso.project"}', true, false,
     'https://learn.microsoft.com/azure/devops/organizations/accounts/use-personal-access-tokens-to-authenticate', '#0078D4', 50),
  ('jira', 'task_manager', 'Jira', 'token',
     '{"read:jira-work","write:jira-work"}', true, false,
     'https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/', '#0052CC', 10),
  ('clickup', 'task_manager', 'ClickUp', 'token',
     '{}', false, false,
     'https://developer.clickup.com/docs/authentication', '#7B68EE', 20)
ON CONFLICT (key) DO UPDATE SET
  category = EXCLUDED.category, name = EXCLUDED.name,
  credential_kind = EXCLUDED.credential_kind, permissions = EXCLUDED.permissions,
  needs_base_url = EXCLUDED.needs_base_url, operated = EXCLUDED.operated,
  docs_url = EXCLUDED.docs_url, brand_color = EXCLUDED.brand_color,
  sort = EXCLUDED.sort, active = true;

-- +goose Down
ALTER TABLE accounts DROP COLUMN plan_key;
ALTER TABLE users
  DROP COLUMN birth_date, DROP COLUMN locale, DROP COLUMN timezone,
  DROP COLUMN phone, DROP COLUMN phone_verified_at,
  DROP COLUMN onboarding, DROP COLUMN onboarded_at;
DROP TABLE provider_catalog;
DROP TABLE plan_catalog;
