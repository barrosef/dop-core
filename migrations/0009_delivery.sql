-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Entrega: evidência de verde, pull requests, fila de merge por repositório e
-- diretrizes de coordenação (ADR-0007, ADR-0008, ADR-0015).
--
-- Três invariantes deste domínio são caras demais para viverem só no código de
-- aplicação, e por isso estão aqui como constraint ou trigger:
--
--   1. SEM VERDE, SEM PR — não existe linha em pull_requests cujo commit não
--      tenha evidência aprovada (trigger `assert_pr_tem_verde`);
--   2. A FILA TEM ORDEM TOTAL — (priority, seq) com seq única por repositório;
--      duas entradas nunca empatam de forma ambígua;
--   3. DIRETRIZ COORDENA, NUNCA PAUSA — o vocabulário de ações é enum fechado
--      e a trigger recusa qualquer instrução fora dele (ADR-0015 §5).
--
-- Sobre demand_id: as referências à demanda são por UUID, sem FK. A tabela
-- `demands` pertence a outra migração, escrita em paralelo; amarrar FK aqui
-- criaria dependência de ordem entre trabalhos independentes. A conta continua
-- amarrada por FK em toda tabela — isolamento multi-tenant é constraint.
-- ════════════════════════════════════════════════════════════════════════════

-- ── evidência de verde (ADR-0007) ───────────────────────────────────────────

-- O parecer do crítico é um tipo de execução como os outros, de propósito: ele
-- tem commit, resultado e rastro. Adjetivo pendurado no PR não se audita.
CREATE TYPE verification_kind AS ENUM ('acceptance', 'unit', 'e2e', 'critic');

-- Não existe 'unknown': execução sem resultado não é evidência, é ruído.
CREATE TYPE verification_outcome AS ENUM ('passed', 'failed', 'errored');

CREATE TABLE verification_runs (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  demand_id    uuid NOT NULL,
  repo_id      uuid NOT NULL REFERENCES project_repos(id) ON DELETE CASCADE,
  -- O campo que dá valor a todo o resto: evidência que não diz sobre QUAL
  -- código rodou serve para provar qualquer coisa, e portanto não prova nada.
  commit_sha   text NOT NULL CHECK (commit_sha <> ''),
  kind         verification_kind NOT NULL,
  suite        text NOT NULL CHECK (suite <> ''),   -- o QUE rodou
  outcome      verification_outcome NOT NULL,
  total        int NOT NULL DEFAULT 0 CHECK (total  >= 0),
  passed       int NOT NULL DEFAULT 0 CHECK (passed >= 0),
  failed       int NOT NULL DEFAULT 0 CHECK (failed >= 0),
  -- Rastro: onde rodou e onde ficou o log (ponteiro no ObjectStore).
  sandbox_id   text NOT NULL DEFAULT '',
  log_ref      text NOT NULL DEFAULT '',
  detail       jsonb NOT NULL DEFAULT '{}',          -- parecer do crítico, motivos
  attempts     int NOT NULL DEFAULT 1 CHECK (attempts > 0),
  started_at   timestamptz,
  ended_at     timestamptz NOT NULL DEFAULT now(),
  idempotency_key text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  -- "Passou" sem onde conferir é palavra, não evidência.
  CONSTRAINT evidencia_tem_rastro CHECK (sandbox_id <> '' OR log_ref <> ''),
  -- Aprovado com falha é incoerente; deixar passar seria deixar entrar verde falso.
  CONSTRAINT verde_nao_tem_falha  CHECK (outcome <> 'passed' OR failed = 0),
  -- Rodar a mesma suíte de novo no mesmo commit ATUALIZA a linha e incrementa
  -- attempts: quantas vezes se tentou é parte da evidência.
  UNIQUE (account_id, demand_id, repo_id, commit_sha, kind, suite)
);

-- O índice do caminho quente: "este commit está verde?" é a pergunta que a
-- abertura do PR e a entrada na fila fazem toda vez.
CREATE INDEX verification_runs_evidencia_idx
  ON verification_runs (account_id, demand_id, repo_id, commit_sha);

-- ── pull requests ───────────────────────────────────────────────────────────

CREATE TABLE pull_requests (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  demand_id     uuid NOT NULL,
  repo_id       uuid NOT NULL REFERENCES project_repos(id) ON DELETE CASCADE,
  repo_name     text NOT NULL DEFAULT '',
  source_branch text NOT NULL CHECK (source_branch <> ''),
  target_branch text NOT NULL DEFAULT 'main',
  -- Commit de topo: é sobre ele que a evidência é cobrada, hoje e a cada
  -- re-verificação da fila.
  head_commit   text NOT NULL CHECK (head_commit <> ''),
  url           text NOT NULL DEFAULT '',
  external_id   text NOT NULL DEFAULT '',   -- número/id no provedor
  merged        boolean NOT NULL DEFAULT false,
  has_conflict  boolean NOT NULL DEFAULT false,
  reviewers     jsonb NOT NULL DEFAULT '[]',
  idempotency_key text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  -- Um PR por demanda em cada repositório. É o que o contrato de EnqueueMerge
  -- assume ao endereçar o PR por (repo_id, demand_id) — se um dia houver um PR
  -- por branch de destino, o contrato muda junto com esta constraint.
  UNIQUE (repo_id, demand_id)
);
CREATE INDEX pull_requests_conta_idx  ON pull_requests (account_id, merged);
CREATE INDEX pull_requests_demanda_idx ON pull_requests (demand_id);
CREATE UNIQUE INDEX pull_requests_idem_idx
  ON pull_requests (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- ADR-0007 como TRIGGER. O serviço já recusa antes, com mensagem que diz o que
-- falta; aqui é o anteparo que vale para os caminhos que ninguém previu — um
-- backfill, um script de operação, um bug de ordem de chamada. Regra de negócio
-- que NENHUMA operação pode violar não mora só na aplicação.
CREATE OR REPLACE FUNCTION assert_pr_tem_verde() RETURNS trigger AS $$
DECLARE aceitacao int; critico int; vermelho int;
BEGIN
  SELECT count(*) FILTER (WHERE kind = 'acceptance' AND outcome = 'passed'),
         count(*) FILTER (WHERE kind = 'critic'     AND outcome = 'passed'),
         count(*) FILTER (WHERE outcome <> 'passed')
    INTO aceitacao, critico, vermelho
    FROM verification_runs
   WHERE account_id = NEW.account_id
     AND demand_id  = NEW.demand_id
     AND repo_id    = NEW.repo_id
     AND commit_sha = NEW.head_commit;

  IF aceitacao = 0 THEN
    RAISE EXCEPTION 'sem verde, sem PR: nenhuma execução de aceitação aprovada para o commit %', NEW.head_commit;
  END IF;
  IF critico = 0 THEN
    RAISE EXCEPTION 'sem verde, sem PR: falta o parecer do crítico para o commit %', NEW.head_commit;
  END IF;
  IF vermelho > 0 THEN
    RAISE EXCEPTION 'sem verde, sem PR: % execução(ões) não aprovadas no commit %', vermelho, NEW.head_commit;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

-- Também no UPDATE de head_commit: o branch andar não pode contrabandear
-- código não verificado para dentro de um PR que já existe.
CREATE TRIGGER pull_requests_verde_guard
  BEFORE INSERT OR UPDATE OF head_commit ON pull_requests
  FOR EACH ROW EXECUTE FUNCTION assert_pr_tem_verde();

-- ── fila de merge por repositório (ADR-0008) ────────────────────────────────

CREATE TYPE merge_queue_state AS ENUM
  ('queued', 'rebasing', 'verifying', 'merged', 'conflict');

CREATE TABLE merge_queue_entries (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id      uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  repo_id         uuid NOT NULL REFERENCES project_repos(id) ON DELETE CASCADE,
  demand_id       uuid NOT NULL,
  pull_request_id uuid NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
  -- Sequência de chegada DENTRO do repositório. Atribuída sob lock da linha do
  -- repositório, é o desempate que garante ordem total.
  seq             bigint NOT NULL CHECK (seq > 0),
  -- Onde a diretriz de ordem preferencial atua (ADR-0015 §6). Menor entra
  -- antes. Reordenar não pausa ninguém: quem perdeu a vez continua correndo.
  priority        int NOT NULL DEFAULT 100,
  state           merge_queue_state NOT NULL DEFAULT 'queued',
  overlapping_files text[] NOT NULL DEFAULT '{}',   -- detecção do techlead
  conflict        jsonb,
  idempotency_key text,
  enqueued_at     timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  -- Um PR não entra duas vezes na fila do mesmo repositório. Constraint, não
  -- disciplina de código: dois enfileiramentos concorrentes do mesmo PR são
  -- exatamente o retry que o cliente faz quando a resposta se perde.
  UNIQUE (repo_id, pull_request_id),
  -- A ordem da fila é (priority, seq) e seq é única por repositório: não
  -- existe par de entradas cuja ordem relativa dependa de quem rodou o SELECT.
  UNIQUE (repo_id, seq),
  -- Conflito escalado sem relato é alarme cru: a caixa de atenção precisa do
  -- contexto para o humano decidir sem arqueologia (ADR-0008 §2).
  CONSTRAINT conflito_tem_relato CHECK (state <> 'conflict' OR conflict IS NOT NULL)
);
CREATE INDEX merge_queue_ordem_idx
  ON merge_queue_entries (account_id, repo_id, priority, seq);
CREATE UNIQUE INDEX merge_queue_idem_idx
  ON merge_queue_entries (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- ── diretrizes de coordenação (ADR-0015) ────────────────────────────────────

-- O vocabulário é FECHADO e não tem 'pause', 'block' nem 'suspend'. A regra de
-- ouro ("transversal identificada NUNCA pausa demanda") não é cuidado de quem
-- escreve o código: é a ausência de um valor no tipo.
CREATE TYPE directive_kind AS ENUM
  ('cherry_pick', 'merge_order', 'file_partition', 'cross_verify');

CREATE TYPE directive_status AS ENUM ('proposed', 'decided', 'superseded');

CREATE TABLE directives (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  kind         directive_kind NOT NULL,
  summary      text NOT NULL CHECK (summary <> ''),
  payload      jsonb NOT NULL DEFAULT '{}',       -- sinais do techlead
  affected_demands uuid[] NOT NULL DEFAULT '{}',
  -- Opções prontas, cada uma com as instruções que ela dispara.
  options      jsonb NOT NULL DEFAULT '[]',
  recommended  text NOT NULL,
  status       directive_status NOT NULL DEFAULT 'proposed',
  decided_option text,
  rationale    text,
  decided_by   uuid REFERENCES users(id),
  decided_at   timestamptz,
  idempotency_key text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  -- Provocação de decisão, não alarme: com uma opção só não há o que decidir.
  CONSTRAINT diretriz_tem_opcoes CHECK (jsonb_array_length(options) >= 2),
  -- Decidir REGISTRA quem decidiu e por quê. Sem o motivo gravado, ninguém
  -- entende três semanas depois por que a demanda 2 esperou a 1.
  CONSTRAINT decisao_tem_autor_e_motivo CHECK (
    status <> 'decided' OR (
      decided_option IS NOT NULL AND decided_by IS NOT NULL
      AND decided_at IS NOT NULL AND COALESCE(btrim(rationale), '') <> ''))
);
CREATE INDEX directives_projeto_idx ON directives (account_id, project_id, status);
CREATE UNIQUE INDEX directives_idem_idx
  ON directives (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- A regra de ouro da ADR-0015 §5 como trigger, porque CHECK não faz subconsulta.
-- Duas coisas verificadas juntas: a recomendação é uma das opções (recomendar o
-- que não está na lista é a versão silenciosa de não recomendar nada), e toda
-- instrução de toda opção pertence ao vocabulário de COORDENAÇÃO. Uma diretriz
-- que mandasse "pausar a demanda 1" não chega ao banco.
CREATE OR REPLACE FUNCTION assert_diretriz_coordena_sem_pausar() RETURNS trigger AS $$
DECLARE acao text;
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM jsonb_array_elements(NEW.options) o WHERE o->>'key' = NEW.recommended
  ) THEN
    RAISE EXCEPTION 'a recomendação % não está entre as opções da diretriz', NEW.recommended;
  END IF;

  FOR acao IN
    SELECT i->>'action'
      FROM jsonb_array_elements(NEW.options) o,
           jsonb_array_elements(COALESCE(o->'instructions', '[]'::jsonb)) i
  LOOP
    IF acao IS NULL OR acao NOT IN
       ('cherry_pick', 'merge_order', 'file_partition', 'cross_verify') THEN
      RAISE EXCEPTION
        'instrução de diretriz fora do vocabulário de coordenação: % — diretriz coordena, nunca pausa demanda (ADR-0015)',
        COALESCE(acao, '(vazia)');
    END IF;
  END LOOP;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER directives_coordenacao_guard
  BEFORE INSERT OR UPDATE OF options, recommended ON directives
  FOR EACH ROW EXECUTE FUNCTION assert_diretriz_coordena_sem_pausar();

-- +goose Down
DROP TRIGGER IF EXISTS directives_coordenacao_guard ON directives;
DROP FUNCTION IF EXISTS assert_diretriz_coordena_sem_pausar();
DROP TABLE IF EXISTS directives CASCADE;
DROP TYPE IF EXISTS directive_status, directive_kind;

DROP TABLE IF EXISTS merge_queue_entries CASCADE;
DROP TYPE IF EXISTS merge_queue_state;

DROP TRIGGER IF EXISTS pull_requests_verde_guard ON pull_requests;
DROP FUNCTION IF EXISTS assert_pr_tem_verde();
DROP TABLE IF EXISTS pull_requests CASCADE;

DROP TABLE IF EXISTS verification_runs CASCADE;
DROP TYPE IF EXISTS verification_outcome, verification_kind;
