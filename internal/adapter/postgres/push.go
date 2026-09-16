package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/domain/ports"
)

// PushRecorder turns a push to a project's root repository into an event
// (ADR-0028 §5, ADR-0006). It resolves the project's account first, because an
// event without an account is an event nobody can read back with the account
// filter every projection applies.
type PushRecorder struct{ pool *pgxpool.Pool }

func NewPushRecorder(pool *pgxpool.Pool) *PushRecorder { return &PushRecorder{pool: pool} }

// Record writes the event and returns the project's account.
func (r *PushRecorder) Record(ctx context.Context, p ports.Push) (string, error) {
	var accountID string
	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT account_id FROM projects WHERE id = $1`, p.ProjectID).Scan(&accountID); err != nil {
			return Translate(err, "project")
		}
		return Emit(ctx, tx, ports.Event{
			AccountID: accountID, Aggregate: "project_repository", AggregateID: p.ProjectID,
			Type: "dop.knowledge.pushed",
			Payload: mustJSON(map[string]any{
				"demand_id": p.DemandID, "ref": p.Ref, "before": p.Before, "after": p.After,
			}),
		})
	})
	return accountID, err
}
