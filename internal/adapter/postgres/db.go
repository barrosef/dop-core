// Infraestrutura compartilhada de acesso ao Postgres.
//
// O padrão que TODO caso de uso segue: abrir transação, mudar estado, emitir
// evento na MESMA transação, confirmar. É o que dá atomicidade sem 2PC
// (ADR-0019) — e é por isso que Emit recebe pgx.Tx, não o pool.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// InTx executa fn dentro de uma transação, com rollback automático em erro.
//
//	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
//	    if err := gravarEstado(ctx, tx); err != nil { return err }
//	    return postgres.Emit(ctx, tx, evento)   // mesma transação
//	})
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "falha ao abrir transação")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.KindInternal, err, "falha ao confirmar transação")
	}
	return nil
}

// Códigos do Postgres que precisamos distinguir.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeRaiseException      = "P0001" // RAISE EXCEPTION das nossas triggers
)

// Translate converte erro do driver em erro de domínio. Sem isso, uma violação
// de unicidade viraria 500 em vez de 409.
func Translate(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound("%s não encontrado", what)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeUniqueViolation:
			return errs.New(errs.KindAlreadyExists, "%s já existe", what)
		case codeForeignKeyViolation:
			return errs.Invalid("referência inválida em %s", what)
		case codeCheckViolation:
			return errs.Invalid("%s viola uma restrição de integridade", what)
		case codeRaiseException:
			// Mensagem das nossas triggers de invariante — é regra de negócio,
			// então vai para o usuário como veio.
			return errs.Precondition("%s", pgErr.Message)
		}
	}
	return errs.Wrap(errs.KindInternal, err, "falha ao acessar %s", what)
}

// NoRows diz se o erro é "nada encontrado" — útil em consultas opcionais.
func NoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// pgxQuerier é o mínimo que uma leitura precisa: pool e tx satisfazem os dois.
// Existe para que um mesmo carregador sirva dentro e fora de transação.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}
