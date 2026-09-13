package postgres

import (
	"encoding/json"
	"testing"
	"time"

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
