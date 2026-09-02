package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
)

// AttentionRepo is READ-ONLY. The write lives in the projection
// (adapter/postgres/projection/attention.go), because an attention item is born
// from an event and dies from one — and a repository with a Create would be an
// invitation to create an item by hand, which would stop the box being a
// projection.
type AttentionRepo struct{ pool *pgxpool.Pool }

func NewAttentionRepo(pool *pgxpool.Pool) *AttentionRepo { return &AttentionRepo{pool: pool} }

const attentionCols = `id::text, account_id::text, kind, target_kind, target_id,
	COALESCE(demand_id::text,''), COALESCE(title_key,''), params,
	title, summary, opened_at, resolved_at, opened_by_event::text`

func (r *AttentionRepo) List(ctx context.Context, accountID, demandID string, includeResolved bool, limit int) ([]attention.Item, error) {
	// The FINAL ordering belongs to the domain (the priority depends on age,
	// which changes on its own). Here the order by opened_at exists only so the
	// LIMIT cuts the oldest ones deterministically instead of arbitrarily.
	rows, err := r.pool.Query(ctx, `
		SELECT `+attentionCols+`
		  FROM attention_items
		 WHERE account_id = $1
		   AND ($2 = '' OR demand_id = $2::uuid)
		   AND ($3 OR resolved_at IS NULL)
		 ORDER BY opened_at
		 LIMIT $4`, accountID, demandID, includeResolved, limit)
	if err != nil {
		return nil, Translate(err, "the attention box")
	}
	defer rows.Close()

	var items []attention.Item
	for rows.Next() {
		var it attention.Item
		var kind string
		var params []byte
		if err := rows.Scan(&it.ID, &it.AccountID, &kind, &it.TargetKind, &it.TargetID,
			&it.DemandID, &it.TitleKey, &params,
			&it.Title, &it.Summary, &it.OpenedAt, &it.ResolvedAt,
			&it.EventID); err != nil {
			return nil, Translate(err, "attention item")
		}
		// Unreadable params lose the parameters, never the item: the row is
		// still an open matter someone has to decide, and the English fallback
		// still says what it is.
		if len(params) > 0 {
			_ = json.Unmarshal(params, &it.Params)
		}
		it.Kind = attention.Kind(kind)
		items = append(items, it)
	}
	return items, Translate(rows.Err(), "the attention box")
}

func (r *AttentionRepo) OpenTotal(ctx context.Context, accountID string) (int, error) {
	var total int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM attention_items
		 WHERE account_id = $1 AND resolved_at IS NULL`, accountID).Scan(&total)
	return total, Translate(err, "the attention box's total")
}

var _ attention.Repository = (*AttentionRepo)(nil)
