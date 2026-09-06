package reaction_test

import (
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/reaction"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
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
	// something a user typed — the same stance ADR-0014 takes on stage types.
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
