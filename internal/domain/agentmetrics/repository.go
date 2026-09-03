package agentmetrics

import "context"

// Repository is where the collected measurement lands.
//
// Two verbs, and the shape of both is decided by one fact: collection happens
// MORE THAN ONCE over the same file. The agent is still working while we read,
// so every pass re-reads a little and must not duplicate anything.
type Repository interface {
	// EnsureSession creates or finds the session by (account, external id) and
	// returns it WITH the stored cursor — which is where the next read starts.
	EnsureSession(ctx context.Context, s Session) (*Session, error)

	// RecordTurns writes the turns and advances the cursor, in ONE transaction.
	//
	// The two together or neither: a cursor that moved without the turns being
	// written loses them silently, and turns written without the cursor moving
	// makes the next pass read them again. It is idempotent by (session, uuid),
	// so a re-read costs nothing.
	RecordTurns(ctx context.Context, sessionID string, turns []Turn, offset int64) error
}
