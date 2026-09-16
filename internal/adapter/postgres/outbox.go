// The transactional outbox — the mechanism that gives atomicity WITHOUT 2PC
// (ADR-0019).
//
// The rule, in one sentence: every state change writes, in the SAME transaction,
// the new state and the event. A commit ⇒ atomic by construction. Never "I
// wrote it but did not publish it".
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

type Outbox struct{ pool *pgxpool.Pool }

func NewOutbox(pool *pgxpool.Pool) *Outbox { return &Outbox{pool: pool} }

// Emit writes the event + the outbox row inside a transaction ALREADY OPENED by
// the use case. It takes a pgx.Tx on purpose: if it accepted the pool, somebody
// would end up publishing outside the transaction — which is exactly the loss
// window the outbox closes.
func Emit(ctx context.Context, tx pgx.Tx, e ports.Event) error {
	call, _ := ctxutil.From(ctx)
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if len(e.Payload) == 0 {
		e.Payload = []byte(`{}`)
	}

	// Not every event has an account: `user.ensured` happens on the first login,
	// before the personal account exists. An empty string is not a UUID — it
	// goes in as NULL.
	var accountID any
	if e.AccountID != "" {
		accountID = e.AccountID
	}
	var actorID any
	if call.ActorID != "" {
		actorID = call.ActorID
	}

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO events (account_id, aggregate, aggregate_id, type, payload,
		                    actor_kind, actor_id, request_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		accountID, e.Aggregate, e.AggregateID, e.Type, e.Payload,
		string(call.ActorKind), actorID, call.RequestID, e.OccurredAt,
	).Scan(&id)
	if err != nil {
		return errs.Wrap(errs.KindInternal, err, "failed to write the event")
	}

	env := envelopeOf(id, e, call)

	// The same transaction. It is the entire point of the pattern.
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (event_id, occurred_at, subject, payload)
		VALUES ($1,$2,$3,$4)`,
		id, e.OccurredAt, Subject(e.Type), env,
	); err != nil {
		return errs.Wrap(errs.KindInternal, err, "failed to enqueue the event in the outbox")
	}
	return nil
}

// envelopeOf builds the wire envelope the relay publishes. Extracted from Emit
// so the fields that cross can be asserted without a database: this is the
// boundary where the context used to be silently dropped.
func envelopeOf(id string, e ports.Event, call ctxutil.Call) []byte {
	env, _ := json.Marshal(map[string]any{
		"id": id, "account_id": e.AccountID, "aggregate": e.Aggregate,
		"aggregate_id": e.AggregateID, "aggregate_key": e.AggregateKey,
		"type": e.Type, "payload": json.RawMessage(e.Payload),
		"occurred_at": e.OccurredAt,
		"actor_kind":  string(call.ActorKind), "actor_id": call.ActorID,
		"request_id": call.RequestID, "session_id": call.SessionID,
		"caller": call.Caller,
	})
	return env
}

// Subject derives the NATS subject from the event's type:
// demand.stage.advanced becomes dop.demand.stage.advanced.
func Subject(eventType string) string {
	return "dop." + strings.TrimPrefix(eventType, "dop.")
}

// Relay reads the outbox and publishes to the broker, marking what it
// published. At-least-once delivery: consumers MUST be idempotent.
type Relay struct {
	pool  *pgxpool.Pool
	bus   ports.EventBus
	batch int
}

func NewRelay(pool *pgxpool.Pool, bus ports.EventBus, batch int) *Relay {
	if batch <= 0 {
		batch = 100
	}
	return &Relay{pool: pool, bus: bus, batch: batch}
}

// Drain publishes a batch of pending rows. It returns how many were published.
//
// FOR UPDATE SKIP LOCKED allows several relay replicas without publishing the
// same event twice — and without one blocking the other.
func (r *Relay) Drain(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, errs.Wrap(errs.KindUnavailable, err, "failed to open the relay's transaction")
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT event_id, subject, payload, occurred_at
		  FROM outbox
		 WHERE published_at IS NULL
		 ORDER BY occurred_at
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, r.batch)
	if err != nil {
		return 0, errs.Wrap(errs.KindInternal, err, "failed to read the outbox")
	}

	type pending struct {
		id      string
		subject string
		payload []byte
		at      time.Time
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.subject, &p.payload, &p.at); err != nil {
			rows.Close()
			return 0, errs.Wrap(errs.KindInternal, err, "unreadable outbox row")
		}
		batch = append(batch, p)
	}
	rows.Close()
	if len(batch) == 0 {
		return 0, tx.Commit(ctx)
	}

	published := make([]string, 0, len(batch))
	for _, p := range batch {
		ev := ports.Event{ID: p.id, Type: p.subject, Payload: p.payload, OccurredAt: p.at}
		if err := r.bus.Publish(ctx, ev); err != nil {
			// A publication failure does not lose the event: it stays pending
			// and the next cycle tries again.
			_, _ = tx.Exec(ctx, `
				UPDATE outbox SET attempts = attempts + 1, last_error = $2
				 WHERE event_id = $1`, p.id, err.Error())
			continue
		}
		published = append(published, p.id)
	}

	if len(published) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE outbox SET published_at = now() WHERE event_id = ANY($1)`, published,
		); err != nil {
			return 0, errs.Wrap(errs.KindInternal, err, "failed to mark rows as published")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, errs.Wrap(errs.KindInternal, err, "failed to commit the relay")
	}
	return len(published), nil
}

// Run keeps the relay draining until the context ends.
func (r *Relay) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			for {
				n, err := r.Drain(ctx)
				if err != nil {
					return fmt.Errorf("relay: %w", err)
				}
				if n < r.batch {
					break // the batch is empty; wait for the next tick
				}
			}
		}
	}
}
