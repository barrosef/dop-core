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

// Attention constrói a caixa de atenção a partir do log.
//
// A DECISÃO de o que vira item mora no domínio (`attention.Apply`); aqui só
// acontece a gravação. Foi de propósito: decidir o que exige decisão humana é
// regra de negócio, e regra de negócio dentro de projeção é regra que ninguém
// consegue testar sem subir banco.
//
// Idempotente, como toda projeção precisa ser — a entrega é ao-menos-uma-vez.
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
		// Evento ilegível nunca melhora com retry.
		return nil
	}
	// Evento sem conta (user.ensured, migração 0003) não pertence a caixa
	// nenhuma — e a coluna tem FK para accounts.
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
	// Dois ON CONFLICT, e cada um protege de uma coisa diferente:
	// `opened_by_event` faz a REENTREGA do mesmo evento ser inócua; o índice
	// parcial de alvo aberto impede que um alvo que bloqueia, destrava e
	// bloqueia de novo acumule itens fantasmas.
	_, err := a.pool.Exec(ctx, `
		INSERT INTO attention_items
		       (account_id, kind, target_kind, target_id, demand_id,
		        title, summary, opened_at, opened_by_event)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,$8,$9)
		ON CONFLICT DO NOTHING`,
		it.AccountID, string(it.Kind), it.TargetKind, it.TargetID, it.DemandID,
		it.Title, it.Summary, it.OpenedAt, it.EventID)
	if err != nil {
		return err
	}
	logging.From(ctx).Debug("item de atenção aberto",
		"kind", string(it.Kind), "target", it.TargetID)
	return nil
}

func (a *Attention) fechar(ctx context.Context, accountID string, c attention.CloseSpec, quando time.Time) error {
	// Fecha por ALVO e só o que está aberto: reprocessar o log não pode mexer
	// no instante de resolução já gravado, senão a métrica de tempo de resposta
	// mudaria a cada reconstrução da projeção.
	tag, err := a.pool.Exec(ctx, `
		UPDATE attention_items SET resolved_at = $5
		 WHERE account_id = $1 AND kind = $2 AND target_kind = $3 AND target_id = $4
		   AND resolved_at IS NULL`,
		accountID, string(c.Kind), c.TargetKind, c.TargetID, quando)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		logging.From(ctx).Debug("item de atenção resolvido",
			"kind", string(c.Kind), "target", c.TargetID)
	}
	return nil
}
