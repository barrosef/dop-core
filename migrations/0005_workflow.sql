-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Fluxo de trabalho dinâmico, tipado e herdável (ADR-0014).
--
-- Duas decisões moldam este schema, e as duas são invariantes — não convenção:
--
--  1. VERSÃO É IMUTÁVEL. `flows` guarda a IDENTIDADE do fluxo (quem é o dono,
--     qual a versão corrente) e `flow_versions` guarda o CONTEÚDO, uma linha
--     por versão, que nunca muda depois de gravada. Uma demanda em andamento
--     congela (flow_id, version) ao iniciar: alterar a linha no lugar
--     reescreveria o passado dela e o histórico deixaria de explicar o que
--     aconteceu de fato. A trigger no fim do arquivo recusa o UPDATE.
--
--  2. UM FLUXO POR NÍVEL. A cadeia plataforma ◁ conta ◁ workspace ◁ projeto ◁
--     demanda precisa render UM fluxo efetivo. Dois fluxos concorrentes no
--     mesmo nível transformariam a resolução em escolha arbitrária — então o
--     índice único impede que existam. Evoluir um fluxo é versionar; ter outro
--     fluxo é declarar num nível diferente.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TYPE flow_scope AS ENUM ('platform', 'account', 'workspace', 'project', 'demand');

CREATE TABLE flows (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- NULL apenas no catálogo da plataforma (nível 0), que não tem dono e vale
  -- para todas as contas. Todo o resto carrega conta com FK — isolamento
  -- multi-tenant é constraint, não convenção.
  account_id      uuid REFERENCES accounts(id) ON DELETE CASCADE,
  owner_scope     flow_scope NOT NULL,
  -- Dono POLIMÓRFICO: aponta para conta, workspace, projeto ou demanda
  -- conforme o escopo. Sem FK porque não há uma tabela só para referenciar —
  -- quem confere a linhagem é o domínio, contra a árvore da conta, antes de
  -- gravar (ver workflow.Service.resolveOwner).
  owner_id        uuid,
  current_version int NOT NULL DEFAULT 1,
  -- Toda escrita carrega chave de idempotência (ADR-0017): repetir a criação
  -- colide aqui e devolve o fluxo já criado, em vez de criar um gêmeo.
  idempotency_key text NOT NULL UNIQUE,
  created_by      uuid REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),

  -- Catálogo da plataforma e fluxo de conta são exclusivos: ou tem dono e
  -- conta, ou é nível 0 e não tem nenhum dos dois. O estado intermediário
  -- ("fluxo de workspace sem conta") não significa nada e por isso não existe.
  CONSTRAINT flow_plataforma_sem_dono CHECK (
    (owner_scope =  'platform' AND account_id IS NULL     AND owner_id IS NULL) OR
    (owner_scope <> 'platform' AND account_id IS NOT NULL AND owner_id IS NOT NULL)
  ),
  -- Fluxo de conta é dono de si mesmo: owner_id É a conta. Guardar outro id
  -- aqui abriria a porta para um fluxo pendurado na conta errada.
  CONSTRAINT flow_conta_e_dona_de_si CHECK (
    owner_scope <> 'account' OR owner_id = account_id
  )
);

-- A regra "um fluxo por nível", como índice. COALESCE porque NULL não colide
-- em índice único e o catálogo da plataforma poderia nascer duplicado.
CREATE UNIQUE INDEX flows_um_por_nivel
  ON flows (owner_scope, COALESCE(owner_id, '00000000-0000-0000-0000-000000000000'::uuid));

-- A consulta da resolução: os fluxos dos cinco níveis da cadeia, numa varredura.
CREATE INDEX flows_account_scope_idx ON flows (account_id, owner_scope);

CREATE TABLE flow_versions (
  flow_id         uuid NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
  version         int  NOT NULL,
  -- Nome e descrição são CONGELADOS junto com as etapas: renomear um fluxo
  -- também é reescrever o passado de quem já o adotou.
  name            text NOT NULL,
  description     text,
  -- As etapas, na ordem, como o autor compôs. JSONB e não tabela filha porque
  -- a versão é um DOCUMENTO imutável lido inteiro: normalizar traria o custo
  -- de junção e a tentação de editar uma etapa isolada — que é justamente o
  -- que não pode acontecer.
  stages          jsonb NOT NULL,
  idempotency_key text NOT NULL UNIQUE,
  created_by      uuid REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),

  -- (flow_id, version) é a chave que a demanda congela — e é para cá que a
  -- FK do domínio de demanda deve apontar quando ele nascer.
  PRIMARY KEY (flow_id, version),
  CONSTRAINT flow_version_positiva CHECK (version > 0),
  CONSTRAINT flow_version_tem_etapa CHECK (jsonb_typeof(stages) = 'array' AND jsonb_array_length(stages) > 0)
);

-- current_version aponta para uma versão que EXISTE. Sem isso, um bug de
-- aplicação deixaria o fluxo apontando para o vazio e a tela abriria em branco.
ALTER TABLE flows ADD CONSTRAINT flows_versao_corrente_existe
  FOREIGN KEY (id, current_version) REFERENCES flow_versions(flow_id, version)
  DEFERRABLE INITIALLY DEFERRED;

-- A invariante do congelamento, no banco.
--
-- UPDATE é sempre recusado: mudar um fluxo é gravar a versão seguinte, e
-- nenhum caminho — nem o que ninguém previu — pode reescrever o documento que
-- uma demanda em execução está seguindo.
--
-- DELETE é recusado enquanto o fluxo existir. A exceção é o cascade: quando a
-- conta ou o fluxo inteiro somem, o Postgres apaga o pai ANTES de propagar
-- para os filhos, e a versão órfã pode ir junto. Sem essa distinção, apagar
-- uma conta ficaria impossível — a proteção viraria um bug.
CREATE OR REPLACE FUNCTION assert_flow_version_congelada() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF EXISTS (SELECT 1 FROM flows WHERE id = OLD.flow_id) THEN
      RAISE EXCEPTION 'versão de fluxo não se apaga: a demanda que a congelou perderia o próprio histórico';
    END IF;
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'versão de fluxo é imutável: alterar um fluxo GERA versão nova (ADR-0014 §4)';
END $$ LANGUAGE plpgsql;

CREATE TRIGGER flow_versions_imutaveis
  BEFORE UPDATE OR DELETE ON flow_versions
  FOR EACH ROW EXECUTE FUNCTION assert_flow_version_congelada();

-- ── catálogo da plataforma (ADR-0014 §8) ────────────────────────────────────
-- O nível 0 da cadeia, semeado aqui e não por RPC: ele vale para TODAS as
-- contas, e conta nenhuma deveria conseguir escrevê-lo. É o que garante que
-- ResolveFlow sempre tenha uma base — uma conta recém-criada já trabalha.
-- As duas linhas nascem no MESMO comando de propósito: flows aponta para a
-- versão corrente e flow_versions aponta de volta para o fluxo — a referência
-- é circular por construção. A FK adiada resolve isso dentro de uma transação,
-- e o CTE garante que exista uma, mesmo quando o arquivo é aplicado fora do
-- goose (psql em autocommit, que é como a migração é conferida no cluster local).
WITH catalogo AS (
  INSERT INTO flows (id, account_id, owner_scope, owner_id, current_version, idempotency_key)
  VALUES ('0000f10a-0000-4000-8000-000000000001', NULL, 'platform', NULL, 1, 'seed:platform:flow')
  RETURNING id
)
INSERT INTO flow_versions (flow_id, version, name, description, stages, idempotency_key)
SELECT catalogo.id, 1,
  'Fluxo padrão da plataforma',
  'Contexto → spec → plano → implementação → teste → validação humana → finalização. Herdado por toda conta que não declarar o seu.',
  '[
    {"key":"contexto",          "name":"Contexto",          "type":"context",          "artifacts":["document"],  "gate":"none",  "subtypes":[]},
    {"key":"spec",              "name":"Spec",              "type":"spec",             "artifacts":["spec"],      "gate":"none",  "subtypes":[]},
    {"key":"plano",             "name":"Plano",             "type":"plan",             "artifacts":["plan"],      "gate":"none",  "subtypes":[]},
    {"key":"implementacao",     "name":"Implementação",     "type":"implementation",   "artifacts":[],            "gate":"none",  "subtypes":[]},
    {"key":"teste",             "name":"Teste",             "type":"test",             "artifacts":["test_plan"], "gate":"none",  "subtypes":["aaa","e2e","integracao"]},
    {"key":"validacao-humana",  "name":"Validação humana",  "type":"human_validation", "artifacts":["report"],    "gate":"human", "subtypes":[]},
    {"key":"finalizacao",       "name":"Finalização",       "type":"finalization",     "artifacts":["report"],    "gate":"none",  "subtypes":[]}
  ]'::jsonb,
  'seed:platform:flow:1'
FROM catalogo;

-- +goose Down
DROP TRIGGER IF EXISTS flow_versions_imutaveis ON flow_versions;
DROP FUNCTION IF EXISTS assert_flow_version_congelada();
ALTER TABLE IF EXISTS flows DROP CONSTRAINT IF EXISTS flows_versao_corrente_existe;
DROP TABLE IF EXISTS flow_versions;
DROP TABLE IF EXISTS flows;
DROP TYPE IF EXISTS flow_scope;
