package agentmetrics

import (
	"context"
	"time"
)

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

	// ConsumptionOf aggregates a demand's turns. It is a QUERY and not a stored
	// projection: the aggregates that matter change as the questions do, and a
	// stored aggregate ages and starts to lie.
	ConsumptionOf(ctx context.Context, accountID, demandID string) (*Consumption, error)
}

// Consumption is the number phase 1 exists to move.
type Consumption struct {
	DemandID string
	Sessions int
	Turns    int

	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64

	TokensByModel map[string]int64
	CallsByTool   map[string]int
	FirstTurnAt   time.Time
	LastTurnAt    time.Time
}

// CacheRatio is the share of the incoming context that came from cache.
func (c Consumption) CacheRatio() float64 {
	in := c.InputTokens + c.CacheCreationTokens + c.CacheReadTokens
	if in == 0 {
		return 0
	}
	return float64(c.CacheReadTokens) / float64(in)
}
