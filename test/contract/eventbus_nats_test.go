//go:build integration

// A MESMA suíte de contrato, agora contra o NATS de verdade.
//
//	go test ./test/contract/ -tags=integration -v
//
// É esta execução que dá sentido à regra dos dois adaptadores: uma suíte que só
// roda contra o duplo em memória prova que o duplo é consistente consigo mesmo.
// Foi divergência de CODIFICAÇÃO entre as duas pontas — o relay publicando
// envelope JSON e o assinante esperando base64 — que já deixou toda entrega
// sendo descartada em silêncio.
package contract_test

import (
	"context"
	"os"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestEventBusContractNATS(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://127.0.0.1:4222"
	}
	contract.EventBusSuite(t, "nats", func(t *testing.T) ports.EventBus {
		ctx := context.Background()
		bus, err := eventbus.NewNATS(ctx, url)
		if err != nil {
			t.Skipf("NATS indisponível em %s: %v", url, err)
		}
		t.Cleanup(func() { _ = bus.Close() })
		return bus
	})
}
