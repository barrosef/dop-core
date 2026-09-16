package attention

import (
	"context"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Service serves the box. It is deliberately small: the intelligence lives in
// the impact table and in the event map, both testable without a database.
type Service struct {
	repo    Repository
	watcher Watcher
	clock   ports.Clock
}

func NewService(repo Repository, watcher Watcher, clock ports.Clock) *Service {
	if repo == nil || watcher == nil {
		panic("attention.NewService: repository and watcher are required")
	}
	if clock == nil {
		panic("attention.NewService: clock is required — use clock.NewSystem()")
	}
	return &Service{repo: repo, watcher: watcher, clock: clock}
}

// DefaultLimit exists because the box is for DECIDING, not for browsing: if
// someone has more than a hundred open items, pagination is not the problem.
const DefaultLimit = 100

// List returns the active account's box, already ordered.
//
// Priority is computed on READ, not stored: it depends on age, and age changes
// on its own. A materialized priority would go stale in silence.
func (s *Service) List(ctx context.Context, demandID string, includeResolved bool, limit int) ([]Item, int, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > DefaultLimit {
		limit = DefaultLimit
	}
	items, err := s.repo.List(ctx, accountID, demandID, includeResolved, limit)
	if err != nil {
		return nil, 0, err
	}
	Sort(items, s.clock.Now())

	total, err := s.repo.OpenTotal(ctx, accountID)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Watch follows the box live, translating events into changes.
//
// It reuses the event domain's fan-out through the `Watcher` port: a second
// fan-out implementation would be a second chance to get cross-account
// isolation wrong.
func (s *Service) Watch(ctx context.Context, sinceEventID string, emit func(change, eventID string, it Item) error) error {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return err
	}
	if emit == nil {
		return errs.Invalid("Watch requires an emitter")
	}
	return s.watcher.Watch(ctx, sinceEventID, nil, nil, func(e Event) error {
		d := Apply(e)
		switch {
		case d.Open != nil:
			return emit("opened", e.ID, *d.Open)
		case d.Close != nil:
			// Closing does not carry the whole item — whoever closes knows the
			// TARGET, not the id. The client matches by target, the way the
			// projection does.
			return emit("resolved", e.ID, Item{
				AccountID: e.AccountID, Kind: d.Close.Kind,
				TargetKind: d.Close.TargetKind, TargetID: d.Close.TargetID,
			})
		}
		return nil
	})
}
