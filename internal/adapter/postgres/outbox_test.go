package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
)

func TestTheEnvelopeCarriesWhoCausedTheEvent(t *testing.T) {
	// The information already reaches the events table. What this pins is that
	// it also reaches the CONSUMER, which is where it is needed and where it
	// used to be dropped.
	call := ctxutil.Call{
		ActorKind: ctxutil.ActorUser, ActorID: "u-1",
		RequestID: "req-1", SessionID: "s-1", Caller: "bff",
	}
	e := ports.Event{
		AccountID: "acct-1", Aggregate: "account", AggregateID: "9a2c",
		AggregateKey: "acme", Type: "dop.identity.account.created",
		Payload: []byte(`{}`), OccurredAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	}

	raw := envelopeOf("ev-1", e, call)

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unreadable envelope: %v", err)
	}
	for field, want := range map[string]string{
		"actor_kind": "user", "actor_id": "u-1", "request_id": "req-1",
		"session_id": "s-1", "caller": "bff", "aggregate_key": "acme",
	} {
		if got[field] != want {
			t.Errorf("%s: got %v, want %q", field, got[field], want)
		}
	}
}

// TestTheEnvelopeUnmarshalsIntoEventbusEnvelopeWithEveryFieldIntact pairs the
// outbox's own map-built envelope — the shape every REAL event takes — with
// eventbus.Envelope, the struct every consumer decodes into.
//
// The two are written independently: outbox.envelopeOf is a map with
// hand-written JSON keys, eventbus.Envelope is a struct built from the wire
// format on the other adapter's side. Keeping them apart is defensible —
// postgres importing eventbus in PRODUCTION code would be this repo's first
// adapter-to-adapter dependency — but nothing else guards the pairing: a key
// renamed on one side and not the other would silently drop that field for
// every consumer, decoded into a zero value with no error at all. This test
// is that guard, kept in the test binary only.
func TestTheEnvelopeUnmarshalsIntoEventbusEnvelopeWithEveryFieldIntact(t *testing.T) {
	call := ctxutil.Call{
		ActorKind: ctxutil.ActorUser, ActorID: "u-1",
		RequestID: "req-1", SessionID: "s-1", Caller: "bff",
	}
	e := ports.Event{
		AccountID: "acct-1", Aggregate: "account", AggregateID: "9a2c",
		AggregateKey: "acme", Type: "dop.identity.account.created",
		Payload: []byte(`{"x":1}`), OccurredAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	}

	raw := envelopeOf("ev-1", e, call)

	var env eventbus.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("the outbox's envelope did not decode into eventbus.Envelope: %v", err)
	}
	if env.ID != "ev-1" || env.AccountID != e.AccountID || env.Aggregate != e.Aggregate ||
		env.AggregateID != e.AggregateID || env.AggregateKey != e.AggregateKey || env.Type != e.Type {
		t.Fatalf("the identifying fields did not survive the round trip: %+v", env)
	}
	if string(env.Payload) != `{"x":1}` {
		t.Fatalf("the payload did not survive: %s", env.Payload)
	}
	if !env.OccurredAt.Equal(e.OccurredAt) {
		t.Fatalf("OccurredAt did not survive: %v != %v", env.OccurredAt, e.OccurredAt)
	}
	if env.ActorKind != "user" || env.ActorID != "u-1" || env.RequestID != "req-1" ||
		env.SessionID != "s-1" || env.Caller != "bff" {
		t.Fatalf("the call's context did not survive: %+v", env)
	}
}
