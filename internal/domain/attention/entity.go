// Package attention is the attention box: the single queue that answers "where
// am I needed, and in what order".
//
// It is a PROJECTION (ADR-0004): every item is born of an event and dies of
// another. This package does not create items — it translates events into items
// and ORDERS them. The translation lives here, and not in the adapter, because
// deciding what deserves human attention is a business rule, not a storage
// detail.
package attention

import (
	"time"
)

// Kind is the item's nature. It is what determines the impact on ordering.
type Kind string

const (
	KindThreadBlocked     Kind = "thread_blocked"
	KindGatePending       Kind = "gate_pending"
	KindPRReview          Kind = "pr_review"
	KindMergeConflict     Kind = "merge_conflict"
	KindDirective         Kind = "directive"
	KindBudgetExceeded    Kind = "budget_exceeded"
	KindIntegrationBroken Kind = "integration_broken"
	// A reminder about the person, not about work: the contact phone given in
	// the onboarding journey and never confirmed (spec 2026-09-20 D-8).
	KindContactPhoneUnverified Kind = "contact_phone_unverified"
)

// Item is a pending matter that requires a HUMAN DECISION.
//
// What does not require a decision does not get in: status and progress stay in
// the cockpit. A noisy box becomes noise and is ignored, and an ignored box
// protects nobody (risk R-1 of the conversation-and-attention spec).
type Item struct {
	ID         string
	AccountID  string
	Kind       Kind
	TargetKind string // thread | stage | pull_request | directive | demand | resource
	TargetID   string
	DemandID   string // empty on an account-level item (broken integration)

	// TitleKey and Params are what the cockpit TRANSLATES. The key is stable
	// ("attention.gate_pending.title") and Params carries the values the
	// sentence needs.
	//
	// They exist because this text is read by a person, and text read by a
	// person cannot be frozen in one language inside the domain. The domain
	// knows WHICH sentence applies; it does not get to choose the words.
	TitleKey string
	Params   map[string]any

	// Title and Summary are the ENGLISH FALLBACK, for a reader that has no
	// catalogue: logs, the API's raw response, an operator reading the table.
	// They are never the translation — a client that shows them to a user is
	// showing developer text, and the presence of TitleKey is how it knows
	// better.
	Title   string
	Summary string

	OpenedAt   time.Time
	ResolvedAt *time.Time
	// EventID is the event that OPENED the item. Keeping it is what makes the
	// projection rebuildable and redelivery harmless.
	EventID string
}

func (i Item) Open() bool { return i.ResolvedAt == nil }

// impact is the urgency table by kind, and its ordering IS the rule.
//
// One place only, like the model router: spreading this across ifs would let
// each domain decide its own urgency, and the queue would stop having a single
// order — which is exactly what the box exists to provide.
//
// The yardstick comes from the spec: "priority by impact (a production merge
// queue > an exploratory question) and age". Translated: what blocks DELIVERY
// comes before what blocks ONE demand, which comes before what blocks ONE
// conversation.
var impact = map[Kind]int{
	// Blocks delivery for everyone: the merge queue is per repository, and an
	// escalated conflict holds up everything behind it.
	KindMergeConflict: 10,
	// Blocks the whole account: with no integration, no demand moves.
	KindIntegrationBroken: 20,
	// Blocks ONE demand entirely.
	KindBudgetExceeded: 30,
	KindGatePending:    40,
	// Finished work waiting on a person — it costs money sitting still, but it
	// does not block whoever is already moving.
	KindPRReview: 50,
	// Coordination: important and never urgent. Demand 1 goes as far as it can;
	// it does NOT stop because a cross-cutting concern was identified
	// (ADR-0011).
	KindDirective: 60,
	// A conversation waiting on an answer.
	KindThreadBlocked: 70,
	// A reminder never outranks work: it sits below everything a demand can
	// raise, and age alone lifts it within its band.
	KindContactPhoneUnverified: 80,
}

// ImpactOf returns the kind's impact. An unknown kind lands at the END of the
// queue, not the start: an item nobody can classify must not push a merge
// conflict down just for being new.
func ImpactOf(k Kind) int {
	if v, ok := impact[k]; ok {
		return v
	}
	return 999
}

// Priority combines impact and age into a single number, smallest first.
//
// Age only breaks ties WITHIN the same impact — it never crosses bands. If it
// did, a three-day-old exploratory question would jump ahead of a three-minute
// production conflict, which is precisely the inversion the spec forbids.
func (i Item) Priority(now time.Time) int32 {
	hours := int(now.Sub(i.OpenedAt).Hours())
	if hours < 0 {
		hours = 0 // a clock running backwards must not become top priority
	}
	// A one-week ceiling: past that, age stops differentiating, otherwise an
	// item forgotten months ago would dominate its band forever.
	if hours > 24*7 {
		hours = 24 * 7
	}
	// Impact × 1000 reserves the age digits without letting them invade the
	// band above.
	return int32(ImpactOf(i.Kind)*1000 + (24*7 - hours))
}

// Kinds ordered by impact — it exists so the test can prove the table covers
// every kind, and so a calibration screen can show the yardstick.
func Kinds() []Kind {
	return []Kind{
		KindMergeConflict, KindIntegrationBroken, KindBudgetExceeded,
		KindGatePending, KindPRReview, KindDirective, KindThreadBlocked,
		KindContactPhoneUnverified,
	}
}
