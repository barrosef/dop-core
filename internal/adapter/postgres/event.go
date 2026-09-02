package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// EventRepo implements event.Repository — reading the log for the replay. It is
// the ONLY place with `events` SQL; the domain never sees a query.
type EventRepo struct{ pool *pgxpool.Pool }

func NewEventRepo(pool *pgxpool.Pool) *EventRepo { return &EventRepo{pool: pool} }

// eventCols brings the SAME fields as ports.Event, in the same order.
//
// id and account_id come out as text because the domain works with strings and
// must not know Postgres's uuid type. The COALESCE exists because account_id has
// been nullable since migration 0003 — although the WHERE by account already
// guarantees no null row reaches here.
const eventCols = `id::text, COALESCE(account_id::text,''), aggregate, aggregate_id,
	type, payload, occurred_at`

// Locate finds the cursor's position.
//
// account_id goes into the WHERE: an event id from another account returns "not
// found", not the position — the id's existence does not leak.
func (r *EventRepo) Locate(ctx context.Context, accountID, eventID string) (event.Cursor, error) {
	if !looksLikeUUID(eventID) {
		// Without this the driver would return "invalid input syntax for type
		// uuid", which would become a 500. A malformed cursor is the client's
		// error.
		return event.Cursor{}, errs.Invalid("invalid event cursor")
	}
	var c event.Cursor
	err := r.pool.QueryRow(ctx,
		`SELECT id::text, occurred_at FROM events WHERE id = $1 AND account_id = $2`,
		eventID, accountID).Scan(&c.ID, &c.OccurredAt)
	if err != nil {
		return event.Cursor{}, Translate(err, "the cursor's event")
	}
	return c, nil
}

// EventsAfter reads the log's next page, for the account, after the cursor.
//
// The comparison is by the PAIR (occurred_at, id): `events`'s primary key is
// composite and the table is partitioned by occurred_at, so neither the instant
// alone (it ties) nor the id alone (it does not order) works as a cursor. The
// tuple also matches the events_account_idx index.
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
		return nil, Translate(err, "events")
	}
	defer rows.Close()

	out := make([]ports.Event, 0, limit)
	for rows.Next() {
		var e ports.Event
		if err := rows.Scan(&e.ID, &e.AccountID, &e.Aggregate, &e.AggregateID,
			&e.Type, &e.Payload, &e.OccurredAt); err != nil {
			return nil, Translate(err, "events")
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, Translate(err, "events")
	}
	return out, nil
}

// notNil stops an empty list from becoming NULL on the wire: cardinality(NULL)
// is NULL, and the filter's condition would stop matching any row.
func notNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// looksLikeUUID checks the SHAPE, not the version: just enough for the driver
// not to have to reject the query.
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
