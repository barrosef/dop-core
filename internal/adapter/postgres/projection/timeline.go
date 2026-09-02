// Package projection builds the READS derived from the event log.
//
// ADR-0006's rule: the truth is the log; the dossier, the timeline, the
// attention box, the metrics and the cost are PROJECTIONS — computed in code, at
// zero token cost. No projection writes new truth; all of them can be rebuilt.
package projection

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// Timeline keeps the timeline queryable by aggregate.
//
// The handler is IDEMPOTENT — mandatory, because JetStream's delivery is
// at-least-once (ADR-0019). The ON CONFLICT DO NOTHING is what makes the
// redelivery harmless instead of duplicating a row.
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
		// An unreadable event never improves with a retry — the NATS adapter
		// discards it with a record instead of blocking the queue.
		return nil
	}

	_, err := t.pool.Exec(ctx, `
		INSERT INTO timeline (event_id, account_id, aggregate, aggregate_id, type, payload, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id) DO NOTHING`,
		env.ID, nullIfEmpty(env.AccountID), env.Aggregate, env.AggregateID,
		env.Type, env.Payload, env.OccurredAt)
	if err != nil {
		// A real error (the database being down, say) goes back for NATS to redeliver.
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
