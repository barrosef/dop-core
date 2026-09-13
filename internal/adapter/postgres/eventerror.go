package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
)

// EventErrorRepo implements event.SignatureStore. It is the ONLY place with
// error_signatures SQL — the domain never sees a query.
type EventErrorRepo struct{ pool *pgxpool.Pool }

func NewEventErrorRepo(pool *pgxpool.Pool) *EventErrorRepo { return &EventErrorRepo{pool: pool} }

func (r *EventErrorRepo) Get(ctx context.Context, consumer, code string) (*event.Signature, error) {
	var s event.Signature
	var classification, classifiedBy string
	var lastSuccess *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT consumer, code, classification, exhausted_count, last_success_at,
		       classified_by, first_seen, last_seen
		  FROM error_signatures WHERE consumer = $1 AND code = $2`,
		consumer, code).Scan(&s.Consumer, &s.Code, &classification,
		&s.ExhaustedCount, &lastSuccess, &classifiedBy, &s.FirstSeen, &s.LastSeen)
	if NoRows(err) {
		// Not an error: a signature nobody has seen yet is the normal case, and
		// the caller falls back to the seed.
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "the error signature")
	}
	s.Classification = event.Classification(classification)
	s.ClassifiedBy = event.Origin(classifiedBy)
	if lastSuccess != nil {
		s.LastSuccessAt = *lastSuccess
	}
	return &s, nil
}

func (r *EventErrorRepo) RecordExhausted(ctx context.Context, consumer, code string, seed event.Classification) error {
	// The seed only applies on INSERT. On conflict the stored classification is
	// left alone unless the promotion rule fires — overwriting it on every
	// failure would erase a human's mark, and would also erase a classification
	// the learning already settled on.
	//
	// The promotion also requires last_success_at IS NULL. Without it, this
	// SQL and event.Decide disagree: RecordSuccess is evidence a success
	// happened but leaves exhausted_count untouched (before this fix) or
	// resets it to zero (after it) — either way, a signature can carry BOTH a
	// recorded success and, from exhaustions that came after it, a count past
	// the threshold. Decide already treats a recorded success as decisive over
	// the exhaustion count (§ Decide, rule 2): "burned every retry more than
	// once AND has never succeeded" is the actual spec rule, and the second
	// half — AND never succeeded — was missing here. Checking it here makes
	// the stored `classification` column agree with what Decide computes,
	// instead of a reader of the raw row (a panel, a report, any caller that
	// does not route through Decide) seeing a different answer than the
	// domain gives.
	_, err := r.pool.Exec(ctx, `
		INSERT INTO error_signatures (consumer, code, classification, exhausted_count, classified_by)
		VALUES ($1, $2, $3, 1, 'seed')
		ON CONFLICT (consumer, code) DO UPDATE
		   SET exhausted_count = error_signatures.exhausted_count + 1,
		       last_seen = now(),
		       classification = CASE
		         WHEN error_signatures.classified_by = 'human' THEN error_signatures.classification
		         WHEN error_signatures.exhausted_count + 1 >= $4
		              AND error_signatures.last_success_at IS NULL THEN 'irrecoverable'
		         ELSE error_signatures.classification END,
		       classified_by = CASE
		         WHEN error_signatures.classified_by = 'human' THEN 'human'
		         WHEN error_signatures.exhausted_count + 1 >= $4
		              AND error_signatures.last_success_at IS NULL THEN 'learned'
		         ELSE error_signatures.classified_by END`,
		consumer, code, string(seed), event.ExhaustionsBeforeLearning)
	return Translate(err, "the error signature")
}

func (r *EventErrorRepo) RecordSuccess(ctx context.Context, consumer, code string) error {
	// A success is evidence against a machine's mark, and never against a
	// human's. exhausted_count resets to zero: after a success, the count
	// means "exhaustions since the last success" — under any other reading
	// the number does not mean what RecordExhausted's threshold check needs
	// it to mean, and a later exhaustion would count budgets burned before
	// the success towards a promotion that should only fire on budgets burned
	// SINCE it.
	_, err := r.pool.Exec(ctx, `
		UPDATE error_signatures
		   SET last_success_at = now(), last_seen = now(),
		       exhausted_count = 0,
		       classification = CASE WHEN classified_by = 'human'
		                             THEN classification ELSE 'recoverable' END,
		       classified_by  = CASE WHEN classified_by = 'human'
		                             THEN 'human' ELSE 'learned' END
		 WHERE consumer = $1 AND code = $2`, consumer, code)
	return Translate(err, "the error signature")
}

var _ event.SignatureStore = (*EventErrorRepo)(nil)

// EventErrorRow is the terminal row's column values, built from a dead letter.
// Exported and separated from the INSERT so the mapping can be asserted without
// a database — the mapping is where the context gets silently dropped.
type EventErrorRow struct {
	EventID, Consumer, AccountID      string
	EventType, Aggregate, AggregateID string
	AggregateKey, ActorKind, ActorID  string
	RequestID, SessionID, Caller      string
	Classification                    string
	LastCode, LastMessage             string
	Attempts                          []byte
	BrokerAttempts                    int
}

// EventErrorColumns maps a DeadLetter, plus the classification decided at the
// end of the road, to the terminal row's columns. Kept apart from the INSERT
// on purpose: the mapping is exactly where a field from ports.Event's caller
// context (AggregateKey, ActorID, RequestID, ...) can be silently dropped, and
// a pure function is the part of this task a test can hold without a database.
func EventErrorColumns(dl event.DeadLetter, final event.Classification) EventErrorRow {
	// Attempts marshals the WHOLE history, DLQ rounds included — the question
	// this table answers is "what happened, exactly", and an answer needing a
	// second query is an answer nobody looks up. json.Marshal on a []Attempt
	// slice of plain fields cannot fail; the error is discarded rather than
	// threaded through a mapping the caller expects to be pure.
	attempts, _ := json.Marshal(dl.Attempts)
	row := EventErrorRow{
		EventID:        dl.Event.ID,
		Consumer:       dl.Consumer,
		AccountID:      dl.Event.AccountID,
		EventType:      dl.Event.Type,
		Aggregate:      dl.Event.Aggregate,
		AggregateID:    dl.Event.AggregateID,
		AggregateKey:   dl.Event.AggregateKey,
		ActorKind:      dl.Event.ActorKind,
		ActorID:        dl.Event.ActorID,
		RequestID:      dl.Event.RequestID,
		SessionID:      dl.Event.SessionID,
		Caller:         dl.Event.Caller,
		Classification: string(final),
		Attempts:       attempts,
		BrokerAttempts: dl.BrokerAttempts,
	}
	if n := len(dl.Attempts); n > 0 {
		row.LastCode = dl.Attempts[n-1].ErrorCode
		row.LastMessage = dl.Attempts[n-1].ErrorMessage
	}
	return row
}

// Record writes the terminal row. Called once, when a dead letter has burned
// its retries and its DLQ rounds — nothing downstream of this reads the event
// again, so everything a reader would need goes in now.
//
// ON CONFLICT DO NOTHING on (event_id, consumer) — the same key the dead
// letter's own dedup uses — is what makes this safe to call twice: a
// redelivery of DLQConsumer.Handle after this INSERT committed but something
// later in the same call failed (RecordExhausted, for instance) must not
// produce a second row, the same way `timeline`'s own idempotent INSERT
// already guards against a duplicate projection.
func (r *EventErrorRepo) Record(ctx context.Context, dl event.DeadLetter, final event.Classification) error {
	c := EventErrorColumns(dl, final)
	// An account is not guaranteed: `user.ensured` happens before the personal
	// account exists. An empty string is not a uuid — it goes in as NULL rather
	// than failing the insert on a type the column would reject.
	var accountID any
	if c.AccountID != "" {
		accountID = c.AccountID
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO event_errors (event_id, consumer, account_id, event_type,
		    aggregate, aggregate_id, aggregate_key, actor_kind, actor_id,
		    request_id, session_id, caller, attempts, broker_attempts,
		    classification, last_code, last_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (event_id, consumer) DO NOTHING`,
		c.EventID, c.Consumer, accountID, c.EventType, c.Aggregate, c.AggregateID,
		c.AggregateKey, c.ActorKind, c.ActorID, c.RequestID, c.SessionID, c.Caller,
		c.Attempts, c.BrokerAttempts, c.Classification, c.LastCode, c.LastMessage)
	return Translate(err, "the event error")
}

var _ event.ErrorStore = (*EventErrorRepo)(nil)
