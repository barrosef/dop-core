//go:build integration

// The substrate's contract suite against the host's Docker.
//
//	go test ./test/contract/ -tags=integration -run TestSandboxContractDocker -v
//
// This is the adapter that makes developing the platform possible with no
// cluster — and it is by running it and the Kubernetes one through the SAME
// suite that one discovers where the two diverge. Without that, the port would
// come out in the shape of whichever inspired it.
package contract_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/sandbox"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestSandboxContractDocker(t *testing.T) {
	socket := sandbox.DefaultDockerSocket
	if v := os.Getenv("DOCKER_HOST"); strings.HasPrefix(v, "unix://") {
		socket = strings.TrimPrefix(v, "unix://")
	}
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Skipf("Docker unavailable at %s: %v — start the daemon or point DOCKER_HOST", socket, err)
	}
	_ = conn.Close()

	launcher := sandbox.NewDocker(sandbox.DockerConfig{Socket: socket})
	tiers, err := launcher.SupportedTiers(context.Background())
	if err != nil {
		t.Skipf("the Docker daemon did not answer: %v", err)
	}
	t.Logf("isolation levels this host offers: %v", tiers)

	contract.SandboxSuite(t, "docker", func(t *testing.T) (ports.SandboxLauncher, contract.SandboxEnv) {
		return launcher, contract.SandboxEnv{
			NamespacePrefix: "dop-ct",
			Image:           testImage(),
			Tier:            ports.TierNamespace,
			// No development host has Kata registered in Docker. It is precisely
			// the case guarantee 1 exists to cover: asking for a microVM here
			// has to REFUSE, not deliver an ordinary container.
			Unsupported: ports.TierHardware,
			Ready:       90 * time.Second,
			GitHost:     envOr("SANDBOX_GIT_HOST", "172.17.0.1"),
		}
	})
}

// testImage is tiny on purpose: the suite has to run on the laptop of whoever
// touches the adapter, not only in CI.
func testImage() string {
	if v := os.Getenv("DOP_SANDBOX_TEST_IMAGE"); v != "" {
		return v
	}
	return "busybox:1.36"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
