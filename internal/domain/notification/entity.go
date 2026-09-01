// Package notification is communication's TRIGGER (ADR-0025).
//
// In the product owner's words: *"a Mailer living without a Notifier would be
// like a bullet that could be fired without a trigger."* The point is not that
// the channel COULD live alone — it is that **the trigger exists either way**.
// If it is not designed, somebody improvises: the invite use case calls the
// `Mailer` directly and becomes the trigger, with no name and no home, spread
// across however many use cases send email. The risk is not "a channel with no
// trigger", it is a DIFFUSE TRIGGER.
//
//	event consumer
//	   └─ Notifier: decides WHAT to notify and TO WHOM   ← this package
//	        └─ command: (kind, recipient, data)
//	             └─ ports.Mailer                          ← the dispatch
//	                  └─ SendGrid | SMTP
//
// The invite use case knows neither of them: it publishes an event and is done.
//
// ── Why decider and executor are born separate ──────────────────────────────
//
// Because reaction to events becomes DATA under P-29: the product owner wants a
// structure mapping event → action(s), and the actions will not only be email.
// What is already agreed is that communication should be born in the shape that
// change will demand — decider separate from executor, action addressable by
// NAME, and an idempotency key derived from (event, rule, action), not from the
// event alone.
//
// That is why the decision is a TABLE (rules.go) and not a `switch`: the swap
// has to be of a LOADER, not a rewrite. `Apply` reads `Rules()`; the day the
// rules come from the database, `Rules()` starts reading them from there and
// nothing else changes.
package notification

import (
	"sort"
	"strings"
	"time"
)

// Kind is the notification's TYPE — the vocabulary the domain can emit and that
// every channel adapter has to be able to resolve.
//
// Not to be confused with `Action` (which way it leaves) or with a template (how
// it looks): the kind is the INTENT. "Somebody was invited" is the same intent
// in SendGrid, in SMTP and in a future OneSignal; what changes is the catalogue
// on the other side.
type Kind string

const (
	// KindInvite — transactional: fires immediately, always, one per event.
	KindInvite Kind = "invite"
	// KindAttentionDigest — the attention notice, delayed and grouped. It hangs
	// off the BOX (ADR-0006), not off raw events. See DefaultDigestDelay.
	KindAttentionDigest Kind = "attention_digest"
)

// Action is WHAT is done when the rule matches, addressable by NAME.
//
// Today only email exists and even so the action is a named value, not an
// implicit field: under P-29 actions stop being only notices ("fire other
// processes too"), and the idempotency key already carries it. Adding the action
// later, with keys already stored without it, would mean migrating live data.
type Action string

const (
	ActionEmail Action = "email"
)

// Trigger is WHERE the rule fires from, and the distinction comes from
// ADR-0025: transactional and attention notice are different things.
type Trigger string

const (
	// TriggerEvent — the trigger is an event from the spine (ADR-0019). An
	// invite, an account verification: fires immediately, always, one per event.
	TriggerEvent Trigger = "event"
	// TriggerAttentionBox — the trigger is the attention BOX, not the raw event.
	// Two maps diverge on the first adjustment: the box already decides what
	// requires a human decision, and a second map of "what deserves an email"
	// would start identical and end up different.
	TriggerAttentionBox Trigger = "attention_box"
)

// DefaultDigestDelay is the attention notice's delay.
//
// The item opens, WAITS, and only becomes an email if it is still open — whoever
// was in the cockpit has already handled it. Fifteen minutes is short enough
// that the urgent does not wait and long enough that the trivial resolves
// itself.
//
// It is an INFORMED GUESS, like the model router's table, and becomes a
// calibrated number once there is telemetry. That is why it is configurable: the
// right value for an on-call team is not the one for a team that checks the box
// in the morning.
const DefaultDigestDelay = 15 * time.Minute

// DefaultMaxAttempts bounds the retry of what FAILED.
//
// It exists because the opposite — retrying forever — turns an account with an
// invalid address into a traffic generator aimed at the provider, which is how
// sender reputation is lost and, with it, everybody's deliverability.
const DefaultMaxAttempts = 5

// Recipient is who the notice goes to. The name is optional; the address is not.
type Recipient struct {
	Email string
	Name  string
}

// Event is the minimum the rule needs to know about an event. It exists — like
// attention.Event, and for the same reason — so this package imports neither the
// adapter nor the bus envelope.
type Event struct {
	ID          string
	AccountID   string
	Aggregate   string
	AggregateID string
	Type        string
	OccurredAt  time.Time
	Payload     map[string]any
}

// Command is what the DECIDER returns and what the EXECUTOR consumes: (kind,
// recipient, data). It is the boundary between the two, and it is deliberately
// dumb — there is nothing here a loader of database-backed rules could not
// fill.
type Command struct {
	AccountID string
	// EventID, Rule and Action are the IDEMPOTENCY KEY, composite from day one
	// (ADR-0025). See Key.
	EventID string
	Rule    string
	Action  Action

	Kind       Kind
	Recipients []Recipient
	Data       map[string]any
}

// Key is the idempotency key, composite.
//
// Not `EventID` alone: with declarative reaction (P-29) one event will be able
// to fire N actions, and a key made only of the event would discard the second
// as a duplicate. Discarding by idempotency is silent by design — the second
// notice simply would not happen, with no error and no log.
func (c Command) Key() (eventID, rule string, action Action) {
	return c.EventID, c.Rule, c.Action
}

// Valid says whether the command has the minimum to be executed.
//
// A command with no recipient is NOT an error: it is the normal case of an
// account whose members have no known address yet. The error would be stopping
// the consumer over it — and a stopped consumer delays EVERY notification in the
// queue.
func (c Command) Valid() bool {
	return c.AccountID != "" && c.EventID != "" && c.Rule != "" &&
		c.Action != "" && c.Kind != "" && len(c.Recipients) > 0
}

// State is the state of the send RECORD, in the shape the sibling project
// proved useful in operation — "the invite never arrived" has to be an
// answerable question.
//
// The last three come from there; `pending` is the claim, and it exists because
// the claim happens BEFORE the send.
//
// There is no read struct (`Delivery`) in this package, and the absence is
// deliberate: the record has no query surface today (no RPC was invented for
// it), and a read type with no reader is a type that ages out of step with the
// table without anybody noticing.
type State string

const (
	// StatePending: claimed, still without an outcome. A row stuck here is a
	// visible anomaly — the process died between the send and the record — and it
	// is NOT retried on purpose: a duplicate email is visible to the user and has
	// no undo, whereas a lost notice stays recorded.
	StatePending State = "pending"
	// StateSent: the provider accepted it.
	StateSent State = "sent"
	// StateSentLocal: a REHEARSAL. There was no credential, it printed instead of
	// sending, and nobody received anything.
	StateSentLocal State = "sent_local"
	// StateError: it failed. The ONLY retryable state.
	StateError State = "error"
)

// AttentionNotice is a MATURE box item — open for longer than the delay and
// still un-notified.
//
// The package does not import `attention`: it would need only three strings, and
// importing a whole domain for three strings couples the two the day the box
// changes shape. The possible divergence is cosmetic (title and kind), not
// structural, and the mapping lives in the adapter that reads the table.
type AttentionNotice struct {
	AccountID string
	// EventID is the event that OPENED the item — it is what goes into the
	// idempotency key, and it is why the same item never becomes two notices.
	EventID  string
	ItemID   string
	Kind     string
	Title    string
	Summary  string
	DemandID string
	OpenedAt time.Time
}

// Kinds returns the types the domain can emit, DERIVED from the rules table —
// never a second hand-written list.
//
// This function is the whole contract of the `Mailer` port's guarantee 1: the
// suite requires every adapter to resolve everything it returns. If it were its
// own list, somebody would add a rule without adding the kind here, and the
// suite would stay green while the new email reached nobody — exactly the
// silence ADR-0025 orders us to test.
func Kinds() []Kind {
	seen := map[Kind]bool{}
	out := make([]Kind, 0, len(Rules()))
	for _, r := range Rules() {
		if r.Kind == "" || seen[r.Kind] {
			continue
		}
		seen[r.Kind] = true
		out = append(out, r.Kind)
	}
	// Stable order: the contract suite iterates over this, and a test whose order
	// changes between runs is a test nobody can debug.
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// KindNames is Kinds as strings, for whoever talks to the port (which does not
// know this package's named type — see ports.Mail.Kind).
func KindNames() []string {
	ks := Kinds()
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, string(k))
	}
	return out
}

// validAddress is the cheapest check that separates "an address" from "text".
//
// Deliberately loose: validating email with a regex is a classic way to reject
// legitimate addresses, and the only thing that really knows whether an address
// exists is the server on the other side. All that is wanted here is not to
// spend a round trip to the provider on a string that is obviously not an
// address.
func validAddress(s string) bool {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, "@")
	return i > 0 && i < len(s)-1 && !strings.ContainsAny(s, " \t\r\n")
}
