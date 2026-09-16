package event

import (
	"context"

	"github.com/barrosef/dop-core/internal/domain/ports"
)

// Repository is the event log's read PORT.
//
// A minimal surface on purpose: replay needs exactly two things — find where the
// client stopped and read what came after. Everything else (aggregation,
// counting, dossiers) is a projection and does not belong here.
//
// Every operation takes accountID and MUST filter by it inside the WHERE:
// multi-tenant isolation is a constraint, not a convention.
type Repository interface {
	// Locate returns the event's position in the log.
	//
	// It takes accountID because the cursor is attack surface too: an event id
	// from another account has to return "not found", never the position —
	// otherwise the id's existence leaks.
	Locate(ctx context.Context, accountID, eventID string) (Cursor, error)

	// EventsAfter returns, in order of occurrence, up to `limit` of the
	// account's events after `after` that match the filter.
	EventsAfter(ctx context.Context, accountID string, after Cursor, f Filter, limit int) ([]ports.Event, error)
}
