package reaction_test

import (
	"encoding/json"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/reaction"
)

func event(t *testing.T, typ string, payload map[string]any) ports.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return ports.Event{ID: "ev-1", AccountID: "acct-1", Type: typ, Payload: raw}
}

func ruleAt(scope, id string, when map[string]string, actions ...reaction.ActionName) reaction.Rule {
	r := reaction.Rule{
		ID: id, OwnerScope: scope, OwnerID: "o", Trigger: reaction.TriggerEvent,
		EventType: "dop.identity.invite.created", When: when, Enabled: true, Why: "because",
	}
	for _, n := range actions {
		r.Actions = append(r.Actions, reaction.Action{Name: n})
	}
	return r
}

func TestRulesAccumulateDownTheChain(t *testing.T) {
	// The whole point: an account adding ONE rule must not silence the
	// platform's. If the nearest level won, as it does for flows, the welcome
	// e-mail would vanish the first time an account wrote a rule of its own.
	got, err := reaction.Decide(
		event(t, "dop.identity.invite.created", map[string]any{"email": "a@b.c"}),
		[]reaction.Rule{
			ruleAt("platform", "r-platform", nil, reaction.ActionSendEmail),
			ruleAt("account", "r-account", nil, reaction.ActionOpenAttention),
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("both levels' rules apply, got %d", len(got))
	}
}

func TestALowerLevelDisablesAnInheritedRuleByID(t *testing.T) {
	// Turning something off has to be an ACT. Creating an unrelated rule must
	// never switch off an inherited one as a side effect.
	got, err := reaction.Decide(
		event(t, "dop.identity.invite.created", map[string]any{"email": "a@b.c"}),
		[]reaction.Rule{
			ruleAt("platform", "r-platform", nil, reaction.ActionSendEmail),
			func() reaction.Rule {
				r := ruleAt("account", "r-account", nil)
				r.Disables = []string{"r-platform"}
				return r
			}(),
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the platform's rule was disabled; nothing should be planned, got %v", got)
	}
}

func TestOnlyALOWERLevelCanDisable(t *testing.T) {
	// A rule cannot switch off one that is more specific than itself: the
	// platform must not be able to reach into an account's policy.
	got, err := reaction.Decide(
		event(t, "dop.identity.invite.created", map[string]any{"email": "a@b.c"}),
		[]reaction.Rule{
			func() reaction.Rule {
				r := ruleAt("platform", "r-platform", nil)
				r.Disables = []string{"r-account"}
				return r
			}(),
			ruleAt("account", "r-account", nil, reaction.ActionSendEmail),
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the platform must not disable an account's rule, got %d planned", len(got))
	}
}

func TestWhenMatchesEveryEntryOrTheRuleDoesNotApply(t *testing.T) {
	e := event(t, "dop.identity.invite.created", map[string]any{"email": "a@b.c", "role": "admin"})
	cases := map[string]struct {
		when  map[string]string
		plans int
	}{
		"no condition":              {nil, 1},
		"one entry that matches":    {map[string]string{"role": "admin"}, 1},
		"one entry that does not":   {map[string]string{"role": "viewer"}, 0},
		"both match":                {map[string]string{"role": "admin", "email": "a@b.c"}, 1},
		"one of two does not":       {map[string]string{"role": "admin", "email": "x@y.z"}, 0},
		"a field the payload lacks": {map[string]string{"absent": "x"}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := reaction.Decide(e, []reaction.Rule{
				ruleAt("account", "r", tc.when, reaction.ActionSendEmail)})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.plans {
				t.Fatalf("wanted %d planned, got %d", tc.plans, len(got))
			}
		})
	}
}

func TestARuleForAnotherEventOrDisabledDoesNotApply(t *testing.T) {
	e := event(t, "dop.identity.invite.created", map[string]any{})
	other := ruleAt("account", "r-other", nil, reaction.ActionSendEmail)
	other.EventType = "dop.demand.stage.advanced"
	off := ruleAt("account", "r-off", nil, reaction.ActionSendEmail)
	off.Enabled = false
	got, err := reaction.Decide(e, []reaction.Rule{other, off})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("neither rule applies, got %d", len(got))
	}
}

func TestThePlanCarriesWhatTheExecutorNeeds(t *testing.T) {
	e := event(t, "dop.identity.invite.created", map[string]any{"email": "a@b.c"})
	r := ruleAt("account", "r-1", nil, reaction.ActionSendEmail)
	r.Actions[0].Params = map[string]string{"to_source": "payload", "to_field": "email"}
	got, err := reaction.Decide(e, []reaction.Rule{r})
	if err != nil {
		t.Fatal(err)
	}
	// RuleRef is what the idempotency key is built from, and what a DLQ record
	// will carry so a retry re-executes what failed rather than what the rules
	// say later.
	if got[0].RuleRef != "r-1" || got[0].Name != reaction.ActionSendEmail {
		t.Fatalf("the plan does not identify its rule or action: %+v", got[0])
	}
	if got[0].Params["to_field"] != "email" || got[0].Event.ID != "ev-1" {
		t.Fatalf("the plan does not carry enough to execute: %+v", got[0])
	}
}

func TestAStageEventPlansTheExitOfWhereItLeftAndTheEntryOfWhereItArrived(t *testing.T) {
	e := event(t, "dop.demand.stage.advanced", map[string]any{
		"from": "implementation", "to": "test",
	})
	stages := map[string][]reaction.StageActionSpec{
		"implementation": {{On: "exit", Name: reaction.ActionProvisionBench}},
		"test":           {{On: "enter", Name: reaction.ActionOpenAttention}},
		"spec":           {{On: "enter", Name: reaction.ActionSendEmail}},
	}
	got, err := reaction.DecideStage(e, "flow-1", 3, stages)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the exit of implementation and the entry of test, got %d: %+v", len(got), got)
	}
	// RuleRef names the frozen flow, its version, the stage and the moment —
	// which is what makes the idempotency key unique and a DLQ record
	// re-executable without consulting the rules again.
	if got[0].RuleRef != "flow-1/3/implementation/exit" {
		t.Fatalf("the plan does not identify its origin: %q", got[0].RuleRef)
	}
}

func TestTheFirstStageHasNoExitBeforeIt(t *testing.T) {
	e := event(t, "dop.demand.stage.advanced", map[string]any{"from": "", "to": "context"})
	stages := map[string][]reaction.StageActionSpec{
		"context": {{On: "enter", Name: reaction.ActionSendEmail}},
	}
	got, err := reaction.DecideStage(e, "flow-1", 1, stages)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RuleRef != "flow-1/1/context/enter" {
		t.Fatalf("a demand starting has an entry and no exit: %+v", got)
	}
}

// TestThePlanDoesNotHandOutTheRulesOwnParams closes a domain boundary. A
// handler that writes to Params would otherwise be writing into the Rule the
// CALLER is still holding — the same map, aliased across the decision. The
// caller may well go on to decide with that rule for another event.
func TestThePlanDoesNotHandOutTheRulesOwnParams(t *testing.T) {
	r := ruleAt("account", "r-1", nil, reaction.ActionSendEmail)
	r.Actions[0].Params = map[string]string{"to_field": "email"}
	got, err := reaction.Decide(event(t, "dop.identity.invite.created", nil), []reaction.Rule{r})
	if err != nil {
		t.Fatal(err)
	}
	got[0].Params["to_field"] = "hijacked"
	if r.Actions[0].Params["to_field"] != "email" {
		t.Fatalf("the handler wrote through into the rule: %v", r.Actions[0].Params)
	}
}

// The same boundary on the in-demand side: the stages map belongs to whoever
// read the frozen flow version, and a handler must not be able to edit it.
func TestTheStagePlanDoesNotHandOutTheFlowsOwnParams(t *testing.T) {
	spec := reaction.StageActionSpec{On: "enter", Name: reaction.ActionOpenAttention,
		Params: map[string]string{"severity": "info"}}
	stages := map[string][]reaction.StageActionSpec{"test": {spec}}
	got, err := reaction.DecideStage(
		event(t, "dop.demand.stage.advanced", map[string]any{"from": "", "to": "test"}),
		"flow-1", 1, stages)
	if err != nil {
		t.Fatal(err)
	}
	got[0].Params["severity"] = "hijacked"
	if stages["test"][0].Params["severity"] != "info" {
		t.Fatalf("the handler wrote through into the frozen flow: %v", stages["test"][0].Params)
	}
}
