package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/idem"
)

// Idempotency stores each write's result by key.
//
// With events and retries, repeating a call must not duplicate an effect
// (ADR-0017). The SAME key with a different body is a conflict, not a repetition
// — otherwise a client bug would become silent data corruption.
type Idempotency struct{ pool *pgxpool.Pool }

func NewIdempotency(pool *pgxpool.Pool) *Idempotency { return &Idempotency{pool: pool} }

func (i *Idempotency) Begin(ctx context.Context, key, requestHash string, ttl time.Duration) ([]byte, bool, error) {
	if key == "" {
		return nil, false, nil // no key: it goes on unprotected
	}
	if ttl <= 0 {
		ttl = idem.DefaultTTL
	}

	var existingHash string
	var response []byte
	var completed *time.Time

	err := i.pool.QueryRow(ctx, `
		INSERT INTO idempotency (key, request_hash, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (key) DO UPDATE SET key = EXCLUDED.key
		RETURNING request_hash, response, completed_at`,
		key, requestHash, ttl.String(),
	).Scan(&existingHash, &response, &completed)
	if err != nil {
		return nil, false, errs.Wrap(errs.KindInternal, err, "failure in the idempotency control")
	}

	if existingHash != requestHash {
		return nil, false, errs.Conflict(
			"the idempotency key has already been used with different content")
	}
	if completed != nil {
		return response, true, nil // a legitimate repetition: return what was stored
	}
	return nil, false, nil // the first time (or a previous unfinished one): run it
}

func (i *Idempotency) Complete(ctx context.Context, key string, response []byte) error {
	if key == "" {
		return nil
	}
	_, err := i.pool.Exec(ctx,
		`UPDATE idempotency SET response = $2, completed_at = now() WHERE key = $1`,
		key, response)
	return Translate(err, "idempotency")
}

// CompleteTx writes inside the use case's transaction — that way the completion
// mark only exists if the whole operation was committed.
func CompleteTx(ctx context.Context, tx pgx.Tx, key string, response []byte) error {
	if key == "" {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE idempotency SET response = $2, completed_at = now() WHERE key = $1`,
		key, response)
	return Translate(err, "idempotency")
}

var _ idem.Store = (*Idempotency)(nil)
