package notification

import (
	"testing"
	"time"
)

// The table is the entire policy. These tests check the properties that make it
// REPLACEABLE by data (P-29) — not the content of each row, which changes.

func TestEveryRowHasANameAnActionAKindAndAWhy(t *testing.T) {
	for _, r := range Rules() {
		if r.Name == "" {
			t.Errorf("rule with no name: the name goes into the idempotency key, "+
				"and a key with an empty piece collides with the next unnamed rule: %+v", r)
		}
		if r.Action == "" {
			t.Errorf("rule %q has no action: under P-29 the action stops being only "+
				"email, and the key already has to carry it", r.Name)
		}
		if r.Kind == "" {
			t.Errorf("rule %q has no kind: the kind is what every adapter has to resolve", r.Name)
		}
		if r.Why == "" {
			t.Errorf("rule %q has no reason: a policy with no why is not auditable — "+
				"nobody can disagree with 'invite.created → email'", r.Name)
		}
		switch r.Trigger {
		case TriggerEvent:
			if r.Event == "" {
				t.Errorf("rule %q is event-triggered and does not say which", r.Name)
			}
		case TriggerAttentionBox:
			if r.Delay <= 0 {
				t.Errorf("rule %q hangs off the box and has no delay: with no wait, one "+
					"email per item makes the inbox useless", r.Name)
			}
		default:
			t.Errorf("rule %q has an unknown trigger %q", r.Name, r.Trigger)
		}
	}
}

func TestRuleNamesAreUnique(t *testing.T) {
	// A repeated name is a repeated idempotency key: the second rule for the same
	// event would be discarded as a duplicate of the first — in silence, which is
	// the failure mode the composite key exists to prevent.
	vistos := map[string]bool{}
	for _, r := range Rules() {
		if vistos[r.Name] {
			t.Fatalf("two rules named %q", r.Name)
		}
		vistos[r.Name] = true
	}
}

func TestKindsComesFromTheTableAndDoesNotRepeat(t *testing.T) {
	ks := Kinds()
	if len(ks) == 0 {
		t.Fatal("no kinds: the Mailer contract suite would prove nothing")
	}
	naTabela := map[Kind]bool{}
	for _, r := range Rules() {
		naTabela[r.Kind] = true
	}
	vistos := map[Kind]bool{}
	for _, k := range ks {
		if vistos[k] {
			t.Errorf("kind %q repeated in Kinds()", k)
		}
		vistos[k] = true
		if !naTabela[k] {
			t.Errorf("kind %q comes from no rule: Kinds() became a hand-written list, "+
				"and a hand-written list is what leaves the contract suite green with a "+
				"faltando", k)
		}
	}
	for k := range naTabela {
		if !vistos[k] {
			t.Errorf("a rule declares kind %q and Kinds() does not return it", k)
		}
	}
}

func TestSubjectsCoversEveryEventInTheTable(t *testing.T) {
	// One subscription too FEW makes the event never arrive — nobody receives it
	// and nothing fails. It is the same trap as attention.Subjects, and here it is
	// derived precisely so it does not depend on somebody remembering.
	subscribed := map[string]bool{}
	for _, s := range Subjects() {
		subscribed[s] = true
	}
	for _, r := range Rules() {
		if r.Trigger == TriggerEvent && !subscribed[r.Event] {
			t.Errorf("rule %q reacts to %q and the consumer does not subscribe to that subject",
				r.Name, r.Event)
		}
	}
}

func TestTheDigestRuleIsUniqueAndFoundByName(t *testing.T) {
	r, ok := DigestRule()
	if !ok {
		t.Fatal("with no digest rule, the box never becomes an email")
	}
	if r.Name == "" {
		t.Fatal("the digest rule needs a name: it is written into the idempotency " +
			"key, and a sweep that assumed a position in the table would write the " +
			"wrong key the day somebody reordered the literal")
	}
	n := 0
	for _, c := range Rules() {
		if c.Trigger == TriggerAttentionBox {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d box rules: DigestRule would return an arbitrary one", n)
	}
}

func TestApplyIgnoresWhatIsNotInTheTable(t *testing.T) {
	e := Event{ID: "ev-1", AccountID: "account-1", Type: "dop.demand.stage.advanced",
		OccurredAt: time.Now(), Payload: map[string]any{}}
	if cs := Apply(e, noRecipient); len(cs) != 0 {
		t.Fatalf("an event outside the table became %d command(s)", len(cs))
	}
}

func TestApplyIgnoresAnEventWithNoAccount(t *testing.T) {
	// `dop.identity.user.ensured` happens on the first login, before the personal
	// account exists (migration 0003). There is no account to notify on
	// behalf of.
	e := Event{ID: "ev-1", Type: EvInviteCreated,
		Payload: map[string]any{"email": "a@b.test"}}
	if cs := Apply(e, payloadEmail); len(cs) != 0 {
		t.Fatalf("an event with no account became %d command(s)", len(cs))
	}
}

func TestApplyForAnInviteBuildsTheCompositeKey(t *testing.T) {
	e := Event{
		ID: "ev-1", AccountID: "account-1", Aggregate: "invite", AggregateID: "inv-1",
		Type: EvInviteCreated, OccurredAt: time.Now(),
		Payload: map[string]any{"email": "convidado@exemplo.test", "role": "member"},
	}
	cs := Apply(e, payloadEmail)
	if len(cs) != 1 {
		t.Fatalf("expected 1 command, got %d", len(cs))
	}
	c := cs[0]
	if !c.Valid() {
		t.Fatalf("invalid command: %+v", c)
	}
	ev, ruleName, action := c.Key()
	if ev != "ev-1" || ruleName == "" || action == "" {
		t.Fatalf("incomplete key: (%q, %q, %q) — a key made only of the event would "+
			"discard the same event's second action as a duplicate, in silence", ev, ruleName, action)
	}
	if c.Kind != KindInvite {
		t.Fatalf("kind %q, expected %q", c.Kind, KindInvite)
	}
	if len(c.Recipients) != 1 || c.Recipients[0].Email != "convidado@exemplo.test" {
		t.Fatalf("recipient: %+v — the invitee IS NOT YET A USER, and the only place "+
			"their address exists is the payload", c.Recipients)
	}
	if c.Data["role"] != "member" {
		t.Fatalf("the template data did not carry `role`: %+v", c.Data)
	}
}

func TestApplyWithNoRecipientDoesNotBecomeACommand(t *testing.T) {
	// With no recipient there is nothing to fire, and that is NOT an error:
	// stopping the consumer here would delay every notification in the queue.
	e := Event{ID: "ev-1", AccountID: "account-1", Type: EvInviteCreated,
		Payload: map[string]any{"email": "isto-nao-e-endereco"}}
	if cs := Apply(e, payloadEmail); len(cs) != 0 {
		t.Fatalf("an invalid address became %d command(s)", len(cs))
	}
}

func TestRulesReturnsACopy(t *testing.T) {
	// A policy the caller can edit in memory stops being a policy.
	rs := Rules()
	if len(rs) == 0 {
		t.Fatal("the table is empty")
	}
	original := rs[0].Name
	rs[0].Name = "sabotado"
	if Rules()[0].Name != original {
		t.Fatal("Rules() returned the internal slice: the caller can rewrite the policy")
	}
}

func noRecipient(recipientSpec, Event) []Recipient { return nil }

// A `{field}` in LinkPath that is not in Data becomes a URL with a literal key in
// the email — the person clicks, it breaks, and they conclude the invite is
// worthless. The error is invisible in the code (the row looks right) and
// expensive in production, so the table refuses to take that shape.
func TestLinkPlaceholderMustBeInData(t *testing.T) {
	for _, r := range Rules() {
		fields := map[string]bool{}
		for _, d := range r.Data {
			fields[d] = true
		}
		for _, ph := range placeholders(r.LinkPath) {
			if !fields[ph] {
				t.Errorf("rule %q: LinkPath uses {%s}, which is not in Data %v",
					r.Name, ph, r.Data)
			}
		}
	}
}

func TestResolvePathTrocaOCampoEEscapa(t *testing.T) {
	got := resolvePath("/invites/{invite_id}", map[string]any{"invite_id": "inv-1/2"})
	if got != "/invites/inv-1%2F2" {
		t.Errorf("resolved path %q — the value has to be escaped, or it invents a URL segment", got)
	}
}

// A missing or empty field erases the whole link. Half a link is worse than no
// link: the button shows up and leads nowhere.
func TestResolvePathWithoutTheFieldErasesTheLink(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"ausente": {},
		"vazio":   {"invite_id": ""},
		"branco":  {"invite_id": "   "},
	} {
		if got := resolvePath("/invites/{invite_id}", data); got != "" {
			t.Errorf("%s: the path should be empty, got %q", name, got)
		}
	}
}
