package projection

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// Attention builds the attention box from the log.
//
// The DECISION of what becomes an item lives in the domain (`attention.Apply`);
// here only the write happens. It was on purpose: deciding what requires a human
// decision is a business rule, and a business rule inside a projection is a rule
// nobody can test without bringing up a database.
//
// Idempotent, as every projection has to be — delivery is at-least-once.
type Attention struct{ pool *pgxpool.Pool }

func NewAttention(pool *pgxpool.Pool) *Attention { return &Attention{pool: pool} }

func (a *Attention) Handle(ctx context.Context, e ports.Event) error {
	var env struct {
		ID          string         `json:"id"`
		AccountID   string         `json:"account_id"`
		Aggregate   string         `json:"aggregate"`
		AggregateID string         `json:"aggregate_id"`
		Type        string         `json:"type"`
		Payload     map[string]any `json:"payload"`
		OccurredAt  time.Time      `json:"occurred_at"`
	}
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		// An unreadable event never improves with a retry.
		return nil
	}
	// An event with no account (user.ensured, migration 0003) belongs to no box
	// at all — and the column has an FK to accounts.
	if env.AccountID == "" {
		return nil
	}

	d := attention.Apply(attention.Event{
		ID: env.ID, AccountID: env.AccountID, Aggregate: env.Aggregate,
		AggregateID: env.AggregateID, Type: env.Type,
		OccurredAt: env.OccurredAt, Payload: env.Payload,
	})

	switch {
	case d.Open != nil:
		return a.abrir(ctx, *d.Open)
	case d.Close != nil:
		return a.fechar(ctx, env.AccountID, *d.Close, env.OccurredAt)
	}
	return nil
}

func (a *Attention) abrir(ctx context.Context, it attention.Item) error {
	// Two ON CONFLICTs, and each protects against a different thing:
	// `opened_by_event` makes the same event's REDELIVERY harmless; the partial
	// index on an open target stops a target that blocks, unblocks and blocks
	// again from accumulating ghost items.
	_, err := a.pool.Exec(ctx, `
		INSERT INTO attention_items
		       (account_id, kind, target_kind, target_id, demand_id,
		        title_key, params, title, summary, opened_at, opened_by_event)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,$11)
		ON CONFLICT DO NOTHING`,
		it.AccountID, string(it.Kind), it.TargetKind, it.TargetID, it.DemandID,
		it.TitleKey, paramsJSON(it.Params), it.Title, it.Summary, it.OpenedAt, it.EventID)
	if err != nil {
		return err
	}
	logging.From(ctx).Debug("attention item opened",
		"kind", string(it.Kind), "target", it.TargetID)
	return nil
}

func (a *Attention) fechar(ctx context.Context, accountID string, c attention.CloseSpec, quando time.Time) error {
	// It closes by TARGET and only what is open: reprocessing the log must not
	// touch the resolution instant already written, or else the response-time
	// metric would change on every rebuild of the projection.
	tag, err := a.pool.Exec(ctx, `
		UPDATE attention_items SET resolved_at = $5
		 WHERE account_id = $1 AND kind = $2 AND target_kind = $3 AND target_id = $4
		   AND resolved_at IS NULL`,
		accountID, string(c.Kind), c.TargetKind, c.TargetID, quando)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		logging.From(ctx).Debug("attention item resolved",
			"kind", string(c.Kind), "target", c.TargetID)
	}
	return nil
}

// paramsJSON serializes the translation parameters.
//
// A nil map becomes `{}` and not SQL NULL: the column is NOT NULL, and a reader
// that had to handle both "no params" and "null params" would be handling the
// same thing twice. A map that fails to serialize becomes `{}` as well — losing
// a cosmetic parameter is better than losing the attention item, which is what
// returning an error here would do.
func paramsJSON(p map[string]any) []byte {
	if len(p) == 0 {
		return []byte(`{}`)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}
