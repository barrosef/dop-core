package contract_test

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// O barramento em memória roda sempre; o NATS roda sob a tag `integration`
// (eventbus_nats_test.go). Ambos passam pela MESMA suíte — foi divergência de
// codificação entre as duas pontas que já deixou o sistema mudo e verde uma vez.
func TestEventBusContract(t *testing.T) {
	contract.EventBusSuite(t, "memoria", func(t *testing.T) ports.EventBus {
		return eventbus.NewMemory()
	})
}
