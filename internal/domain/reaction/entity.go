// Package reaction turns "what should happen when this event arrives" into
// data (P-29).
//
// Until this package existed, every consumer decided in Go: `attention.Apply`
// was a `switch`, and each new reaction meant another one. The cost was not the
// switch itself — it was that a policy written in code cannot be audited by the
// person it affects, cannot be changed without a deploy, and cannot differ
// between two accounts.
//
// House rule, as everywhere in internal/domain: this package knows nothing of
// Postgres, NATS or a mailer. It declares what it needs as a port and the
// composition root wires it.
package reaction

import (
	"strings"

	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ActionName is what a rule asks for. The vocabulary is CLOSED because an
// action is code: there is a Go handler behind each name, and a name nobody
// implemented is a contract error rather than something a user typed. It is the
// same stance ADR-0014 §1 takes on stage types.
type ActionName string

const (
	ActionOpenAttention  ActionName = "open_attention"
	ActionCloseAttention ActionName = "close_attention"
	ActionSendEmail      ActionName = "send_email"
	ActionProvisionBench ActionName = "provision_bench"
)

func ValidActionName(n ActionName) bool {
	switch n {
	case ActionOpenAttention, ActionCloseAttention, ActionSendEmail, ActionProvisionBench:
		return true
	}
	return false
}

// Action is one thing to do, with its parameters copied from the event's
// payload BY NAME.
//
// Params is a flat map of strings on purpose. The moment it holds an
// expression, the table stops being data and a loader stops being enough — and
// that is precisely what P-29 exists to prevent.
type Action struct {
	Name   ActionName
	Params map[string]string
}

// TriggerKind is what starts a rule.
//
// TriggerSchedule exists because the notification table already has one row
// that is not event-driven: the attention digest fires on a DELAY after an item
// opens. This package builds the event path; the scheduled path keeps whatever
// drives it today until somebody decides to move it.
type TriggerKind string

const (
	TriggerEvent    TriggerKind = "event"
	TriggerSchedule TriggerKind = "schedule"
)

// Rule is one row of the policy.
type Rule struct {
	ID         string
	AccountID  string // empty at the platform level, which has no owner
	OwnerScope string // platform | account | workspace | project — never demand
	OwnerID    string
	Trigger    TriggerKind
	EventType  string            // with TriggerEvent
	When       map[string]string // every entry must match the payload
	Actions    []Action
	Disables   []string // ids of inherited rules this one switches off
	Enabled    bool
	Why        string
	CreatedBy  string
}

// validTriggers restates the reaction_trigger enum the column carries. Without
// it an empty or misspelt trigger is refused only by the cast in Postgres, as a
// database error naming a type nobody writing a policy has heard of.
var validTriggers = map[TriggerKind]bool{TriggerEvent: true, TriggerSchedule: true}

// validScopes excludes `demand` deliberately: a demand's reactions come from
// the flow version it froze, not from this table. Two places deciding for one
// demand is two places to look when it does the wrong thing.
var validScopes = map[string]bool{"platform": true, "account": true, "workspace": true, "project": true}

func (r Rule) Validate() error {
	if !validScopes[r.OwnerScope] {
		return errs.Invalid("rule at an unusable level: %q — a demand's reactions come from its flow", r.OwnerScope)
	}
	if !validTriggers[r.Trigger] {
		return errs.Invalid("rule with an unusable trigger %q: a rule fires on an event or on a schedule", r.Trigger)
	}
	if r.Trigger == TriggerEvent && strings.TrimSpace(r.EventType) == "" {
		return errs.Invalid("an event rule with no event type reacts to nothing")
	}
	if len(r.Actions) == 0 && len(r.Disables) == 0 {
		return errs.Invalid("a rule that neither acts nor disables does nothing")
	}
	// WHY the names must be unique, so nobody "helpfully" relaxes it: the
	// idempotency gate's primary key is (event_id, rule_ref, action_name), and
	// a rule's rule_ref is its id. Two actions sharing a name in one rule
	// therefore claim the SAME row — the first runs, the second is skipped as
	// already applied, forever, with no error anywhere. The alternative was an
	// ordinal in the key; it was rejected because it makes idempotency depend
	// on ORDER, so inserting an action in the middle of the list would shift
	// every later ordinal and make already-applied actions look unapplied,
	// and a redelivery would re-run them.
	seen := make(map[ActionName]bool, len(r.Actions))
	for i, a := range r.Actions {
		if !ValidActionName(a.Name) {
			return errs.Invalid("action #%d has an unknown name %q: the vocabulary is the platform's", i+1, a.Name)
		}
		if seen[a.Name] {
			return errs.Invalid(
				"action %q appears twice in this rule: what marks an action as done is "+
					"(event, rule, action name), so the second one would be skipped as "+
					"already applied and would never run. To do %q twice — to reach two "+
					"recipients, say — write two rules, or one action whose params name both",
				a.Name, a.Name)
		}
		seen[a.Name] = true
	}
	if strings.TrimSpace(r.Why) == "" {
		return errs.Invalid(
			"a rule with no stated reason is not auditable: say WHY this rule " +
				"exists, not what it does — nobody can disagree with \"invite.created " +
				"sends an email\", and anybody can disagree with the reason")
	}
	return nil
}
