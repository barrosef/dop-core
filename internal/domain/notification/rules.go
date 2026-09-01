package notification

import (
	"strings"
	"time"
)

// ════════════════════════════════════════════════════════════════════════════
// THE TABLE. The table is the decider.
//
// Same shape as `attention.impact` and `cost.routingTable`, and for the same
// reason: a policy has to be AUDITABLE and REPLACEABLE in one move. Fifteen
// `if`s scattered across callers give the same answer today and are impossible
// to recalibrate tomorrow — and nobody can explain why an email went out.
//
// The requirement is stronger here than in its two cousins: under P-29 this
// table stops being code and becomes DATA. That is why every row is fillable by
// a dumb loader — there is no function, no closure and no `switch` inside any
// row. What looks like rigidity (copying a payload field by NAME, building the
// link by PATH) is what makes the swap a loader instead of a rewrite.
//
// BEFORE ADDING A ROW, the question is the attention box's, one notch up: "does
// this deserve to INTERRUPT the person outside the platform?". The box already
// filters what requires a decision; email filters what cannot wait for the
// person to come back to the cockpit.
// ════════════════════════════════════════════════════════════════════════════

// Event types the table knows. A named constant, not a literal in the row, so
// the compiler helps when an event is renamed.
const (
	EvInviteCreated = "dop.identity.invite.created"
)

// recipientSource is WHERE the recipient comes from. Two origins because they
// are two worlds: someone who is already a platform user (the account knows the
// address) and someone who is not yet (the address only exists in the event).
type recipientSource string

const (
	// fromPayload: the address comes from a field of the event's payload. That
	// is the invite's case, and it is the only possible way — the invitee IS NOT
	// YET A USER, and there is nowhere for the platform to resolve their address
	// from.
	fromPayload recipientSource = "payload"
	// fromAccountMembers: the address comes from whoever has a membership in the account.
	fromAccountMembers recipientSource = "account_members"
)

// recipientSpec is declarative on purpose: `Field` is a field NAME, not an
// extraction function. See the header.
type recipientSpec struct {
	Source recipientSource
	Field  string // only used with fromPayload
}

// Rule is a ROW of the table.
type Rule struct {
	// Name is the rule's ADDRESSABLE name, and it goes into the idempotency key.
	// Renaming a rule in production reopens everything it has already sent — the
	// name is identity, not a label.
	Name    string
	Trigger Trigger
	// Event only applies with TriggerEvent.
	Event  string
	Action Action
	Kind   Kind

	Recipients recipientSpec
	// Data are the payload keys copied into the template's data, by NAME. There
	// is no transformation: what the adapter receives is what the event brought.
	// Transforming here would bring presentation vocabulary inside the policy.
	Data []string
	// LinkPath is the cockpit path the notice leads to, relative. Relative
	// because the base belongs to the INSTALLATION (Config.BaseURL), not to the
	// policy — the same rule holds in the SaaS and in a self-hosted deployment on
	// another domain.
	//
	// It may contain `{field}`, replaced by the value of the SAME field in Data.
	// It is how the invite addresses its own row instead of dumping the person
	// into a list: `/invites/{invite_id}`. It stays data — the row declares the
	// format, nobody writes concatenation in Go.
	//
	// Every `{field}` MUST be in Data; a table test refuses one that is not,
	// because the price of getting it wrong is an email with "{invite_id}"
	// sitting in the middle of the URL.
	LinkPath string
	// Delay only applies with TriggerAttentionBox. Zero uses DefaultDigestDelay.
	Delay time.Duration
	// Why is the ROW's reason, not a restatement of what it does. Without it the
	// policy is not auditable: nobody can disagree with "invite.created → email",
	// and anybody can disagree with the reason.
	Why string
}

// table — THE TABLE. A slice and not a map: the order is the reading order, and
// two rules for the same event (which P-29 will allow) need a defined order.
var table = []Rule{
	{
		Name:       "invite-created",
		Trigger:    TriggerEvent,
		Event:      EvInviteCreated,
		Action:     ActionEmail,
		Kind:       KindInvite,
		Recipients: recipientSpec{Source: fromPayload, Field: "email"},
		Data:       []string{"invite_id", "email", "role"},
		LinkPath:   "/invites/{invite_id}",
		Why: "it is the only notification whose recipient IS NOT YET A USER: they " +
			"have no cockpit to look at, no attention box, and the invite does not " +
			"exist for them until it arrives from outside. Without this email the " +
			"invite is a record nobody sees",
	},
	{
		Name:       "attention-digest",
		Trigger:    TriggerAttentionBox,
		Action:     ActionEmail,
		Kind:       KindAttentionDigest,
		Recipients: recipientSpec{Source: fromAccountMembers},
		LinkPath:   "/attention",
		Delay:      DefaultDigestDelay,
		Why: "the notice hangs off the BOX and not off raw events — the box already " +
			"decides what requires a human decision, and a second map would start " +
			"identical and diverge on the first adjustment. The delay exists because " +
			"one email per item makes the inbox useless, and an ignored box protects " +
			"nobody (risk R-1 of the spec)",
	},
}

// Rules returns the policy. A copy, not the slice: a policy the caller can edit
// in memory stops being a policy.
//
// THIS is where P-29 lands. Today it returns the literal above; the day reaction
// becomes data, this function reads from the database and no caller changes —
// not `Apply`, not `Kinds`, not `Subjects`, not the contract suite.
func Rules() []Rule {
	out := make([]Rule, len(table))
	copy(out, table)
	return out
}

// Subjects is what the consumer subscribes to, DERIVED from the table.
//
// Derived, and not hand-written, because divergence between the two is silent in
// both directions: one subscription too few makes the event never arrive
// (nobody receives it and nothing fails), one too many wastes deliveries. It is
// the same reasoning as `attention.Subjects`, except that there the list is a
// literal — here it cannot be, because the table is going to become data.
func Subjects() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(table))
	for _, r := range table {
		if r.Trigger != TriggerEvent || r.Event == "" || seen[r.Event] {
			continue
		}
		seen[r.Event] = true
		out = append(out, r.Event)
	}
	return out
}

// DigestRule returns the attention notice's rule, if there is one.
//
// The sweep needs it by NAME, not by position: the stored idempotency key
// carries the rule's name, and a sweep that assumed "the table's second row"
// would write the wrong key the day somebody reordered the literal.
func DigestRule() (Rule, bool) {
	for _, r := range table {
		if r.Trigger == TriggerAttentionBox {
			return r, true
		}
	}
	return Rule{}, false
}

// Apply translates an event into commands.
//
// It returns a slice — not a single command — from day one: with declarative
// reaction one event will be able to trigger N actions, and a signature that
// returned one would force every caller to change along with the policy.
//
// It returns EMPTY for the overwhelming majority of events, which is the normal
// case.
func Apply(e Event, resolve func(recipientSpec, Event) []Recipient) []Command {
	if e.AccountID == "" || e.ID == "" {
		// An event with no account (`user.ensured`, migration 0003) belongs to no
		// notification: there are no members to warn and no account to warn on
		// behalf of.
		return nil
	}
	var out []Command
	for _, r := range table {
		if r.Trigger != TriggerEvent || r.Event != e.Type {
			continue
		}
		dest := resolve(r.Recipients, e)
		if len(dest) == 0 {
			// With no recipient there is nothing to fire. Not an error — see
			// Command.Valid.
			continue
		}
		out = append(out, Command{
			AccountID:  e.AccountID,
			EventID:    e.ID,
			Rule:       r.Name,
			Action:     r.Action,
			Kind:       r.Kind,
			Recipients: dest,
			Data:       dataFromEvent(r, e),
		})
	}
	return out
}

// dataFromEvent copies, by NAME, what the row asked for. A missing key becomes a
// missing field, never an error: the template decides what to do with the gap,
// and dropping an invite because a cosmetic field did not arrive would trade a
// cosmetic problem for an access blocker.
func dataFromEvent(r Rule, e Event) map[string]any {
	d := map[string]any{}
	for _, key := range r.Data {
		if v, ok := e.Payload[key]; ok {
			d[key] = v
		}
	}
	return d
}

// payloadEmail extracts the address from a payload field. It exists because the
// service (which knows the repository) resolves recipients, not the table.
func payloadEmail(spec recipientSpec, e Event) []Recipient {
	if spec.Source != fromPayload || spec.Field == "" {
		return nil
	}
	v, _ := e.Payload[spec.Field].(string)
	v = strings.TrimSpace(v)
	if !validAddress(v) {
		return nil
	}
	return []Recipient{{Email: v}}
}
