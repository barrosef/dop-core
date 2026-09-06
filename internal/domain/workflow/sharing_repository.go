package workflow

import (
	"context"
	"time"
)

// SharingRepository is the persistence PORT for everything that crosses — or
// prepares to cross — an account boundary.
//
// It is separate from Repository on purpose: that one carries the invariants of
// a flow's identity and versions, and reopening it to add publication would put
// two unrelated sets of rules in one place.
//
// Every operation takes accountID except ResolvePublication, which is the ONE
// deliberate crossing: it reads another account's publication, and it is
// authorised by a share row, not by the caller's word.
type SharingRepository interface {
	CreatePublication(ctx context.Context, p *Publication, idempotencyKey string) (*Publication, error)
	PublicationByID(ctx context.Context, accountID, id string) (*Publication, error)
	Withdraw(ctx context.Context, accountID, id string, at time.Time) error

	// ResolvePublication answers a reference for a caller. It returns NotFound —
	// never Permission — when the caller holds no share: whether a flow exists
	// in another account is not something an outsider gets to learn.
	ResolvePublication(ctx context.Context, callerAccountID string, ref PublicationRef) (*Publication, error)

	CreateShare(ctx context.Context, s *Share, idempotencyKey string) (*Share, error)
	ShareByID(ctx context.Context, accountID, id string) (*Share, error)
	SharesOfPublication(ctx context.Context, accountID, publicationID string) ([]Share, error)

	// RevokeShare does the WHOLE revocation in one transaction: the share, the
	// copies the policy reaches, their adoption records, and both events.
	//
	// It is one call and not four because a crash between four calls leaves a
	// share revoked with its copies untouched — a half-revocation nobody would
	// notice until somebody used a flow that was supposed to be gone. The
	// transaction and the outbox are the adapter's job (ADR-0019); the domain's
	// job is to decide WHAT the policy reaches and hand it over.
	RevokeShare(ctx context.Context, accountID string, rev Revocation) error

	RecordAdoption(ctx context.Context, a *Adoption) error
	AdoptionsOfPublication(ctx context.Context, accountID, publicationID string) ([]Adoption, error)

	Pin(ctx context.Context, accountID, flowID string, version int32, by string, at time.Time) error
	PinOf(ctx context.Context, accountID, flowID string) (int32, bool, error)
}
