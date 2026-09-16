package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
)

// NotificationRepo writes the SEND RECORD and answers the trigger's two read
// questions: who receives notices for this account, and what in the box has
// already waited long enough to become an email.
type NotificationRepo struct{ pool *pgxpool.Pool }

func NewNotificationRepo(pool *pgxpool.Pool) *NotificationRepo {
	return &NotificationRepo{pool: pool}
}

var _ notification.Repository = (*NotificationRepo)(nil)

// Claim is the idempotency, and it is ONE STATEMENT — not a SELECT followed by
// an INSERT.
//
// The difference is not stylistic: with two statements, two workers processing
// the same event's redelivery read "it does not exist" at the same time and send
// two emails. The `ON CONFLICT` turns the race into a decision of the unique
// index, and the index is `(event_id, rule_name, action_name)` — ADR-0025's
// composite key.
//
// The DO UPDATE's `WHERE` is the other half: only a row in `error` is resumed. A
// row in `sent` never resends (a duplicate email has no undo) and a row stuck in
// `pending` does not either — it means the process died between the send and the
// record, and resending would be betting the message did NOT go out.
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
		// A conflict the WHERE refused: the key has already been served (or has
		// exhausted its attempts). It is the redelivery's NORMAL path — it is
		// not an error.
		return false, nil
	}
	if err != nil {
		return false, Translate(err, "reserva de aviso")
	}
	return true, nil
}

// Settle writes the outcome and emits the event in the SAME transaction.
//
// The same rule as every state change in this house (ADR-0019): a commit ⇒
// atomic by construction, never "I recorded it but did not publish it". Here
// that counts double, because the `dop.notification.*` event is what P-29 will
// consume when the reaction becomes data — a record with no event would be a
// reaction invisible to the reaction machine itself.
//
// The RESERVATION (Claim) emits no event on purpose: a reservation is not a fact
// about the world, it is an intention. The fact is "we notified so-and-so" or
// "we could not", and it only exists here.
func (r *NotificationRepo) Settle(ctx context.Context, o notification.Outcome) (string, error) {
	if len(o.Keys) == 0 {
		return "", nil
	}
	events := make([]string, 0, len(o.Keys))
	rules := make([]string, 0, len(o.Keys))
	actions := make([]string, 0, len(o.Keys))
	for _, k := range o.Keys {
		events = append(events, k.EventID)
		rules = append(rules, k.Rule)
		actions = append(actions, string(k.Action))
	}

	var batchID string
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// `lote` generates ONE uuid for every row: it is what ties the digest's
		// N items to the ONE message that covered them. Without it, "how many
		// emails went out?" could only be answered with "how many rows were
		// written", which is a different question.
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
			o.AccountID, events, rules, actions,
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
			// No row in `pending`: another process settled first. It is not an
			// error and it emits no event — the event was already emitted by
			// whoever settled, and emitting again would tell the same thing
			// twice.
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
				// The events COVERED: it is what links the notice back to what
				// caused it, and it is what P-29 will want to read.
				"event_ids": events, "rules": rules, "actions": actions,
				// The count, not the list: a person's address in an event
				// payload is personal data at rest, replicated to every
				// projection that subscribes to `dop.>`.
				"recipients": len(o.Recipients),
				// The message arrives already REDACTED from the adapter (the port's guarantee 4).
				"error": truncarErro(o.Error),
			}),
		})
	})
	if err != nil {
		return "", err
	}
	return batchID, nil
}

// Recipients returns who receives the account's notices.
//
// ── Why only a VERIFIED email ───────────────────────────────────────────────
//
// Because the digest carries the TITLE of a box item — a demand's, a PR's, a
// project's name. Sending that to an address nobody has proven belongs to the
// member is leaking the account's work to whoever typed that address at sign-up.
// The `IdentityProvider` port already treats `email_verified` as false when the
// issuer does not assert it (its guarantee 5), and that caution is only worth
// something if somebody consumes it — this is the place.
//
// The consequence is declared: an account with no verified member receives no
// digest. It is preferable to the inverse.
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
		return nil, Translate(err, "the account's recipients")
	}
	defer rows.Close()

	var out []notification.Recipient
	for rows.Next() {
		var r notification.Recipient
		if err := rows.Scan(&r.Email, &r.Name); err != nil {
			return nil, Translate(err, "recipient")
		}
		out = append(out, r)
	}
	return out, Translate(rows.Err(), "the account's recipients")
}

// ripeWhere is the DELAY's predicate, written once for both queries.
//
// It is here that the "digest with a delay" happens, and it is a CLAUSE, not a
// scheduler: an item still open (`resolved_at IS NULL`), opened before the
// cut-off, and with no notice record — or with a record in `error` that still
// has an attempt left.
//
// An item resolved before the cut-off disappears from the result on its own.
// There is no timer to cancel, and therefore no cancellation path to get wrong —
// which is precisely the path that only runs in the rare case and so breaks
// quietly.
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
		return nil, Translate(err, "accounts with a ripe pending item")
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, Translate(err, "an account with a ripe pending item")
		}
		out = append(out, id)
	}
	return out, Translate(rows.Err(), "accounts with a ripe pending item")
}

func (r *NotificationRepo) RipeAttention(ctx context.Context, accountID, rule string,
	action notification.Action, olderThan time.Time, maxAttempts, limit int) ([]notification.AttentionNotice, error) {
	if maxAttempts <= 0 {
		maxAttempts = notification.DefaultMaxAttempts
	}
	if limit <= 0 {
		limit = notification.DefaultDigestLimit
	}
	// A filter by account, as in every query in this house. The sweep visits
	// account by account precisely so this clause keeps existing.
	rows, err := r.pool.Query(ctx, `
		SELECT a.account_id::text, a.opened_by_event::text, a.id::text, a.kind,
		       a.title, a.summary, COALESCE(a.demand_id::text,''), a.opened_at`+
		ripeWhere+`
		   AND a.account_id = $5
		 ORDER BY a.opened_at
		 LIMIT $6`,
		rule, string(action), olderThan, maxAttempts, accountID, limit)
	if err != nil {
		return nil, Translate(err, "ripe pending items")
	}
	defer rows.Close()

	var out []notification.AttentionNotice
	for rows.Next() {
		var n notification.AttentionNotice
		if err := rows.Scan(&n.AccountID, &n.EventID, &n.ItemID, &n.Kind,
			&n.Title, &n.Summary, &n.DemandID, &n.OpenedAt); err != nil {
			return nil, Translate(err, "a ripe pending item")
		}
		out = append(out, n)
	}
	return out, Translate(rows.Err(), "ripe pending items")
}

// truncateError limits what goes into the column and into the event's payload.
//
// A provider's error message may come with the whole HTTP body inside, and a
// 200 KB event crosses the outbox, JetStream and EVERY projection subscribing to
// `dop.>`. What matters for diagnosis is at the start.
func truncarErro(s string) string {
	const limit = 500
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func naoVazioTexto(v, padrao string) string {
	if v == "" {
		return padrao
	}
	return v
}
