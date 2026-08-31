-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Custo: medição de uso de LLM, orçamento por escopo (ADR-0011/0012).
--
-- Três tabelas, e cada uma existe por um motivo diferente:
--
--   cost_usage      — o que foi consumido. É a tabela que MAIS cresce na
--                     plataforma: N agentes × sessões longas × um registro por
--                     turno. Particionada por mês, como `events`.
--   cost_usage_keys — a guarda de não-duplicação. Separada de propósito; a
--                     justificativa está sobre a tabela.
--   cost_budgets    — o teto e o acumulado por escopo. Poucas linhas, muito
--                     UPDATE: fica fora do caminho de partição.
-- ════════════════════════════════════════════════════════════════════════════

-- ── por que inteiro, e não numeric nem float ─────────────────────────────────
-- Valor monetário em MICROS (10^-6 da unidade da moeda), bigint, igual ao
-- Money.amount_micros do contrato (api/proto/dop/v1/common.proto).
--
-- float/double está fora de questão: soma de milhões de linhas em binário de
-- base 2 acumula erro, e orçamento que erra por centavos deixa de ser
-- orçamento. numeric seria exato, mas custa mais por soma, não cabe num
-- registrador e — o que decide — o contrato JÁ fala em micros: converter na
-- borda criaria dois vocabulários de dinheiro no mesmo sistema.
--
-- Micros dá margem folgada: um token de saída em modelo forte custa da ordem
-- de 75 micros de dólar; bigint em micros só estoura acima de 9,2 trilhões de
-- dólares. E a moeda viaja junto, porque somar micros de moedas diferentes é
-- exatamente o bug que um `int64` solto não impede.

CREATE TABLE cost_usage (
  id             uuid NOT NULL DEFAULT gen_random_uuid(),
  account_id     uuid NOT NULL,
  -- Sem FK para demandas: a tabela nasce em outra migração, e custo precisa
  -- poder ser medido para trabalho que não é de nenhuma demanda (job noturno,
  -- indexação em batch — ADR-0012 §5). NULL aqui é "consumo da conta".
  demand_id      uuid,
  thread_id      text NOT NULL DEFAULT '',
  model          text NOT NULL,
  input_tokens          bigint NOT NULL DEFAULT 0,
  output_tokens         bigint NOT NULL DEFAULT 0,
  -- Os dois campos de cache são o material de calibração do ModelRouter (P-7)
  -- e o detector do invalidador silencioso da ADR-0012: leitura de prefixo
  -- cacheado custa ~0,1× do input, e cache_read zerado em turno repetido de
  -- prefixo estável é ALERTA, não curiosidade.
  cache_read_tokens     bigint NOT NULL DEFAULT 0,
  cache_creation_tokens bigint NOT NULL DEFAULT 0,
  cost_micros    bigint NOT NULL DEFAULT 0,
  currency       text   NOT NULL DEFAULT 'USD',
  occurred_at    timestamptz NOT NULL DEFAULT now(),
  recorded_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT cost_usage_nao_negativo CHECK (
    input_tokens >= 0 AND output_tokens >= 0 AND
    cache_read_tokens >= 0 AND cache_creation_tokens >= 0 AND
    cost_micros >= 0),
  -- A chave de partição entra na PK porque o Postgres exige — e é isso que
  -- torna impossível uma UNIQUE só em idempotency_key AQUI (ver adiante).
  PRIMARY KEY (id, occurred_at),
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
) PARTITION BY RANGE (occurred_at);

-- As três consultas que existem: extrato da conta, extrato da demanda e
-- calibração por modelo. Todas por conta — isolamento é constraint.
CREATE INDEX cost_usage_account_idx ON cost_usage (account_id, occurred_at DESC);
CREATE INDEX cost_usage_demand_idx  ON cost_usage (account_id, demand_id, occurred_at DESC)
  WHERE demand_id IS NOT NULL;
CREATE INDEX cost_usage_model_idx   ON cost_usage (account_id, model, occurred_at DESC);

-- Partição corrente e as seguintes; o modo sched cria as próximas, como já faz
-- para `events`. Particionar aqui é o que permite APAGAR histórico antigo com
-- DROP de partição (instantâneo) em vez de DELETE de milhões de linhas.
CREATE TABLE cost_usage_2026_08 PARTITION OF cost_usage
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE cost_usage_2026_09 PARTITION OF cost_usage
  FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE cost_usage_2026_10 PARTITION OF cost_usage
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

-- ── a guarda de não-duplicação ───────────────────────────────────────────────
-- Por que uma tabela só para a chave, em vez de UNIQUE em cost_usage:
--
-- Toda UNIQUE numa tabela particionada TEM de conter a chave de partição. Uma
-- `UNIQUE (idempotency_key, occurred_at)` só barraria a repetição que chegasse
-- com o MESMO instante — e o instante é preenchido pelo relógio quando o
-- chamador não o envia, então a retentativa chegaria com outro. A garantia
-- morreria em silêncio, e orçamento com registro dobrado é ficção.
--
-- Esta tabela é plana e a UNIQUE é real. É pequena por linha (chave + dois
-- ponteiros), cresce junto com cost_usage e é podada na mesma cadência em que
-- as partições antigas caem — occurred_at está aqui para isso.
--
-- A chave é escopada por conta: chave vazada de um tenant não sonda o outro, e
-- dois clientes independentes podem usar o mesmo UUID sem colidir.
CREATE TABLE cost_usage_keys (
  account_id      uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  idempotency_key text NOT NULL,
  -- Sem FK: a PK do alvo é composta (id, occurred_at) por causa da partição.
  usage_id        uuid NOT NULL,
  occurred_at     timestamptz NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, idempotency_key)
);
CREATE INDEX cost_usage_keys_pruning_idx ON cost_usage_keys (occurred_at);

-- ── orçamento ────────────────────────────────────────────────────────────────
-- Escopo é (scope, scope_id): 'account' com o id da conta, 'demand' com o id da
-- demanda. Dois escopos e não mais: a fatia por thread é derivada da ficha do
-- subagente (ADR-0010), não é linha aqui.
--
-- spent_micros é acumulado, não recomputado: somar cost_usage a cada registro
-- viraria varredura da maior tabela do sistema no caminho quente. O acumulado
-- é reconstruível a partir de cost_usage se algum dia divergir — a verdade
-- continua sendo o log.
CREATE TABLE cost_budgets (
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  scope        text NOT NULL CHECK (scope IN ('account', 'demand')),
  scope_id     text NOT NULL,
  -- 0 = SEM TETO. Ausência de orçamento não pode virar orçamento zero, senão
  -- toda conta nova nasceria pausada no primeiro token.
  limit_micros bigint NOT NULL DEFAULT 0 CHECK (limit_micros >= 0),
  spent_micros bigint NOT NULL DEFAULT 0 CHECK (spent_micros >= 0),
  currency     text   NOT NULL DEFAULT 'USD',
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, scope, scope_id)
);

-- +goose Down
DROP TABLE IF EXISTS cost_budgets;
DROP TABLE IF EXISTS cost_usage_keys;
DROP TABLE IF EXISTS cost_usage CASCADE;
