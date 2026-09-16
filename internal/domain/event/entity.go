// Package event is the domain of the LIVE READ of the event log.
//
// The truth is the log (ADR-0006); this package does not produce it, it only
// delivers it as it happens. Writing an event is the use case's job, in the same
// transaction as the state (ADR-0019) — here we only read.
//
// House rule: no Postgres, no NATS, no gRPC. What is needed from outside comes
// as a PORT — Repository (repository.go) and ports.EventBus.
package event

import (
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
)

// The event itself gets NO new type: ports.Event is already the house
// vocabulary and mirrors the `events` table field for field. Inventing an
// event.Event here would only create one more translation to keep in sync.

// Cursor is an event's POSITION in the log.
//
// It comes as a pair because the primary key of `events` is (id, occurred_at)
// and the table is partitioned by occurred_at: comparing only the id defines no
// ordering at all, and comparing only the instant ties between events in the
// same microsecond. The pair orders totally and stably — which is what a cursor
// has to be.
type Cursor struct {
	OccurredAt time.Time
	ID         string
}

// Filter narrows what the subscriber cares about. Empty lists = everything.
//
// The SAME filter is applied on replay (becoming a WHERE in Postgres) and on the
// live stream (in memory). They have to agree: if they diverged, the client
// would see one event at the seam and miss its sibling a second later.
type Filter struct {
	Aggregates []string // demand, project, workspace...
	Types      []string // dop.hierarchy.project.created
}

func (f Filter) Matches(e ports.Event) bool {
	return matchesAny(f.Aggregates, e.Aggregate) && matchesAny(f.Types, e.Type)
}

func matchesAny(allowed []string, v string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == v {
			return true
		}
	}
	return false
}

// belongsTo is per-account isolation, in one place.
//
// An event with no account (`identity.user.ensured` happens before the personal
// account exists — migration 0003) belongs to NOBODY: it leaks to no subscriber.
// That is why the comparison is strict equality and the empty string is refused
// on both sides, rather than treated as a wildcard.
func belongsTo(e ports.Event, accountID string) bool {
	return accountID != "" && e.AccountID == accountID
}

// Emitter delivers an event to the subscriber. Returning an error ends the
// stream — that is how the gRPC layer propagates "the client went away".
type Emitter func(e ports.Event) error
