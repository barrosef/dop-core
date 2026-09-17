package reaction_test

import (
	"strings"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/reaction"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

func good() reaction.Rule {
	return reaction.Rule{
		AccountID: "acct-1", OwnerScope: "account", OwnerID: "acct-1",
		Trigger: reaction.TriggerEvent, EventType: "dop.identity.invite.created",
		Actions: []reaction.Action{{
			Name:   reaction.ActionSendEmail,
			Params: map[string]string{"to_source": "payload", "to_field": "email"},
		}},
		Enabled: true,
		Why:     "the invitee is not a user yet: they have no cockpit to look at",
	}
}

func TestTheActionVocabularyIsClosed(t *testing.T) {
	for _, n := range []reaction.ActionName{
		reaction.ActionOpenAttention, reaction.ActionCloseAttention,
		reaction.ActionSendEmail, reaction.ActionProvisionBench,
	} {
		if !reaction.ValidActionName(n) {
			t.Fatalf("%q has to be accepted", n)
		}
	}
	// An action is CODE. A name outside the vocabulary is a contract error, not
	// something a user typed — the same stance ADR-0010 takes on stage types.
	for _, n := range []reaction.ActionName{"", "send_sms", "run_script", "OPEN_ATTENTION"} {
		if reaction.ValidActionName(n) {
			t.Fatalf("%q must not be accepted", n)
		}
	}
}

func TestARuleThatCouldNotBeAppliedIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*reaction.Rule){
		"no event type on an event trigger": func(r *reaction.Rule) { r.EventType = "" },
		"no actions and no disables":        func(r *reaction.Rule) { r.Actions = nil },
		"an action outside the vocabulary":  func(r *reaction.Rule) { r.Actions[0].Name = "send_sms" },
		"no owner":                          func(r *reaction.Rule) { r.OwnerScope = "" },
		"the demand scope":                  func(r *reaction.Rule) { r.OwnerScope = "demand" },
		"no reason":                         func(r *reaction.Rule) { r.Why = "" },
		// An empty or misspelt trigger used to reach Postgres and fail at the
		// cast to the reaction_trigger enum — a database error about a type
		// nobody writing a policy has heard of.
		"no trigger at all":     func(r *reaction.Rule) { r.Trigger = "" },
		"a trigger that is not": func(r *reaction.Rule) { r.Trigger = "webhook" },
	} {
		t.Run(name, func(t *testing.T) {
			r := good()
			mutate(&r)
			if err := r.Validate(); errs.KindOf(err) != errs.KindInvalid {
				t.Fatalf("expected a refusal, got %v", err)
			}
		})
	}
}

func TestARuleThatOnlyDisablesIsLegitimate(t *testing.T) {
	// Switching off an inherited rule is an act of its own. A rule that carries
	// no action but disables one is how an account says "not this one" without
	// having to invent a replacement.
	r := good()
	r.Actions = nil
	r.Disables = []string{"rule-from-the-platform"}
	if err := r.Validate(); err != nil {
		t.Fatalf("a rule that only disables is valid: %v", err)
	}
}

func TestTheReasonIsRequiredAndIsNotARestatement(t *testing.T) {
	// The notification table already carries `Why` for this exact purpose:
	// nobody can disagree with "invite.created -> email", and anybody can
	// disagree with the reason. A policy nobody can argue with is not auditable.
	r := good()
	r.Why = "sends an email when an invite is created"
	if err := r.Validate(); err != nil {
		t.Fatalf("this is a weak reason but not an invalid one: %v", err)
	}
	if !strings.Contains(good().Why, "cockpit") {
		t.Fatal("the fixture's reason should say WHY, so the test above means something")
	}
}

// TestTwoActionsWithTheSameNameInOneRuleAreRefused pins the limit the
// idempotency gate imposes on a rule. applied_actions is keyed on
// (event_id, rule_ref, action_name), and a rule's rule_ref is its id — so two
// actions sharing a name in one rule claim the SAME row: the first runs, the
// second is skipped forever as already applied. "E-mail the owner and e-mail
// the manager" would e-mail one of them, with no error anywhere.
func TestTwoActionsWithTheSameNameInOneRuleAreRefused(t *testing.T) {
	r := good()
	r.Actions = []reaction.Action{
		{Name: reaction.ActionSendEmail, Params: map[string]string{"to_field": "owner_email"}},
		{Name: reaction.ActionSendEmail, Params: map[string]string{"to_field": "manager_email"}},
	}
	err := r.Validate()
	if errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("the second action would be skipped forever; expected a refusal, got %v", err)
	}
	// The message has to say the way OUT, not only that the author was refused.
	if !strings.Contains(err.Error(), "two rules") {
		t.Errorf("the refusal has to say how to reach two recipients: %v", err)
	}
}

// Different names in one rule are the ordinary case and must stay writable:
// they claim different rows, so the gate never confuses them.
func TestTwoDifferentActionsInOneRuleAreLegitimate(t *testing.T) {
	r := good()
	r.Actions = []reaction.Action{
		{Name: reaction.ActionSendEmail},
		{Name: reaction.ActionOpenAttention},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("two different actions do not collide in the gate: %v", err)
	}
}
