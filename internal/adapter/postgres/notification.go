package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// NotificationRepo grava o REGISTRO de envio e responde as duas perguntas de
// leitura do gatilho: quem recebe aviso desta conta, e o que da caixa já
// esperou o bastante para virar e-mail.
type NotificationRepo struct{ pool *pgxpool.Pool }

func NewNotificationRepo(pool *pgxpool.Pool) *NotificationRepo {
	return &NotificationRepo{pool: pool}
}

var _ notification.Repository = (*NotificationRepo)(nil)

// Claim é a idempotência, e ela é UMA INSTRUÇÃO — não um SELECT seguido de um
// INSERT.
//
// A diferença não é de estilo: com duas instruções, dois workers processando a
// reentrega do mesmo evento leem "não existe" ao mesmo tempo e mandam dois
// e-mails. O `ON CONFLICT` transforma a corrida numa decisão do índice único, e
// o índice é `(event_id, rule_name, action_name)` — a chave composta da
// ADR-0025.
//
// O `WHERE` do DO UPDATE é a outra metade: só linha em `error` é retomada.
// Linha em `sent` nunca reenvia (e-mail duplicado não tem desfazer) e linha
// parada em `pending` também não — ela significa que o processo morreu entre o
// envio e o registro, e reenviar seria apostar que a mensagem NÃO saiu.
func (r *NotificationRepo) Claim(ctx context.Context, c notification.Claim, maxAttempts int) (bool, error) {
	if maxAttempts <= 0 {
		maxAttempts = notification.DefaultMaxAttempts
	}
	var id string
	err := r.pool.QueryRow(ctx, `
		INSERT INTO notification_deliveries
		       (account_id, event_id, rule_name, action_name, kind, channel, recipients)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id, rule_name, action_name) DO UPDATE
		   SET state      = 'pending',
		       attempts   = notification_deliveries.attempts + 1,
		       recipients = EXCLUDED.recipients,
		       error      = '',
		       batch_id   = NULL,
		       settled_at = NULL
		 WHERE notification_deliveries.account_id = $1
		   AND notification_deliveries.state = 'error'
		   AND notification_deliveries.attempts < $8
		RETURNING id::text`,
		c.AccountID, c.EventID, c.Rule, string(c.Action), string(c.Kind),
		naoVazioTexto(c.Channel, "email"), c.Recipients, maxAttempts).Scan(&id)
	if NoRows(err) {
		// Conflito que o WHERE recusou: a chave já foi atendida (ou esgotou as
		// tentativas). É o caminho NORMAL da reentrega — não é erro.
		return false, nil
	}
	if err != nil {
		return false, Translate(err, "reserva de aviso")
	}
	return true, nil
}

// Settle grava o desfecho e emite o evento na MESMA transação.
//
// Mesma regra de toda mudança de estado desta casa (ADR-0019): commit ⇒ atômico
// por construção, nunca "registrei mas não publiquei". Aqui isso vale duplo,
// porque o evento `dop.notification.*` é o que o P-29 vai consumir quando a
// reação virar dado — um registro sem evento seria uma reação invisível para a
// própria máquina de reações.
//
// A RESERVA (Claim) não emite evento de propósito: reserva não é fato sobre o
// mundo, é intenção. O fato é "avisamos fulano" ou "não conseguimos", e ele só
// existe aqui.
func (r *NotificationRepo) Settle(ctx context.Context, o notification.Outcome) (string, error) {
	if len(o.Keys) == 0 {
		return "", nil
	}
	eventos := make([]string, 0, len(o.Keys))
	regras := make([]string, 0, len(o.Keys))
	acoes := make([]string, 0, len(o.Keys))
	for _, k := range o.Keys {
		eventos = append(eventos, k.EventID)
		regras = append(regras, k.Rule)
		acoes = append(acoes, string(k.Action))
	}

	var batchID string
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// `lote` gera UM uuid para todas as linhas: é ele que amarra os N itens
		// do resumo à UMA mensagem que os cobriu. Sem ele, "quantos e-mails
		// saíram?" só teria como resposta "quantas linhas foram gravadas", que
		// é outra pergunta.
		rows, err := tx.Query(ctx, `
			WITH lote AS (SELECT gen_random_uuid() AS id),
			     alvo AS (SELECT * FROM unnest($2::uuid[], $3::text[], $4::text[])
			                       AS t(event_id, rule_name, action_name))
			UPDATE notification_deliveries d
			   SET state      = $5,
			       provider   = $6,
			       reference  = $7,
			       error      = $8,
			       batch_id   = lote.id,
			       settled_at = now()
			  FROM lote, alvo
			 WHERE d.account_id  = $1
			   AND d.event_id    = alvo.event_id
			   AND d.rule_name   = alvo.rule_name
			   AND d.action_name = alvo.action_name
			   AND d.state       = 'pending'
			RETURNING lote.id::text`,
			o.AccountID, eventos, regras, acoes,
			string(o.State), o.Provider, o.Reference, truncarErro(o.Error))
		if err != nil {
			return Translate(err, "registro de aviso")
		}
		for rows.Next() {
			if err := rows.Scan(&batchID); err != nil {
				rows.Close()
				return Translate(err, "registro de aviso")
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Translate(err, "registro de aviso")
		}
		if batchID == "" {
			// Nenhuma linha em `pending`: outro processo liquidou antes. Não é
			// erro e não emite evento — o evento já foi emitido por quem
			// liquidou, e emitir de novo contaria a mesma coisa duas vezes.
			return nil
		}

		tipo := "dop.notification.sent"
		if o.State == notification.StateError {
			tipo = "dop.notification.failed"
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: o.AccountID, Aggregate: "notification", AggregateID: batchID,
			Type: tipo,
			Payload: mustJSON(map[string]any{
				"kind": string(o.Kind), "state": string(o.State),
				"provider": o.Provider, "reference": o.Reference,
				// Os eventos COBERTOS: é o que liga o aviso de volta ao que o
				// causou, e é o que o P-29 vai querer ler.
				"event_ids": eventos, "rules": regras, "actions": acoes,
				// Quantidade, não a lista: endereço de pessoa em payload de
				// evento é dado pessoal em repouso, replicado para toda
				// projeção que assinar `dop.>`.
				"recipients": len(o.Recipients),
				// A mensagem já vem REDIGIDA do adaptador (garantia 4 da porta).
				"error": truncarErro(o.Error),
			}),
		})
	})
	if err != nil {
		return "", err
	}
	return batchID, nil
}

// Recipients devolve quem recebe aviso da conta.
//
// ── Por que só e-mail VERIFICADO ────────────────────────────────────────────
//
// Porque o resumo carrega TÍTULO de item da caixa — nome de demanda, de PR, de
// projeto. Mandar isso para um endereço que ninguém provou pertencer ao membro
// é vazar trabalho da conta para quem quer que tenha digitado aquele endereço
// no cadastro. A porta `IdentityProvider` já trata `email_verified` como falso
// quando o emissor não afirma (garantia 5 dela), e essa cautela só vale se
// alguém a consumir — este é o lugar.
//
// A consequência é declarada: conta sem membro verificado não recebe resumo. É
// preferível ao inverso.
func (r *NotificationRepo) Recipients(ctx context.Context, accountID string) ([]notification.Recipient, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT u.email, COALESCE(u.name,'')
		  FROM memberships m
		  JOIN users u ON u.id = m.user_id
		 WHERE m.account_id = $1
		   AND u.email IS NOT NULL AND u.email <> ''
		   AND u.email_verified
		 ORDER BY u.email`, accountID)
	if err != nil {
		return nil, Translate(err, "destinatários da conta")
	}
	defer rows.Close()

	var out []notification.Recipient
	for rows.Next() {
		var r notification.Recipient
		if err := rows.Scan(&r.Email, &r.Name); err != nil {
			return nil, Translate(err, "destinatário")
		}
		out = append(out, r)
	}
	return out, Translate(rows.Err(), "destinatários da conta")
}

// ripeWhere é o predicado do ATRASO, escrito uma vez para as duas consultas.
//
// É aqui que o "resumo com atraso" acontece, e ele é uma CLÁUSULA, não um
// agendador: item ainda aberto (`resolved_at IS NULL`), aberto antes do corte,
// e sem registro de aviso — ou com registro em `error` que ainda tem tentativa.
//
// Item resolvido antes do corte some do resultado sozinho. Não há timer para
// cancelar, e por isso não há caminho de cancelamento para errar — que é
// justamente o caminho que só executa no caso raro e por isso quebra calado.
const ripeWhere = `
	  FROM attention_items a
	  LEFT JOIN notification_deliveries d
	         ON d.event_id    = a.opened_by_event
	        AND d.rule_name   = $1
	        AND d.action_name = $2
	 WHERE a.resolved_at IS NULL
	   AND a.opened_at <= $3
	   AND (d.id IS NULL OR (d.state = 'error' AND d.attempts < $4))`

func (r *NotificationRepo) AccountsWithRipeAttention(ctx context.Context, rule string,
	action notification.Action, olderThan time.Time, maxAttempts int) ([]string, error) {
	if maxAttempts <= 0 {
		maxAttempts = notification.DefaultMaxAttempts
	}
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT a.account_id::text`+ripeWhere,
		rule, string(action), olderThan, maxAttempts)
	if err != nil {
		return nil, Translate(err, "contas com pendência madura")
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, Translate(err, "conta com pendência madura")
		}
		out = append(out, id)
	}
	return out, Translate(rows.Err(), "contas com pendência madura")
}

func (r *NotificationRepo) RipeAttention(ctx context.Context, accountID, rule string,
	action notification.Action, olderThan time.Time, maxAttempts, limit int) ([]notification.AttentionNotice, error) {
	if maxAttempts <= 0 {
		maxAttempts = notification.DefaultMaxAttempts
	}
	if limit <= 0 {
		limit = notification.DefaultDigestLimit
	}
	// Filtro por conta, como toda consulta desta casa. A varredura visita conta
	// por conta justamente para que esta cláusula continue existindo.
	rows, err := r.pool.Query(ctx, `
		SELECT a.account_id::text, a.opened_by_event::text, a.id::text, a.kind,
		       a.title, a.summary, COALESCE(a.demand_id::text,''), a.opened_at`+
		ripeWhere+`
		   AND a.account_id = $5
		 ORDER BY a.opened_at
		 LIMIT $6`,
		rule, string(action), olderThan, maxAttempts, accountID, limit)
	if err != nil {
		return nil, Translate(err, "pendências maduras")
	}
	defer rows.Close()

	var out []notification.AttentionNotice
	for rows.Next() {
		var n notification.AttentionNotice
		if err := rows.Scan(&n.AccountID, &n.EventID, &n.ItemID, &n.Kind,
			&n.Title, &n.Summary, &n.DemandID, &n.OpenedAt); err != nil {
			return nil, Translate(err, "pendência madura")
		}
		out = append(out, n)
	}
	return out, Translate(rows.Err(), "pendências maduras")
}

// truncarErro limita o que vai para a coluna e para o payload do evento.
//
// Mensagem de erro de fornecedor pode vir com o corpo HTTP inteiro dentro, e um
// evento de 200 KB atravessa o outbox, o JetStream e TODA projeção que assina
// `dop.>`. O que interessa para diagnosticar está no começo.
func truncarErro(s string) string {
	const teto = 500
	if len(s) <= teto {
		return s
	}
	return s[:teto] + "…"
}

func naoVazioTexto(v, padrao string) string {
	if v == "" {
		return padrao
	}
	return v
}
