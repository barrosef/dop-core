package postgres

import (
	"context"
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
