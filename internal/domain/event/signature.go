package event

import (
	"context"
	"time"
)

// Origin says who placed a classification, and it is what protects a human's
// judgement from the machine's.
type Origin string

const (
	BySeed    Origin = "seed"
	ByLearned Origin = "learned"
	ByHuman   Origin = "human"
)

// ExhaustionsBeforeLearning is how many full retry budgets a signature has to
// burn before the platform stops believing it can recover.
//
// TWO, not one: once may be an outage that ended. Twice, with nothing ever
// having succeeded, is a broken call, and continuing to retry it spends the
// budget that belongs to failures that might actually recover.
//
// EXPORTED, not a literal repeated in the migration and the adapter's SQL: a
// number duplicated between Go and SQL is two rulers that can drift apart, and
// the one nobody remembers is the SQL one. Every place that needs it — this
// package's own Decide and the adapter's RecordExhausted — reads the same
// constant.
const ExhaustionsBeforeLearning = 2

// Signature is what the platform has learned about one (consumer, code) pair.
type Signature struct {
	Consumer       string
	Code           string
	Classification Classification
	ExhaustedCount int
	LastSuccessAt  time.Time
	ClassifiedBy   Origin
	FirstSeen      time.Time
	LastSeen       time.Time
}

// SignatureStore persists what was learned. A port: the domain states the rule,
// the adapter keeps the rows.
type SignatureStore interface {
	Get(ctx context.Context, consumer, code string) (*Signature, error)
	// RecordExhausted notes that this signature burned a whole retry budget,
	// creating the row from the seed when it is the first time.
	RecordExhausted(ctx context.Context, consumer, code string, seed Classification) error
	// RecordSuccess is the evidence that contradicts a mark.
	RecordSuccess(ctx context.Context, consumer, code string) error
}

// Decide is the classification in force for a signature, given the seed its
// error kind produces.
//
// The order matters and encodes three rules:
//
//  1. A human's mark wins over everything. They marked it for a reason the
//     system cannot see, and one accidental success must not undo that.
//  2. A success takes a machine's mark back. The learning can be wrong, and it
//     accepts evidence against itself.
//  3. Repetition without success promotes to irrecoverable.
//
// With no history, the seed decides.
func Decide(sig Signature, seed Classification) Classification {
	if sig.ClassifiedBy == ByHuman {
		return sig.Classification
	}
	if !sig.LastSuccessAt.IsZero() {
		return Recoverable
	}
	if sig.ExhaustedCount >= ExhaustionsBeforeLearning {
		return Irrecoverable
	}
	if sig.Classification != "" {
		return sig.Classification
	}
	return seed
}
