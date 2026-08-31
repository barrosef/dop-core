package contract_test

import (
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/clock"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// Os dois relógios passam pela MESMA suíte. É esse o ponto: se o relógio
// controlável não cumprir as garantias do de verdade, um teste que passa com
// ele não diz nada sobre produção.
func TestClockContract(t *testing.T) {
	contract.ClockSuite(t, "sistema", func(t *testing.T) ports.Clock {
		return clock.NewSystem()
	})
	contract.ClockSuite(t, "fixo", func(t *testing.T) ports.Clock {
		return clock.NewFixed(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	})
}
