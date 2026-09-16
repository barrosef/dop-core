package attention_test

import (
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/domain/attention"
)

var now = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func item(k attention.Kind, ageHours int) attention.Item {
	return attention.Item{Kind: k, OpenedAt: now.Add(-time.Duration(ageHours) * time.Hour)}
}

// The spec's rule: "a production merge queue > an exploratory question".
// This is the test that encodes it.
func TestMergeConflictComesBeforeExploratoryQuestion(t *testing.T) {
	conflict := item(attention.KindMergeConflict, 0)  // three minutes ago
	question := item(attention.KindThreadBlocked, 72) // three days ago

	if conflict.Priority(now) >= question.Priority(now) {
		t.Fatalf("a just-opened production conflict (%d) should come before a three-day-old question (%d)",
			conflict.Priority(now), question.Priority(now))
	}
}

// Age breaks ties WITHIN a band, and that is what keeps the box from forgetting
// an old item — without letting it cross bands.
func TestAgeBreaksTiesWithinTheSameBand(t *testing.T) {
	old := item(attention.KindPRReview, 48)
	fresh := item(attention.KindPRReview, 1)

	if old.Priority(now) >= fresh.Priority(now) {
		t.Fatal("among items of the same kind, the older one must come first")
	}
}

// If age crossed bands, a forgotten question would jump ahead of a production
// conflict. That is the inversion the spec forbids.
func TestAgeDoesNotCrossBands(t *testing.T) {
	ancientQuestion := item(attention.KindThreadBlocked, 24*365)
	conflictNow := item(attention.KindMergeConflict, 0)

	if ancientQuestion.Priority(now) <= conflictNow.Priority(now) {
		t.Fatal("age must not cross an impact band")
	}
}

func TestUnknownKindGoesToTheEndOfTheQueue(t *testing.T) {
	unknown := item(attention.Kind("made-up"), 0)
	worst := item(attention.KindThreadBlocked, 0)

	if unknown.Priority(now) <= worst.Priority(now) {
		t.Fatal("an unknown kind must not push a classified item down")
	}
}

func TestEveryKindHasADeclaredImpact(t *testing.T) {
	for _, k := range attention.Kinds() {
		if attention.ImpactOf(k) >= 999 {
			t.Errorf("kind %q has no impact in the table — it would silently land at the end of the queue", k)
		}
	}
}

// A clock running backwards must not become top priority.
func TestClockRunningBackwardsIsNotUrgency(t *testing.T) {
	future := attention.Item{Kind: attention.KindPRReview, OpenedAt: now.Add(time.Hour)}
	present := attention.Item{Kind: attention.KindPRReview, OpenedAt: now}

	if future.Priority(now) < present.Priority(now) {
		t.Fatal("an item with OpenedAt in the future must not be more urgent than one from now")
	}
}
