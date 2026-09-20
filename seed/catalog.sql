-- The plan and provider catalogues the onboarding journey reads
-- (spec 2026-09-20 D-4, D-6). Idempotent: edit a row here and restart the
-- worker; never a migration.

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
