package contract_test

import (
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/eventbus"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/test/contract"
)

// The in-memory bus always runs; NATS runs under the `integration` tag
// (eventbus_nats_test.go). Both go through the SAME suite — it was an encoding
// divergence between the two ends that once left the system mute and green.
func TestEventBusContract(t *testing.T) {
	contract.EventBusSuite(t, "memory", func(t *testing.T) ports.EventBus {
		return eventbus.NewMemory()
	})
}
