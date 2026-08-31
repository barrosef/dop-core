package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// EventRepo implementa event.Repository — a leitura do log para o replay.
// É o ÚNICO lugar com SQL de `events`; o domínio nunca vê uma query.
type EventRepo struct{ pool *pgxpool.Pool }

func NewEventRepo(pool *pgxpool.Pool) *EventRepo { return &EventRepo{pool: pool} }

// eventCols traz os MESMOS campos de ports.Event, na mesma ordem.
//
// id e account_id saem como texto porque o domínio trabalha com string e não
// deve conhecer o tipo uuid do Postgres. O COALESCE existe porque account_id é
// nullable desde a migração 0003 — embora o WHERE por conta já garanta que
// nenhuma linha nula chegue até aqui.
const eventCols = `id::text, COALESCE(account_id::text,''), aggregate, aggregate_id,
	type, payload, occurred_at`

// Locate acha a posição do cursor.
//
// account_id entra no WHERE: id de evento de outra conta devolve "não
// encontrado", não a posição — a existência do id não vaza.
func (r *EventRepo) Locate(ctx context.Context, accountID, eventID string) (event.Cursor, error) {
	if !looksLikeUUID(eventID) {
		// Sem isto o driver devolveria "invalid input syntax for type uuid",
		// que viraria 500. Cursor malformado é erro do cliente.
		return event.Cursor{}, errs.Invalid("cursor de evento inválido")
	}
	var c event.Cursor
	err := r.pool.QueryRow(ctx,
		`SELECT id::text, occurred_at FROM events WHERE id = $1 AND account_id = $2`,
		eventID, accountID).Scan(&c.ID, &c.OccurredAt)
	if err != nil {
		return event.Cursor{}, Translate(err, "evento do cursor")
	}
	return c, nil
}

// EventsAfter lê a página seguinte do log, da conta, depois do cursor.
//
// A comparação é pelo PAR (occurred_at, id): a chave primária de `events` é
// composta e a tabela é particionada por occurred_at, então nem o instante
// sozinho (empata) nem o id sozinho (não ordena) servem de cursor. A tupla
// também casa com o índice events_account_idx.
func (r *EventRepo) EventsAfter(ctx context.Context, accountID string, after event.Cursor, f event.Filter, limit int) ([]ports.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+eventCols+`
		  FROM events
		 WHERE account_id = $1
		   AND (occurred_at, id) > ($2::timestamptz, $3::uuid)
		   AND (COALESCE(cardinality($4::text[]),0) = 0 OR aggregate = ANY($4))
		   AND (COALESCE(cardinality($5::text[]),0) = 0 OR type = ANY($5))
		 ORDER BY occurred_at, id
		 LIMIT $6`,
		accountID, after.OccurredAt, after.ID,
		notNil(f.Aggregates), notNil(f.Types), limit)
	if err != nil {
		return nil, Translate(err, "eventos")
	}
	defer rows.Close()

	out := make([]ports.Event, 0, limit)
	for rows.Next() {
		var e ports.Event
		if err := rows.Scan(&e.ID, &e.AccountID, &e.Aggregate, &e.AggregateID,
			&e.Type, &e.Payload, &e.OccurredAt); err != nil {
			return nil, Translate(err, "eventos")
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, Translate(err, "eventos")
	}
	return out, nil
}

// notNil evita que uma lista vazia vire NULL no fio: cardinality(NULL) é NULL,
// e a condição do filtro deixaria de casar com qualquer linha.
func notNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// looksLikeUUID checa a FORMA, não a versão: só o bastante para o driver não
// precisar rejeitar a query.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

var _ event.Repository = (*EventRepo)(nil)
