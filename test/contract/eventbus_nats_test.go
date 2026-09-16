//go:build integration

// The SAME contract suite, now against real NATS.
//
//	go test ./test/contract/ -tags=integration -v
//
// It is this run that gives the two-adapter rule its meaning: a suite that only
// runs against the in-memory double proves the double is consistent with itself.
// It was an ENCODING divergence between the two ends — the relay publishing a
// JSON envelope and the subscriber expecting base64 — that once had every
// delivery discarded in silence.
package contract_test

import (
	"context"
	"os"
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/eventbus"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/test/contract"
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
			t.Skipf("NATS unavailable at %s: %v", url, err)
		}
		t.Cleanup(func() { _ = bus.Close() })
		return bus
	})
}
