package clock

import (
	"sync"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// reference is the instant used when nobody picks one.
//
// It exists so that NewFixed(time.Time{}) still honours the port's guarantee
// ("Now never returns the zero value") instead of producing a clock that
// satisfies the compiler and violates the contract.
var reference = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// Fixed is the clock the test controls: it only moves when told to.
//
// This is what replaces time.Sleep. Testing "an invite expires in 14 days" with
// a real clock means either waiting 14 days or fabricating an ExpiresAt in the
// past — that is, testing something else. With the fixed clock the test
// advances 14 days in nanoseconds and checks the REAL rule, the same one that
// runs in production.
type Fixed struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFixed creates the clock stopped at t (normalized to UTC, as the port
// requires).
func NewFixed(t time.Time) *Fixed {
	if t.IsZero() {
		t = reference
	}
	return &Fixed{now: t.UTC()}
}

func (f *Fixed) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Advance pushes the clock forward and returns the new instant.
//
// Forward only, on purpose: the port guarantees Now does not go backwards, and
// a test clock that rewound would let an adapter pass the contract by accident
// of the path taken. A negative duration is a bug in the test, and a bug in the
// test has to be loud.
func (f *Fixed) Advance(d time.Duration) time.Time {
	if d < 0 {
		panic("clock.Fixed.Advance: the clock does not run backwards")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

var _ ports.Clock = (*Fixed)(nil)
