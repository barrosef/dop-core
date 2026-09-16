package reaction_test

import (
	"context"
	"sync"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/domain/reaction"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// handlerFunc adapts a plain function to reaction.Handler, the way
// http.HandlerFunc adapts a func to http.Handler — so a test can hand Execute
// a closure instead of a struct with dependencies.
type handlerFunc func(context.Context, reaction.PlannedAction) error

func (f handlerFunc) Run(ctx context.Context, p reaction.PlannedAction) error { return f(ctx, p) }

// fakeApplied is an in-memory Applied. It races correctly under a single
// mutex, which is exactly why it cannot stand in for the concurrency test:
// the real question — do two concurrent Postgres inserts on the same triple
// really yield one winner — can only be asked of Postgres, and that test lives
// in test/integration/reaction_test.go.
type fakeApplied struct {
	mu      sync.Mutex
	claimed map[string]bool
}

func newFakeApplied() *fakeApplied { return &fakeApplied{claimed: map[string]bool{}} }

func appliedKey(eventID, ruleRef, actionName string) string {
	return eventID + "/" + ruleRef + "/" + actionName
}

func (f *fakeApplied) MarkApplied(_ context.Context, eventID, ruleRef, actionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := appliedKey(eventID, ruleRef, actionName)
	if f.claimed[k] {
		return false, nil
	}
	f.claimed[k] = true
	return true, nil
}

func (f *fakeApplied) Release(_ context.Context, eventID, ruleRef, actionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claimed, appliedKey(eventID, ruleRef, actionName))
	return nil
}

func (f *fakeApplied) has(eventID, ruleRef, actionName string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claimed[appliedKey(eventID, ruleRef, actionName)]
}

func TestAnActionThatAlreadyRanIsNotRunAgain(t *testing.T) {
	ran := 0
	reg := reaction.Registry{reaction.ActionSendEmail: handlerFunc(func(context.Context, reaction.PlannedAction) error {
		ran++
		return nil
	})}
	applied := newFakeApplied()
	plan := []reaction.PlannedAction{{RuleRef: "r-1", Name: reaction.ActionSendEmail, Event: ports.Event{ID: "ev-1"}}}

	if err := reaction.Execute(context.Background(), reg, applied, plan); err != nil {
		t.Fatal(err)
	}
	if err := reaction.Execute(context.Background(), reg, applied, plan); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatalf("the same action ran %d times; the gate did not hold", ran)
	}
}

func TestOneActionFailingDoesNotStopTheOthers(t *testing.T) {
	// An e-mail that does not go out must never stop an attention item from
	// opening. The delivery is nacked so the failed one is retried, and the
	// gate means the successful ones are skipped on redelivery.
	var opened bool
	reg := reaction.Registry{
		reaction.ActionSendEmail: handlerFunc(func(context.Context, reaction.PlannedAction) error {
			return errs.New(errs.KindUnavailable, "the mail server is down")
		}),
		reaction.ActionOpenAttention: handlerFunc(func(context.Context, reaction.PlannedAction) error {
			opened = true
			return nil
		}),
	}
	applied := newFakeApplied()
	err := reaction.Execute(context.Background(), reg, applied, []reaction.PlannedAction{
		{RuleRef: "r-1", Name: reaction.ActionSendEmail, Event: ports.Event{ID: "ev-1"}},
		{RuleRef: "r-1", Name: reaction.ActionOpenAttention, Event: ports.Event{ID: "ev-1"}},
	})
	if err == nil {
		t.Fatal("the failure has to reach the caller so the delivery is nacked")
	}
	if !opened {
		t.Fatal("the second action did not run: one failure stopped the others")
	}
	if applied.has("ev-1", "r-1", string(reaction.ActionSendEmail)) {
		t.Fatal("a failed action was marked applied; a retry would skip it")
	}
	if !applied.has("ev-1", "r-1", string(reaction.ActionOpenAttention)) {
		t.Fatal("a successful action was not marked; a retry would run it twice")
	}
}

func TestAnUnregisteredActionIsAContractErrorNotASilentSkip(t *testing.T) {
	// The vocabulary is validated when a rule is written, so reaching here means
	// a rule survived a deploy that removed its handler. Skipping quietly would
	// make a policy stop working with nothing to read.
	reg := reaction.Registry{}
	err := reaction.Execute(context.Background(), reg, newFakeApplied(), []reaction.PlannedAction{
		{RuleRef: "r-1", Name: reaction.ActionSendEmail, Event: ports.Event{ID: "ev-1"}},
	})
	if errs.KindOf(err) != errs.KindInternal {
		t.Fatalf("expected an internal error naming the action, got %v", err)
	}
}
