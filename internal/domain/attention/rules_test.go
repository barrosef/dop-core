package attention_test

import (
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
)

func ev(kind string, payload map[string]any) attention.Event {
	return attention.Event{
		ID: "ev-1", AccountID: "acc-1", Aggregate: "demand", AggregateID: "dem-1",
		Type: kind, OccurredAt: now, Payload: payload,
	}
}

func TestBlockedThreadOpensAnItemThatLeadsToTheThread(t *testing.T) {
	d := attention.Apply(ev(attention.EvThreadBlocked, map[string]any{
		"thread_id": "th-9", "question": "May I drop the column?",
	}))
	if d.Open == nil {
		t.Fatal("a blocked thread should open an item")
	}
	if d.Open.TargetKind != "thread" || d.Open.TargetID != "th-9" {
		t.Errorf("the click must lead to the thread; got %s/%s", d.Open.TargetKind, d.Open.TargetID)
	}
	if d.Open.DemandID != "dem-1" {
		t.Error("the item needs the demand so the box can group by it")
	}
}

// Risk R-1 in test form: progress is not attention.
func TestAdvancingStageDoesNotFillTheBox(t *testing.T) {
	d := attention.Apply(ev(attention.EvStageAdvanced, map[string]any{
		"stage_key": "implementation", "to": "running",
	}))
	if d.Open != nil {
		t.Fatal("an advancing stage is progress, and progress is cockpit material — not box material")
	}
}

func TestStageBlockedAtAHumanGateOpensAnItem(t *testing.T) {
	d := attention.Apply(ev(attention.EvStageAdvanced, map[string]any{
		"stage_key": "spec", "to": "blocked", "gate": "human",
	}))
	if d.Open == nil || d.Open.Kind != attention.KindGatePending {
		t.Fatal("a stage stopped at a human gate requires a decision — it must open an item")
	}
}

func TestStageBlockedWithoutAGateOpensNothing(t *testing.T) {
	d := attention.Apply(ev(attention.EvStageAdvanced, map[string]any{
		"stage_key": "test", "to": "blocked", "gate": "none",
	}))
	if d.Open != nil {
		t.Fatal("a block with no gate is not a pending human decision")
	}
}

// Every opened item must carry the translation key. Without it the cockpit has
// only the English fallback to show, and the box stops being localizable — the
// failure is invisible until someone reads the screen in another language.
func TestEveryOpenedItemCarriesATranslationKey(t *testing.T) {
	cases := []attention.Event{
		ev(attention.EvThreadBlocked, map[string]any{"thread_id": "th-1"}),
		ev(attention.EvStageAdvanced, map[string]any{"stage_key": "spec", "to": "blocked", "gate": "human"}),
		ev(attention.EvPullRequestOpened, map[string]any{"pull_request_id": "pr-1"}),
		ev(attention.EvMergeConflict, map[string]any{"pull_request_id": "pr-1"}),
		ev(attention.EvDirectiveProposed, map[string]any{"directive_id": "dir-1"}),
		ev(attention.EvBudgetExceeded, map[string]any{"demand_id": "dem-1"}),
	}
	for _, e := range cases {
		d := attention.Apply(e)
		if d.Open == nil {
			t.Fatalf("%s should have opened an item", e.Type)
		}
		if d.Open.TitleKey == "" {
			t.Errorf("%s opened an item with no TitleKey — it can only ever be shown in English", e.Type)
		}
	}
}

func TestResumedThreadClosesTheItem(t *testing.T) {
	d := attention.Apply(ev(attention.EvThreadResumed, map[string]any{"thread_id": "th-9"}))
	if d.Close == nil {
		t.Fatal("a resumed thread must close the item")
	}
	if d.Close.TargetID != "th-9" || d.Close.Kind != attention.KindThreadBlocked {
		t.Error("closing is by TARGET: whoever unblocks does not know the item id")
	}
}

// A state change that resolves no conflict must not close the item — otherwise
// the box would lie and claim it is resolved.
func TestIntermediateStateDoesNotCloseAConflict(t *testing.T) {
	if d := attention.Apply(ev(attention.EvMergeState, map[string]any{
		"state": "rebasing", "pull_request_id": "pr-1",
	})); d.Close != nil {
		t.Fatal("rebasing resolved no conflict at all")
	}
	if d := attention.Apply(ev(attention.EvMergeState, map[string]any{
		"state": "merged", "pull_request_id": "pr-1",
	})); d.Close == nil {
		t.Fatal("a completed merge resolves the conflict")
	}
}

func TestEventWithNoTargetDoesNotBecomeAnItem(t *testing.T) {
	d := attention.Apply(ev(attention.EvThreadBlocked, map[string]any{}))
	if d.Open != nil {
		t.Fatal("an item with no target cannot be clicked — worse than a missing item")
	}
}

func TestIrrelevantEventIsIgnored(t *testing.T) {
	d := attention.Apply(ev("dop.hierarchy.workspace.created", map[string]any{}))
	if d.Open != nil || d.Close != nil {
		t.Fatal("a created workspace requires nobody's decision")
	}
}

// The subscribed subjects must cover EVERY event the rule knows how to
// translate — otherwise the item simply never arrives, and nobody notices.
func TestSubjectsCoverEveryHandledEvent(t *testing.T) {
	handled := []string{
		attention.EvThreadBlocked, attention.EvStageAdvanced,
		attention.EvPullRequestOpened, attention.EvMergeConflict,
		attention.EvDirectiveProposed, attention.EvBudgetExceeded,
		attention.EvThreadResumed, attention.EvThreadConcluded,
		attention.EvGateDecided, attention.EvDirectiveDecide,
		attention.EvMergeState, attention.EvBudgetSet,
	}
	for _, kind := range handled {
		if !coveredBySomeSubject(kind, attention.Subjects()) {
			t.Errorf("event %q is handled by the rule but no subscribed subject carries it", kind)
		}
	}
}

// Matches the NATS semantics the port documents: ">" matches the tail.
func coveredBySomeSubject(kind string, subjects []string) bool {
	for _, s := range subjects {
		if len(s) > 2 && s[len(s)-2:] == ".>" && len(kind) >= len(s)-1 &&
			kind[:len(s)-1] == s[:len(s)-1] {
			return true
		}
		if s == kind {
			return true
		}
	}
	return false
}

var _ = time.Now
