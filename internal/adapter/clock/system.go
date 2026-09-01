// Adapters for the Clock port.
//
// Two implementations from day one, for the same reason as ADR-0001: a port
// with a single adapter is a guess. Here the pair also earns its keep — the
// system clock runs in production and the fixed one runs in tests, and it is
// the second that turns a deadline rule (a 14-day invite) into an exact
// assertion instead of a timed wait.
package clock

import (
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// System is the process's wall clock.
type System struct{}

func NewSystem() System { return System{} }

// Now returns the instant in UTC, always.
//
// Why normalize here and not at each caller: a timestamp with no defined zone
// is an ambiguity that only shows up when the process starts on a machine whose
// TZ differs from the database's — and then expiry comparisons are off by
// hours. The time zone is a presentation decision, and presentation belongs to
// the edge.
//
// Intended side effect of .UTC(): the monotonic reading is dropped, so that
// instants produced by two different clocks stay comparable with each other.
func (System) Now() time.Time { return time.Now().UTC() }

var _ ports.Clock = System{}
