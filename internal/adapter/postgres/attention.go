package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
)

// AttentionRepo é só LEITURA. A escrita mora na projeção
// (adapter/postgres/projection/attention.go), porque item de atenção nasce e
// morre de evento — e um repositório com Create seria um convite a criar item
// à mão, que faria a caixa deixar de ser projeção.
type AttentionRepo struct{ pool *pgxpool.Pool }

func NewAttentionRepo(pool *pgxpool.Pool) *AttentionRepo { return &AttentionRepo{pool: pool} }

const attentionCols = `id::text, account_id::text, kind, target_kind, target_id,
	COALESCE(demand_id::text,''), COALESCE(title_key,''), params,
	title, summary, opened_at, resolved_at, opened_by_event::text`

func (r *AttentionRepo) List(ctx context.Context, accountID, demandID string, includeResolved bool, limit int) ([]attention.Item, error) {
	// A ordenação FINAL é do domínio (a prioridade depende da idade, que muda
	// sozinha). Aqui a ordem por opened_at existe só para o LIMIT recortar os
	// mais antigos de forma determinística em vez de arbitrária.
	rows, err := r.pool.Query(ctx, `
		SELECT `+attentionCols+`
		  FROM attention_items
		 WHERE account_id = $1
		   AND ($2 = '' OR demand_id = $2::uuid)
		   AND ($3 OR resolved_at IS NULL)
		 ORDER BY opened_at
		 LIMIT $4`, accountID, demandID, includeResolved, limit)
	if err != nil {
		return nil, Translate(err, "caixa de atenção")
	}
	defer rows.Close()

	var itens []attention.Item
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
		itens = append(itens, it)
	}
	return itens, Translate(rows.Err(), "caixa de atenção")
}

func (r *AttentionRepo) OpenTotal(ctx context.Context, accountID string) (int, error) {
	var total int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM attention_items
		 WHERE account_id = $1 AND resolved_at IS NULL`, accountID).Scan(&total)
	return total, Translate(err, "total da caixa de atenção")
}

var _ attention.Repository = (*AttentionRepo)(nil)
