package reaction

import (
	"encoding/json"
	"fmt"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// PlannedAction is one action, decided, with everything needed to run it.
//
// It carries the event rather than a reference to it because a plan outlives
// the decision: a failed action is retried from a frozen plan, and re-deciding
// later would execute what the rules say THEN instead of what failed.
type PlannedAction struct {
	RuleRef string
	Name    ActionName
	Params  map[string]string
	Event   ports.Event
}

// Decide answers what should happen for one event.
//
// `rules` arrives ordered from the MOST GENERIC level to the most specific —
// the order Ancestry.ChainOf already returns — and that order is what makes
// `disables` directional: a rule can only switch off one that came before it,
// so an account can refuse the platform's rule and the platform cannot reach
// into the account's.
//
// Rules ACCUMULATE. Unlike a flow, where the nearest level wins and there is one
// effective document, every rule in the chain applies: if the nearest won, an
// account writing its first rule would silently switch off the platform's.
func Decide(e ports.Event, rules []Rule) ([]PlannedAction, error) {
	var payload map[string]any
	if len(e.Payload) > 0 {
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return nil, errs.Invalid("event %s carries an unreadable payload: %v", e.ID, err)
		}
	}

	// A rule's `disables` reaches only what came BEFORE it in the chain — which,
	// walking generic to specific, is everything more generic than itself. That
	// is the whole direction of the rule: an account may refuse the platform's
	// policy, and the platform may not reach into the account's. Collecting every
	// `disables` regardless of position would let the platform switch off an
	// account's rule, which is the opposite of what this exists for.
	disabled, seen := map[string]bool{}, map[string]bool{}
	for _, r := range rules {
		for _, id := range r.Disables {
			if seen[id] {
				disabled[id] = true
			}
		}
		seen[r.ID] = true
	}

	var planned []PlannedAction
	for _, r := range rules {
		if disabled[r.ID] || !r.Enabled {
			continue
		}
		if r.Trigger != TriggerEvent || r.EventType != e.Type {
			continue
		}
		if !matches(r.When, payload) {
			continue
		}
		for _, a := range r.Actions {
			planned = append(planned, PlannedAction{
				RuleRef: r.ID, Name: a.Name, Params: copyParams(a.Params), Event: e,
			})
		}
	}
	return planned, nil
}

// matches is equality and nothing else. A comparison, a range or a composite
// boolean would make this a DSL — which ADR-0014 §2 already refused for flows,
// for the same reason: the moment the row holds an expression, swapping the
// policy stops being a loader and becomes a rewrite.
func matches(when map[string]string, payload map[string]any) bool {
	for field, want := range when {
		got, ok := payload[field]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", got) != want {
			return false
		}
	}
	return true
}

// copyParams keeps the rule's own map out of the plan.
//
// A plan crosses a domain boundary: a handler receives it and there is nothing
// stopping the handler from writing to Params — filling in a resolved address,
// say. Handing over the source map means that write lands in the Rule the
// CALLER is still holding, and the caller may go on to decide with that same
// rule for the next event. One small map per planned action is the price of
// not having that aliasing be discovered in production.
func copyParams(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
