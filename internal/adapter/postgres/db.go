// Shared Postgres access infrastructure.
//
// The pattern EVERY use case follows: open a transaction, change state, emit the
// event in the SAME transaction, commit. It is what gives atomicity without 2PC
// (ADR-0014) — and it is why Emit takes a pgx.Tx, not the pool.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/platform/errs"
)

// InTx runs fn inside a transaction, with an automatic rollback on error.
//
//	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
//	    if err := writeState(ctx, tx); err != nil { return err }
//	    return postgres.Emit(ctx, tx, event)   // the same transaction
//	})
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to open a transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.KindInternal, err, "failed to commit the transaction")
	}
	return nil
}

// The Postgres codes we need to tell apart.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeRaiseException      = "P0001" // RAISE EXCEPTION from our triggers
)

// Translate converts a driver error into a domain error. Without it, a
// uniqueness violation would become a 500 instead of a 409.
func Translate(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound("%s not found", what)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeUniqueViolation:
			return errs.New(errs.KindAlreadyExists, "%s already exists", what)
		case codeForeignKeyViolation:
			return errs.Invalid("invalid reference in %s", what)
		case codeCheckViolation:
			return errs.Invalid("%s violates an integrity constraint", what)
		case codeRaiseException:
			// The message from our invariant triggers — it is a business rule,
			// so it goes to the user as it came.
			return errs.Precondition("%s", pgErr.Message)
		}
	}
	return errs.Wrap(errs.KindInternal, err, "failed to access %s", what)
}

// NoRows says whether the error is "nothing found" — useful in optional queries.
func NoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// pgxQuerier is the minimum a read needs: both the pool and a tx satisfy it. It
// exists so the same loader works inside and outside a transaction.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}
