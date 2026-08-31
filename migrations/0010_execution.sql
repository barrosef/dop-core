-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Substrato de execução: o sandbox onde a demanda roda.
--
-- Uma demanda ativa, um sandbox (spec do substrato §1). Duas coisas vivem
-- dentro dele — a EXECUÇÃO, efêmera, e o WORKSPACE, que é o trabalho. Suspender
-- derruba a primeira e preserva o segundo; destruir leva os dois e não volta.
-- Essa diferença é a razão de existir do trigger no fim deste arquivo.
-- ════════════════════════════════════════════════════════════════════════════

-- Declarado, nunca presumido: são estes três, e a coluna nasce SEM DEFAULT.
CREATE TYPE isolation_tier AS ENUM ('hardware', 'kernel_emulated', 'namespace');

CREATE TYPE sandbox_state AS ENUM ('provisioning', 'active', 'suspended', 'destroyed');

CREATE TABLE sandboxes (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id     uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  -- Sem FK para demands: a tabela de demandas nasce em outra migração, escrita
  -- em paralelo. Quando ela existir, o vínculo é um ALTER de uma linha — e
  -- inventar aqui uma tabela que não é minha criaria duas verdades sobre o que
  -- é uma demanda.
  demand_id      uuid NOT NULL,
  state          sandbox_state NOT NULL DEFAULT 'provisioning',
  -- SEM DEFAULT, e é o ponto inteiro desta coluna: isolationTier é DECLARADO,
  -- nunca presumido. Um default aqui seria o banco escolhendo, em nome do
  -- cliente, quanto isolamento a carga dele merece — e o erro dessa escolha só
  -- apareceria no dia do incidente.
  tier           isolation_tier NOT NULL,
  namespace      text NOT NULL,
  -- Portas publicadas pela pilha da demanda. jsonb porque a lista vem do
  -- substrato e muda com o compose interno, sem migração.
  endpoints      jsonb NOT NULL DEFAULT '[]',
  -- Toda escrita carrega a chave: repetir a chamada não pode duplicar microVM.
  idempotency_key text,
  last_active_at timestamptz NOT NULL DEFAULT now(),
  suspended_at   timestamptz,
  destroyed_at   timestamptz,
  created_by     uuid REFERENCES users(id),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT sandbox_destruido_tem_data
    CHECK (state <> 'destroyed' OR destroyed_at IS NOT NULL)
);

-- Repetição da mesma chave resolve para o MESMO sandbox. Índice parcial porque
-- chamada sem chave (fluxo interno) não compete por unicidade.
CREATE UNIQUE INDEX sandboxes_idempotency_uniq
  ON sandboxes (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

-- Uma demanda ativa tem UM sandbox. Vivo é tudo que não é destruído: o
-- histórico de destruídos fica, e uma demanda pode ser reprovisionada depois.
CREATE UNIQUE INDEX sandboxes_demanda_viva_uniq
  ON sandboxes (demand_id) WHERE state <> 'destroyed';

CREATE INDEX sandboxes_account_state_idx ON sandboxes (account_id, state);

-- Índice do varredor de economia: só os ativos interessam a ele.
CREATE INDEX sandboxes_ociosos_idx
  ON sandboxes (last_active_at) WHERE state = 'active';

-- Invariante: destruído é ABSORVENTE.
--
-- Verificado por trigger, e não só no serviço, pela mesma razão do guarda de
-- owner da migração 0001: é regra que NENHUMA operação pode violar, nem por
-- caminho que ninguém previu. A destruição levou o workspace junto — ressuscitar
-- a linha faria o sistema afirmar que existe um trabalho que não existe mais.
CREATE OR REPLACE FUNCTION assert_sandbox_destruicao_irreversivel() RETURNS trigger AS $$
BEGIN
  IF OLD.state = 'destroyed' AND NEW.state <> 'destroyed' THEN
    RAISE EXCEPTION 'sandbox destruído não retoma: a destruição levou o workspace junto';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER sandboxes_destruicao_guard
  BEFORE UPDATE ON sandboxes
  FOR EACH ROW EXECUTE FUNCTION assert_sandbox_destruicao_irreversivel();

-- +goose Down
DROP TRIGGER IF EXISTS sandboxes_destruicao_guard ON sandboxes;
DROP FUNCTION IF EXISTS assert_sandbox_destruicao_irreversivel();
DROP TABLE IF EXISTS sandboxes CASCADE;
DROP TYPE IF EXISTS sandbox_state;
DROP TYPE IF EXISTS isolation_tier;
