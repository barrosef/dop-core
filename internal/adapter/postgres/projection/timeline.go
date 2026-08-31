// Package projection constrói as LEITURAS derivadas do log de eventos.
//
// Regra da ADR-0006: a verdade é o log; dossiê, timeline, caixa de atenção,
// métricas e custo são PROJEÇÕES — computadas em código, custo de token zero.
// Nenhuma projeção escreve verdade nova; todas podem ser reconstruídas.
package projection

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// Timeline mantém a linha do tempo consultável por agregado.
//
// O handler é IDEMPOTENTE — obrigatório, porque a entrega do JetStream é
// ao-menos-uma-vez (ADR-0019). O ON CONFLICT DO NOTHING é o que faz a
// reentrega ser inócua em vez de duplicar linha.
type Timeline struct{ pool *pgxpool.Pool }

func NewTimeline(pool *pgxpool.Pool) *Timeline { return &Timeline{pool: pool} }

func (t *Timeline) Handle(ctx context.Context, e ports.Event) error {
	var env struct {
		ID          string          `json:"id"`
		AccountID   string          `json:"account_id"`
		Aggregate   string          `json:"aggregate"`
		AggregateID string          `json:"aggregate_id"`
		Type        string          `json:"type"`
		Payload     json.RawMessage `json:"payload"`
		OccurredAt  string          `json:"occurred_at"`
	}
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		// Evento ilegível nunca melhora com retry — o adaptador do NATS
		// descarta com registro em vez de travar a fila.
		return nil
	}

	_, err := t.pool.Exec(ctx, `
		INSERT INTO timeline (event_id, account_id, aggregate, aggregate_id, type, payload, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id) DO NOTHING`,
		env.ID, nullIfEmpty(env.AccountID), env.Aggregate, env.AggregateID,
		env.Type, env.Payload, env.OccurredAt)
	if err != nil {
		// Erro real (banco fora, por exemplo) devolve para o NATS reentregar.
		return err
	}

	logging.From(ctx).Debug("timeline projetada",
		"event_id", env.ID, "type", env.Type, "aggregate", env.Aggregate)
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
