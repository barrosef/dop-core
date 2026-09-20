package attention

import "time"

// This file is the map between the LOG and the BOX: which events open an item,
// which close one, and what each item says.
//
// It is the frontier of risk R-1 in the spec ("a noisy box becomes noise and is
// ignored"). Every line here is a decision that this REQUIRES a human decision;
// anything not listed is, by definition, cockpit material and not box material.
// Adding a line is too easy — the question before adding one is "does the dev
// need to DECIDE something, or only to know?".

// Event types that OPEN an item.
const (
	EvThreadBlocked     = "dop.demand.thread.blocked"
	EvStageAdvanced     = "dop.demand.stage.advanced"
	EvPullRequestOpened = "dop.delivery.pull_request.opened"
	EvMergeConflict     = "dop.delivery.merge.conflict_escalated"
	EvDirectiveProposed = "dop.delivery.directive.proposed"
	EvBudgetExceeded    = "dop.cost.budget.exceeded"
	EvPhoneAdded        = "dop.identity.user.phone_added"
)

// Event types that CLOSE an item.
const (
	EvThreadResumed   = "dop.demand.thread.resumed"
	EvThreadConcluded = "dop.demand.thread.concluded"
	EvGateDecided     = "dop.demand.gate.decided"
	EvDirectiveDecide = "dop.delivery.directive.decided"
	EvMergeState      = "dop.delivery.merge.state_changed"
	EvBudgetSet       = "dop.cost.budget.set"
	EvPhoneVerified   = "dop.identity.user.phone_verified"
)

// Translation keys for what the box SHOWS. They live next to the rule that
// produces them, so that adding a rule without a key is visibly incomplete.
const (
	KeyThreadBlocked  = "attention.thread_blocked.title"
	KeyGatePending    = "attention.gate_pending.title"
	KeyPRReview       = "attention.pr_review.title"
	KeyMergeConflict  = "attention.merge_conflict.title"
	KeyDirective      = "attention.directive.title"
	KeyBudgetExceeded = "attention.budget_exceeded.title"
	KeyContactPhone   = "attention.contact_phone_unverified.title"
)

// Subjects is what the consumer subscribes to. Subscribing to `dop.>` and
// discarding 90% would waste deliveries; subscribing too broadly is also noise,
// just network noise.
func Subjects() []string {
	return []string{"dop.demand.>", "dop.delivery.>", "dop.cost.>", "dop.identity.>"}
}

// Event is the minimum the rule needs to know about an event. It exists so this
// package imports neither the adapter nor the bus envelope.
type Event struct {
	ID          string
	AccountID   string
	Aggregate   string
	AggregateID string
	Type        string
	OccurredAt  time.Time
	Payload     map[string]any
}

// Decision is what the rule returns: open an item, close a target's items, or
// ignore the event.
type Decision struct {
	Open  *Item
	Close *CloseSpec
}

// CloseSpec closes by TARGET, not by item id: whoever unblocks the thread does
// not know (and should not know) which item the box created for it.
type CloseSpec struct {
	Kind       Kind
	TargetKind string
	TargetID   string
}

// Apply translates an event into a decision. It returns the zero value when the
// event does not concern the box — which is the case for the overwhelming
// majority of them.
func Apply(e Event) Decision {
	switch e.Type {

	case EvThreadBlocked:
		return open(e, KindThreadBlocked, "thread", threadID(e),
			KeyThreadBlocked, map[string]any{"question": str(e.Payload, "question", "")},
			"An agent needs an answer",
			str(e.Payload, "detail", ""))

	case EvStageAdvanced:
		// It only counts when the stage STOPPED at a human gate. A stage moving
		// forward is progress, and progress is cockpit material — if every
		// transition became an item, the box would fill with things nobody has
		// to decide.
		//
		// The field is `to`, the state the stage moved TO. The first version
		// read `status`, which the event never had: the box could only CLOSE an
		// item that never opened, and the unit test did not catch it because it
		// fabricated the event in the assumed shape instead of the emitted one.
		// It is the integration test at the end of this domain that closes that
		// gap.
		if str(e.Payload, "to", "") != "blocked" {
			return Decision{}
		}
		if str(e.Payload, "gate", "") == "none" {
			return Decision{}
		}
		stage := str(e.Payload, "stage_key", "")
		return open(e, KindGatePending, "stage", stage,
			KeyGatePending, map[string]any{"stage": stage},
			"Stage awaiting a decision: "+stage,
			str(e.Payload, "reason", ""))

	case EvPullRequestOpened:
		return open(e, KindPRReview, "pull_request", str(e.Payload, "pull_request_id", e.AggregateID),
			KeyPRReview, map[string]any{"title": str(e.Payload, "title", "")},
			"Pull request awaiting review", str(e.Payload, "title", ""))

	case EvMergeConflict:
		return open(e, KindMergeConflict, "pull_request", str(e.Payload, "pull_request_id", e.AggregateID),
			KeyMergeConflict, map[string]any{"detail": str(e.Payload, "detail", "")},
			"Conflict escalated in the merge queue", str(e.Payload, "detail", ""))

	case EvDirectiveProposed:
		return open(e, KindDirective, "directive", str(e.Payload, "directive_id", e.AggregateID),
			KeyDirective, map[string]any{"recommendation": str(e.Payload, "recommendation", "")},
			"Cross-cutting concern detected — coordination decision",
			str(e.Payload, "recommendation", ""))

	case EvBudgetExceeded:
		return open(e, KindBudgetExceeded, "demand", demandID(e),
			KeyBudgetExceeded, map[string]any{"scope": str(e.Payload, "scope", "")},
			"Budget exceeded — demand paused",
			str(e.Payload, "scope", ""))

	case EvPhoneAdded:
		// The one item that is about the person and not about a demand: the
		// journey recorded a phone that was never confirmed (D-8). It targets
		// the user, so the cockpit leads to the contact screen.
		return open(e, KindContactPhoneUnverified, "user", e.AggregateID,
			KeyContactPhone, map[string]any{"phone": str(e.Payload, "phone_masked", "")},
			"Confirm your phone number", str(e.Payload, "phone_masked", ""))

	// ── closing ─────────────────────────────────────────────────────────────

	case EvPhoneVerified:
		return closeFor(KindContactPhoneUnverified, "user", e.AggregateID)

	case EvThreadResumed, EvThreadConcluded:
		return closeFor(KindThreadBlocked, "thread", threadID(e))

	case EvGateDecided:
		return closeFor(KindGatePending, "stage", str(e.Payload, "stage_key", ""))

	case EvDirectiveDecide:
		return closeFor(KindDirective, "directive", str(e.Payload, "directive_id", e.AggregateID))

	case EvMergeState:
		// It only closes once the conflict has ceased to exist. A state change
		// to "rebasing" resolves no conflict at all.
		if s := str(e.Payload, "state", ""); s != "merged" && s != "cancelled" {
			return Decision{}
		}
		return closeFor(KindMergeConflict, "pull_request",
			str(e.Payload, "pull_request_id", e.AggregateID))

	case EvBudgetSet:
		// A new ceiling may have unblocked the demand. Closing here and letting
		// the next overrun reopen is more honest than keeping an item for a
		// block that may no longer exist.
		return closeFor(KindBudgetExceeded, "demand", demandID(e))
	}

	return Decision{}
}

func open(e Event, k Kind, targetKind, targetID, titleKey string, params map[string]any, title, summary string) Decision {
	if targetID == "" {
		// An item with no target is an item you cannot click — and an item that
		// leads nowhere is worse than a missing item.
		return Decision{}
	}
	return Decision{Open: &Item{
		AccountID:  e.AccountID,
		Kind:       k,
		TargetKind: targetKind,
		TargetID:   targetID,
		DemandID:   demandID(e),
		TitleKey:   titleKey,
		Params:     params,
		Title:      title,
		Summary:    summary,
		OpenedAt:   e.OccurredAt,
		EventID:    e.ID,
	}}
}

func closeFor(k Kind, targetKind, targetID string) Decision {
	if targetID == "" {
		return Decision{}
	}
	return Decision{Close: &CloseSpec{Kind: k, TargetKind: targetKind, TargetID: targetID}}
}

// demandID: the demand's events carry the "demand" aggregate; delivery and cost
// events carry the id in the payload.
func demandID(e Event) string {
	if e.Aggregate == "demand" {
		return e.AggregateID
	}
	return str(e.Payload, "demand_id", "")
}

func threadID(e Event) string { return str(e.Payload, "thread_id", "") }

func str(p map[string]any, key, fallback string) string {
	if v, ok := p[key].(string); ok && v != "" {
		return v
	}
	return fallback
}
