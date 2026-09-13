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
