package postgres

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

func TestTheTerminalRowCarriesWhoAndWhat(t *testing.T) {
	// The panel's whole value is answering "what happened, exactly" without a
	// second query. If the context does not reach the row, it never will.
	dl := event.DeadLetter{
		Event: ports.Event{
			ID: "ev-1", AccountID: "9a2c", Aggregate: "account",
			AggregateID: "9a2c", AggregateKey: "acme",
			Type:      "dop.identity.account.created",
			ActorKind: "user", ActorID: "u-1", RequestID: "req-1",
		},
		Consumer: "notification",
		Attempts: []event.Attempt{
			{ErrorKind: "unavailable", ErrorCode: "mail.provider_down", ErrorMessage: "boom"},
		},
	}

	cols := EventErrorColumns(dl, event.Irrecoverable)

	if cols.AggregateKey != "acme" || cols.ActorID != "u-1" || cols.RequestID != "req-1" {
		t.Fatalf("the context did not reach the row: %+v", cols)
	}
	if cols.LastCode != "mail.provider_down" || cols.LastMessage != "boom" {
		t.Fatalf("the last failure did not reach the row: %+v", cols)
	}
	if cols.Classification != "irrecoverable" {
		t.Fatalf("the final classification did not reach the row: %q", cols.Classification)
	}
}
