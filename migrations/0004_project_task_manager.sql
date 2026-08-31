-- +goose Up
-- O vínculo do projeto com o gerenciador de tarefas merece tabela própria.
--
-- Ele estava sendo empacotado em JSON dentro de projects.rules — funcionava,
-- mas escondia um relacionamento real: o projeto aponta para UMA integração de
-- task manager e para um espaço/projeto DENTRO dela. Sem FK, nada impedia
-- apontar para integração inexistente ou de outra conta.
--
-- Um projeto tem no máximo um task manager (PK no project_id).
CREATE TABLE project_task_managers (
  project_id          uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  integration_id      uuid NOT NULL REFERENCES resources(id),
  external_space_id   text NOT NULL,
  external_project_id text NOT NULL,
  -- tipos de card vêm do provedor e são dinâmicos (ADR-0013)
  card_types          text[] NOT NULL DEFAULT '{}',
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX project_task_managers_integration_idx
  ON project_task_managers (integration_id);

-- Migra o que já estiver no envelope JSON, se houver.
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

-- rules volta a ser o que o nome diz: uma lista de regras.
UPDATE projects
   SET rules = COALESCE(rules->'rules', '[]'::jsonb)
 WHERE jsonb_typeof(rules) = 'object';

-- +goose Down
UPDATE projects SET rules = jsonb_build_object('rules', rules) WHERE jsonb_typeof(rules) = 'array';
DROP TABLE IF EXISTS project_task_managers;
