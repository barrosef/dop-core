package event_test

import (
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
)

func TestASignatureNobodyHasSeenFollowsTheSeed(t *testing.T) {
	if got := event.Decide(event.Signature{}, event.Recoverable); got != event.Recoverable {
		t.Fatalf("with no history the seed must win, got %s", got)
	}
}

func TestBurningEveryRetryTwiceWithNoSuccessBecomesIrrecoverable(t *testing.T) {
	// Once may be an outage. Twice, with nothing ever working, is a broken
	// call — and continuing to retry it spends the budget that belongs to
	// failures that might actually recover.
	sig := event.Signature{
		Classification: event.Recoverable, ClassifiedBy: event.BySeed,
		ExhaustedCount: 2,
	}
	if got := event.Decide(sig, event.Recoverable); got != event.Irrecoverable {
		t.Fatalf("expected the learning to demote it, got %s", got)
	}
}

func TestASuccessTakesTheMarkBack(t *testing.T) {
	// The learning can be wrong, so it accepts evidence against itself.
	sig := event.Signature{
		Classification: event.Irrecoverable, ClassifiedBy: event.ByLearned,
		ExhaustedCount: 3, LastSuccessAt: time.Now(),
	}
	if got := event.Decide(sig, event.Unknown); got != event.Recoverable {
		t.Fatalf("a success must undo a learned mark, got %s", got)
	}
}

func TestAHumanMarkSurvivesAnAccidentalSuccess(t *testing.T) {
	// Somebody marked it for a reason the system cannot see. One success must
	// not throw that judgement away; another human moves it.
	sig := event.Signature{
		Classification: event.Irrecoverable, ClassifiedBy: event.ByHuman,
		LastSuccessAt: time.Now(),
	}
	if got := event.Decide(sig, event.Recoverable); got != event.Irrecoverable {
		t.Fatalf("a human mark was overwritten by the machine, got %s", got)
	}
}

// storeExhausted and storeSuccess mirror, in Go, exactly what
// internal/adapter/postgres/eventerror.go's SQL does to a row in
// RecordExhausted and RecordSuccess. The domain package cannot call the real
// SQL (importing the postgres package here would cycle back), so this is the
// closest a `make test` run — no Postgres, no environment — can come to
// exercising the adapter's actual rule. If eventerror.go's CASE expressions
// ever change, this mirror has to change with them, on purpose: the whole
// point of TestTheStoreAndDecideNeverDisagree below is to fail loudly the
// moment the two drift, which is exactly the bug this test was written for
// (round 1 of Task 4's review: RecordExhausted promoted to irrecoverable
// without checking last_success_at, and RecordSuccess never reset
// exhausted_count — so the stored `classification` column and event.Decide
// could name two different classifications for the same signature).
func storeExhausted(sig event.Signature, seed event.Classification) event.Signature {
	if sig.ClassifiedBy == "" {
		// The INSERT branch: the seed only applies the first time a signature
		// is seen.
		return event.Signature{
			Consumer: sig.Consumer, Code: sig.Code,
			Classification: seed, ClassifiedBy: event.BySeed, ExhaustedCount: 1,
		}
	}
	next := sig
	next.ExhaustedCount = sig.ExhaustedCount + 1
	promotes := next.ExhaustedCount >= event.ExhaustionsBeforeLearning && sig.LastSuccessAt.IsZero()
	switch {
	case sig.ClassifiedBy == event.ByHuman:
		// classification and classified_by are left exactly as they were.
	case promotes:
		next.Classification, next.ClassifiedBy = event.Irrecoverable, event.ByLearned
	default:
		// classification and classified_by are left exactly as they were.
	}
	return next
}

func storeSuccess(sig event.Signature) event.Signature {
	next := sig
	next.LastSuccessAt = time.Now()
	next.ExhaustedCount = 0
	if sig.ClassifiedBy != event.ByHuman {
		next.Classification, next.ClassifiedBy = event.Recoverable, event.ByLearned
	}
	return next
}

// TestTheStoreAndDecideNeverDisagree walks the exact sequence from the round-1
// review finding: two exhaustions with no success, a success, then one more
// exhaustion. At every step, the classification the store would persist in
// the `classification` column must be the same one event.Decide computes from
// that row — a reader of the raw row (a panel, a report, any future caller
// that does not route through Decide) must never see an answer that
// contradicts the domain rule.
//
// Before the round-1 fix, step 3 broke this: the promotion branch fired on
// exhausted_count alone, so a signature could be promoted back to
// irrecoverable while last_success_at stayed set — and Decide, which checks
// LastSuccessAt before ExhaustedCount, kept answering Recoverable for the same
// row.
func TestTheStoreAndDecideNeverDisagree(t *testing.T) {
	seed := event.Recoverable
	sig := event.Signature{Consumer: "worker", Code: "unavailable"}

	step := func(name string, next event.Signature) event.Signature {
		t.Helper()
		if got, want := next.Classification, event.Decide(next, seed); got != want {
			t.Fatalf("%s: store holds %s but Decide says %s for the same row: %+v", name, got, want, next)
		}
		return next
	}

	sig = step("first exhaustion", storeExhausted(sig, seed))
	sig = step("second exhaustion (no success yet)", storeExhausted(sig, seed))
	if sig.Classification != event.Irrecoverable {
		t.Fatalf("setup: expected two exhaustions with no success to promote, got %s", sig.Classification)
	}

	sig = step("a success", storeSuccess(sig))
	if sig.Classification != event.Recoverable {
		t.Fatalf("setup: expected the success to take the mark back, got %s", sig.Classification)
	}

	sig = step("one further exhaustion after the success", storeExhausted(sig, seed))
	if sig.Classification != event.Recoverable {
		t.Fatalf("a single exhaustion right after a success must not re-promote, got %s", sig.Classification)
	}
}
