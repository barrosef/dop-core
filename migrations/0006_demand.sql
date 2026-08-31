-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- A DEMANDA (ADR-0006, ADR-0010, ADR-0014)
--
-- Onde vivem os eventos da demanda: em `events`, a tabela que já existe.
--
-- A demanda é um log append-only e o estado é projeção dele (ADR-0006). O log
-- da plataforma inteira já é `events` — particionada por mês, com outbox, relay
-- e replay prontos. Dar à demanda um log próprio duplicaria mecanismo (segundo
-- outbox, segundo relay, segundo cursor), quebraria o WatchDemand — que reusa o
-- fan-out único do domínio `event` — e criaria dois passados possíveis para a
-- mesma pergunta de auditoria. Então: nenhuma tabela de evento aqui. Todo
-- evento da demanda entra em `events` com aggregate='demand' e
-- aggregate_id = id da demanda, INCLUSIVE mensagem de thread e achado; quem
-- precisa recortar por thread lê `payload->>'thread_id'`.
--
-- O que as tabelas abaixo guardam é o ESTADO PROJETADO — a leitura que a tela e
-- a máquina de etapas precisam ter em O(1), sem reler o log. Elas são gravadas
-- na MESMA transação do evento (ADR-0019), e não por consumidor assíncrono: a
-- decisão da próxima transição depende do estado corrente, e decidir sobre uma
-- projeção atrasada é decidir sobre o passado. Por isso não há projeção
-- assíncrona de demanda — haveria dois escritores para as mesmas linhas.
--
-- Reconstrução: apagar estas tabelas e reprocessar `events` da conta, em ordem
-- de occurred_at, reconstrói tudo — cada payload carrega o delta completo
-- (`dop.demand.started` traz o snapshot do fluxo e as chaves das etapas;
-- `stage.advanced` e `gate.decided` trazem etapa, de/para e status resultante;
-- `thread.created` traz a ficha; `finding.published` traz o achado inteiro).
--
-- Mensagem de thread NÃO tem tabela: ela é só evento. A leitura do histórico da
-- conversa é a projeção `timeline`, que já indexa (aggregate, aggregate_id,
-- occurred_at) — criar uma tabela de mensagens seria gravar o mesmo texto duas
-- vezes para servir uma consulta que já existe.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TABLE demands (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id      uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  -- a chave do card no provedor: SUOPT-1315
  external_key    text NOT NULL,
  title           text NOT NULL,
  -- tipo de card e status do provedor são dinâmicos, do provedor (ADR-0013)
  card_type       text NOT NULL DEFAULT '',
  provider_status text NOT NULL DEFAULT '',
  dop_status      text NOT NULL DEFAULT 'new'
                  CHECK (dop_status IN ('new','doing','done','delivered')),

  -- ── o fluxo CONGELADO (ADR-0014 §4) ──
  -- flow_id é text e NÃO tem FK de propósito: o snapshot precisa sobreviver ao
  -- fluxo ser editado, promovido ou apagado. Uma FK com CASCADE apagaria a
  -- demanda junto; uma FK com RESTRICT impediria a conta de limpar o catálogo.
  -- O que dirige a demanda daqui em diante é flow_snapshot, não o fluxo vivo.
  flow_id         text NOT NULL DEFAULT '',
  flow_version    int  NOT NULL DEFAULT 0,
  flow_snapshot   jsonb NOT NULL DEFAULT '{}',

  -- ator, não usuário: quem inicia pode ser humano, agente ou o sistema
  -- (sincronização do task manager). Por isso text, sem FK para users.
  created_by      text NOT NULL DEFAULT '',
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),

  -- Uma demanda por card do provedor. É esta constraint que faz o StartDemand
  -- repetido devolver a demanda existente em vez de congelar o fluxo de novo.
  UNIQUE (project_id, external_key)
);

CREATE INDEX demands_account_idx ON demands (account_id, created_at DESC);
CREATE INDEX demands_project_idx ON demands (project_id, created_at DESC);
CREATE INDEX demands_status_idx  ON demands (account_id, dop_status);

-- As etapas da demanda: instâncias do molde congelado. O molde continua em
-- flow_snapshot; aqui mora o PROGRESSO.
CREATE TABLE demand_stages (
  demand_id     uuid NOT NULL REFERENCES demands(id) ON DELETE CASCADE,
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  key           text NOT NULL,
  name          text NOT NULL DEFAULT '',
  -- vocabulário FECHADO da plataforma: tipo novo é evolução, não dado
  type          text NOT NULL
                CHECK (type IN ('context','spec','plan','implementation','test',
                                'human_validation','finalization','generic')),
  gate          text NOT NULL DEFAULT 'none' CHECK (gate IN ('none','human')),
  status        text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','running','blocked','done')),
  position      int  NOT NULL,
  artifacts     jsonb NOT NULL DEFAULT '[]',
  started_at    timestamptz,
  finished_at   timestamptz,
  -- NULL = ninguém decidiu ainda; distinto de "reprovado"
  gate_approved boolean,
  gate_comment  text NOT NULL DEFAULT '',
  PRIMARY KEY (demand_id, key)
);

CREATE UNIQUE INDEX demand_stages_position_uniq ON demand_stages (demand_id, position);
CREATE INDEX demand_stages_account_idx ON demand_stages (account_id, status);

-- Uma thread por agente — o dev conversa sem misturar timelines (ADR-0010).
CREATE TABLE demand_threads (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  demand_id     uuid NOT NULL REFERENCES demands(id) ON DELETE CASCADE,
  key           text NOT NULL,          -- principal, forense-db, logs
  -- ficha do subagente (ADR-0010 §2): sem ela, subagente é caixa-preta
  purpose       text NOT NULL DEFAULT '',
  tools         text[] NOT NULL DEFAULT '{}',
  model         text NOT NULL DEFAULT '',
  effort        text NOT NULL DEFAULT '',
  budget_micros bigint NOT NULL DEFAULT 0,
  -- ciclo da thread: aberta → ativa → bloqueada → concluída
  state         text NOT NULL DEFAULT 'aberta'
                CHECK (state IN ('aberta','ativa','bloqueada','concluida')),
  created_by    text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (demand_id, key),
  -- permite a FK composta do achado abaixo
  UNIQUE (id, demand_id)
);

-- A consulta da caixa de atenção: threads bloqueadas da conta, mais velhas
-- primeiro. Índice parcial porque só o bloqueado interessa à fila.
CREATE INDEX demand_threads_blocked_idx ON demand_threads (account_id, updated_at)
  WHERE state = 'bloqueada';
CREATE INDEX demand_threads_demand_idx ON demand_threads (demand_id);

-- O quadro de achados da demanda (ADR-0010 §4).
--
-- Achado é ESTADO, não só narrativa: é ele que destrava a conclusão da thread,
-- e essa decisão não pode depender da projeção assíncrona, que pode ainda não
-- ter visto a publicação de um segundo atrás.
CREATE TABLE demand_findings (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  demand_id  uuid NOT NULL REFERENCES demands(id) ON DELETE CASCADE,
  thread_id  uuid NOT NULL,
  title      text NOT NULL,
  payload    jsonb NOT NULL DEFAULT '{}',
  created_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  -- FK COMPOSTA: garante no banco que a thread do achado é da MESMA demanda.
  -- A demanda é a fronteira de segurança (ADR-0010 §6); achado cruzando
  -- demanda seria vazamento de contexto, e isso é invariante, não validação.
  FOREIGN KEY (thread_id, demand_id)
    REFERENCES demand_threads (id, demand_id) ON DELETE CASCADE
);

CREATE INDEX demand_findings_demand_idx ON demand_findings (demand_id, created_at DESC);
CREATE INDEX demand_findings_thread_idx ON demand_findings (thread_id);

-- +goose StatementBegin
-- A thread não morre em silêncio.
--
-- Concluir exige achado publicado (spec conversacao-e-atencao §1). A regra
-- também está no domínio, com mensagem melhor; aqui ela é INVARIANTE — nenhum
-- caminho, nem o que ninguém previu, encerra uma investigação sem deixar
-- registro durável. Mesmo padrão do "toda conta tem um owner ativo".
CREATE OR REPLACE FUNCTION demand_thread_requires_finding() RETURNS trigger AS $$
BEGIN
  IF NEW.state = 'concluida' AND OLD.state IS DISTINCT FROM 'concluida' THEN
    IF NOT EXISTS (SELECT 1 FROM demand_findings f WHERE f.thread_id = NEW.id) THEN
      RAISE EXCEPTION
        'a thread % não pode ser concluída sem achado publicado', NEW.key;
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER demand_thread_conclusion_guard
  BEFORE UPDATE ON demand_threads
  FOR EACH ROW EXECUTE FUNCTION demand_thread_requires_finding();

-- +goose Down
DROP TRIGGER IF EXISTS demand_thread_conclusion_guard ON demand_threads;
DROP FUNCTION IF EXISTS demand_thread_requires_finding();
DROP TABLE IF EXISTS demand_findings, demand_threads, demand_stages, demands CASCADE;
