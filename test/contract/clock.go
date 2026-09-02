package contract

import (
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// ClockSuite verifies the four guarantees documented on the Clock port.
//
// It looks like little for a one-line port, and that is precisely the point: it
// was by looking too trivial to deserve an adapter that it spent months being
// passed as nil in the composition root, with the domain calling time.Now()
// internally.
func ClockSuite(t *testing.T, name string, newClock func(t *testing.T) ports.Clock) {
	t.Run(name, func(t *testing.T) {
		t.Run("1_never_returns_the_zero_instant", func(t *testing.T) {
			c := newClock(t)
			if c.Now().IsZero() {
				t.Fatal("Now() returned the zero instant — a deadline rule computed from " +
					"the year 1 expires everything, or nothing, with no warning")
			}
		})

		t.Run("2_always_in_utc", func(t *testing.T) {
			c := newClock(t)
			got := c.Now()
			if got.Location() != time.UTC {
				t.Fatalf("Now() returned zone %v; the port requires UTC", got.Location())
			}
			// _, offset := got.Zone() confirms it is not a "UTC" in name only.
			if _, offset := got.Zone(); offset != 0 {
				t.Fatalf("zone offset %ds; the port requires UTC", offset)
			}
		})

		t.Run("3_does_not_go_backwards", func(t *testing.T) {
			c := newClock(t)
			previous := c.Now()
			for i := 0; i < 1000; i++ {
				current := c.Now()
				if current.Before(previous) {
					t.Fatalf("Now() went backwards on call %d: %s came after %s",
						i, current.Format(time.RFC3339Nano), previous.Format(time.RFC3339Nano))
				}
				previous = current
			}
		})

		t.Run("4_safe_for_concurrent_use", func(t *testing.T) {
			c := newClock(t)
			var wg sync.WaitGroup
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					previous := c.Now()
					for i := 0; i < 500; i++ {
						current := c.Now()
						if current.IsZero() || current.Before(previous) {
							t.Errorf("inconsistent read under concurrency: %v after %v", current, previous)
							return
						}
						previous = current
					}
				}()
			}
			wg.Wait()
		})
	})
}
