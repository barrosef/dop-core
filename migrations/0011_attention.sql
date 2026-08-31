-- +goose Up
-- A caixa de atenção é PROJEÇÃO do log de eventos (ADR-0006), não sistema.
--
-- Consequência no schema: não há coluna que só um humano possa mudar. Item
-- abre por evento e fecha por evento; não existe "marcar como lido". Item que
-- some sem o problema ter sido resolvido é mentira confortável, e caixa que
-- mente vira caixa ignorada — o risco R-1 da spec.
CREATE TABLE attention_items (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  kind         text NOT NULL,
  target_kind  text NOT NULL,
  target_id    text NOT NULL,
  demand_id    uuid,
  title        text NOT NULL,
  summary      text NOT NULL DEFAULT '',
  opened_at    timestamptz NOT NULL,
  resolved_at  timestamptz,
  -- O evento que ABRIU o item. É ele que torna a reentrega inócua: a entrega
  -- do JetStream é ao-menos-uma-vez (ADR-0019), e sem esta chave o mesmo
  -- bloqueio viraria três itens iguais na cara do dev.
  opened_by_event uuid NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- Idempotência da projeção: o mesmo evento nunca abre dois itens.
CREATE UNIQUE INDEX attention_items_evento_idx ON attention_items (opened_by_event);

-- Um alvo só pode ter UM item aberto do mesmo tipo. Sem isto, uma thread que
-- bloqueia, destrava e bloqueia de novo acumularia itens fantasmas — e a caixa
-- encheria de coisa já resolvida, que é como ela morre.
CREATE UNIQUE INDEX attention_items_alvo_aberto_idx
  ON attention_items (account_id, kind, target_kind, target_id)
  WHERE resolved_at IS NULL;

-- A consulta da caixa é sempre "abertos desta conta, mais urgentes primeiro".
-- Índice parcial porque item resolvido não é consultado na tela; ele fica para
-- responder quanto tempo o dev levou.
CREATE INDEX attention_items_abertos_idx
  ON attention_items (account_id, opened_at)
  WHERE resolved_at IS NULL;

CREATE INDEX attention_items_demanda_idx ON attention_items (demand_id)
  WHERE demand_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS attention_items;
