package attention

import "context"

// Repository is the box's read side. There is no Create and no Resolve: an item
// is born of an EVENT and dies of one (ADR-0004), and a port that offered direct
// writes would be an invitation to create items by hand — and the box would stop
// being a projection.
type Repository interface {
	// List returns the account's items, most urgent first. An empty `demandID`
	// brings the whole account, which is the box's own view; filled in, it
	// brings a single demand's.
	List(ctx context.Context, accountID, demandID string, includeResolved bool, limit int) ([]Item, error)

	// OpenTotal is the badge's number. It exists separately from List because
	// counting the PAGE would give a badge that changes as the dev paginates.
	OpenTotal(ctx context.Context, accountID string) (int, error)
}

// Watcher is the log subscription — the same fan-out machinery as the event
// domain, taken as a port so the box does not know where it comes from.
type Watcher interface {
	Watch(ctx context.Context, sinceEventID string, aggregates, types []string, emit func(EventNotice) error) error
}

// EventNotice is the raw event reaching the service, with the account already
// resolved.
type EventNotice = Event
