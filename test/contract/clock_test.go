package contract_test

import (
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/clock"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// Both clocks go through the SAME suite. That is the point: if the controllable
// clock does not deliver the real one's guarantees, a test that passes with it
// says nothing about production.
func TestClockContract(t *testing.T) {
	contract.ClockSuite(t, "system", func(t *testing.T) ports.Clock {
		return clock.NewSystem()
	})
	contract.ClockSuite(t, "fixed", func(t *testing.T) ports.Clock {
		return clock.NewFixed(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	})
}
