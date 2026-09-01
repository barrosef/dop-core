-- +goose Up
-- Registro de ENVIO da comunicação (ADR-0025).
--
-- Esta tabela não é uma fila e não é uma projeção: é o REGISTRO do que foi
-- disparado, com estado, no formato que o projeto irmão já provou útil
-- (`sent` / `sent_local` / `error`). Sem ele, "o convite não chegou" é uma
-- pergunta sem resposta — e-mail é a única parte da plataforma cujo resultado
-- acontece fora dela.
--
-- ── Por que a chave de idempotência é COMPOSTA ──────────────────────────────
--
-- A caixa de atenção usa índice único por EVENTO, e funciona porque lá a
-- relação é 1:1. Aqui não é: com reação declarativa (P-29) um evento poderá
-- disparar N ações, e chave só pelo evento descartaria a segunda como
-- duplicata. Descarte por idempotência é SILENCIOSO por desenho — ninguém veria
-- o segundo aviso sumir. A chave nasce composta mesmo com uma ação só hoje,
-- porque trocar chave de idempotência depois é migrar dado vivo.
--
-- A chave é exatamente (evento, regra, ação), como a ADR escreveu. O
-- DESTINATÁRIO fica FORA dela de propósito: quem a ação endereça é decisão da
-- regra, e um resumo que hoje vai para dois membros e amanhã para três não pode
-- reenviar para os dois antigos por causa do terceiro.
CREATE TABLE notification_deliveries (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,

  -- ── a chave composta ──
  -- Sem FK para `events`: a tabela de eventos é particionada por mês, e uma FK
  -- para tabela particionada amarraria o descarte de partição antiga ao
  -- registro de envio.
  event_id     uuid NOT NULL,
  rule_name    text NOT NULL,
  action_name  text NOT NULL,

  -- kind é o TIPO da notificação no vocabulário do domínio (`invite`,
  -- `attention_digest`). Quem traduz tipo em template é o adaptador — aqui ele
  -- é registro, não roteamento.
  kind         text NOT NULL,
  channel      text NOT NULL DEFAULT 'email',

  -- Os endereços que a ação atendeu. Array, e não linha por destinatário,
  -- porque a unidade de idempotência é a AÇÃO: o resumo de um item é um envio
  -- só, ainda que a conta tenha cinco membros.
  recipients   text[] NOT NULL DEFAULT '{}',

  -- pending: reservado, ainda sem desfecho. Linha parada em pending é anomalia
  -- VISÍVEL (processo morreu entre o envio e o registro) — e de propósito ela
  -- não é reenviada: e-mail duplicado não tem desfazer, e perder um aviso
  -- registrado é menos grave do que mandar dois.
  -- sent / sent_local / error: os três estados do projeto irmão.
  state        text NOT NULL DEFAULT 'pending'
               CHECK (state IN ('pending','sent','sent_local','error')),
  provider     text NOT NULL DEFAULT '',
  reference    text NOT NULL DEFAULT '',
  -- Mensagem de falha. NUNCA carrega credencial: o adaptador redige antes de
  -- devolver o erro, e é a suíte de contrato que prova isso.
  error        text NOT NULL DEFAULT '',
  attempts     int  NOT NULL DEFAULT 1,

  -- batch_id junta as linhas cobertas por UMA mensagem. É o que permite o
  -- resumo (N itens num e-mail) sem abrir mão de uma linha por (evento, regra,
  -- ação): a idempotência continua por item, a mensagem continua sendo uma.
  batch_id     uuid,

  created_at   timestamptz NOT NULL DEFAULT now(),
  settled_at   timestamptz
);

-- A GARANTIA. Reentrega do JetStream é ao-menos-uma-vez (ADR-0019); sem este
-- índice, o mesmo convite chega três vezes na caixa de quem foi convidado.
CREATE UNIQUE INDEX notification_deliveries_idem_idx
  ON notification_deliveries (event_id, rule_name, action_name);

-- A varredura do resumo pergunta "o que desta conta ainda não virou aviso".
CREATE INDEX notification_deliveries_conta_idx
  ON notification_deliveries (account_id, created_at DESC);

-- Reenvio só existe para o que FALHOU, e o índice parcial é o que torna essa
-- varredura barata quando quase tudo deu certo.
CREATE INDEX notification_deliveries_falhas_idx
  ON notification_deliveries (state, created_at)
  WHERE state = 'error';

-- +goose Down
DROP TABLE IF EXISTS notification_deliveries;
