package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/idem"
)

// Idempotency guarda o resultado de cada escrita por chave.
//
// Com eventos e retries, repetir uma chamada não pode duplicar efeito
// (ADR-0017). A MESMA chave com corpo diferente é conflito, não repetição —
// senão um bug de cliente viraria corrupção silenciosa de dados.
type Idempotency struct{ pool *pgxpool.Pool }

func NewIdempotency(pool *pgxpool.Pool) *Idempotency { return &Idempotency{pool: pool} }

func (i *Idempotency) Begin(ctx context.Context, key, requestHash string, ttl time.Duration) ([]byte, bool, error) {
	if key == "" {
		return nil, false, nil // sem chave: segue sem proteção
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
		return nil, false, errs.Wrap(errs.KindInternal, err, "falha no controle de idempotência")
	}

	if existingHash != requestHash {
		return nil, false, errs.Conflict(
			"a chave de idempotência já foi usada com outro conteúdo")
	}
	if completed != nil {
		return response, true, nil // repetição legítima: devolve o gravado
	}
	return nil, false, nil // primeira vez (ou anterior não concluída): executa
}

func (i *Idempotency) Complete(ctx context.Context, key string, response []byte) error {
	if key == "" {
		return nil
	}
	_, err := i.pool.Exec(ctx,
		`UPDATE idempotency SET response = $2, completed_at = now() WHERE key = $1`,
		key, response)
	return Translate(err, "idempotência")
}

// CompleteTx grava dentro da transação do caso de uso — assim a marca de
// concluído só existe se a operação inteira tiver sido confirmada.
func CompleteTx(ctx context.Context, tx pgx.Tx, key string, response []byte) error {
	if key == "" {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE idempotency SET response = $2, completed_at = now() WHERE key = $1`,
		key, response)
	return Translate(err, "idempotência")
}

var _ idem.Store = (*Idempotency)(nil)
