-- +goose Up
-- The project's link to its task manager deserves a table of its own.
--
-- It was being packed into JSON inside projects.rules — it worked, but it hid a
-- real relationship: the project points at ONE task manager integration and at
-- a space/project INSIDE it. With no FK, nothing stopped it from pointing at a
-- nonexistent integration or one of another account.
--
-- A project has at most one task manager (the PK is project_id).
CREATE TABLE project_task_managers (
  project_id          uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  integration_id      uuid NOT NULL REFERENCES resources(id),
  external_space_id   text NOT NULL,
  external_project_id text NOT NULL,
  -- card types come from the provider and are dynamic (ADR-0009)
  card_types          text[] NOT NULL DEFAULT '{}',
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX project_task_managers_integration_idx
  ON project_task_managers (integration_id);

-- Migrate whatever is already in the JSON envelope, if anything.
INSERT INTO project_task_managers (project_id, integration_id, external_space_id, external_project_id, card_types)
SELECT p.id,
       (p.rules->'task_manager'->>'integration_id')::uuid,
       COALESCE(p.rules->'task_manager'->>'external_space_id', ''),
       COALESCE(p.rules->'task_manager'->>'external_project_id', ''),
       COALESCE(ARRAY(SELECT jsonb_array_elements_text(p.rules->'task_manager'->'card_types')), '{}')
  FROM projects p
 WHERE jsonb_typeof(p.rules) = 'object'
   AND p.rules ? 'task_manager'
   AND p.rules->'task_manager'->>'integration_id' IS NOT NULL
ON CONFLICT (project_id) DO NOTHING;

-- rules goes back to being what its name says: a list of rules.
UPDATE projects
   SET rules = COALESCE(rules->'rules', '[]'::jsonb)
 WHERE jsonb_typeof(rules) = 'object';

-- +goose Down
UPDATE projects SET rules = jsonb_build_object('rules', rules) WHERE jsonb_typeof(rules) = 'array';
DROP TABLE IF EXISTS project_task_managers;
