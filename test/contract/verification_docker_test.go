//go:build integration

// The verification runner's contract suite against the host's Docker.
//
//	go test ./test/contract/ -tags=integration -run TestRunnerContractDocker -v
//
// It is the adapter that lets a verification be developed with no cluster — and
// running it and the Kubernetes one through the SAME suite is what keeps the
// evidence from depending on where it was produced (ADR-0030).
package contract_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/verification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestRunnerContractDocker(t *testing.T) {
	socket := verification.DefaultDockerSocket
	if v := os.Getenv("DOCKER_HOST"); strings.HasPrefix(v, "unix://") {
		socket = strings.TrimPrefix(v, "unix://")
	}
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Skipf("Docker unavailable at %s: %v", socket, err)
	}
	_ = conn.Close()

	runner := verification.NewDocker(verification.DockerConfig{
		Socket: socket,
		// Under the suite the cache lives in a temporary directory: the
		// guarantee to prove is that it SURVIVES between runs of one account,
		// not that it survives the machine.
		CacheRoot: t.TempDir(),
	})
	if _, err := runner.Status(context.Background(), ports.RunnerHandle{ID: "none", Namespace: "none"}); err != nil {
		if strings.Contains(err.Error(), "failed to talk to Docker") {
			t.Skipf("the Docker daemon did not answer: %v", err)
		}
	}

	contract.RunnerSuite(t, "docker", func(t *testing.T) (ports.VerificationRunner, contract.RunnerEnv) {
		return runner, contract.RunnerEnv{
			NamespacePrefix: "dop-rn",
			Image:           runnerTestImage(),
			DepImage:        envOr("DOP_RUNNER_DEP_IMAGE", "redis:7-alpine"),
			DepPort:         6379,
			Ready:           180 * time.Second,
			GitHost:         envOr("SANDBOX_GIT_HOST", "172.17.0.1"),
		}
	})
}

// runnerTestImage needs git, nc and a POSIX shell — the three the run script
// checks for by name. `alpine/git` is small and has all three; the platform's
// real runner image is a different animal entirely (ADR-0030 §2).
func runnerTestImage() string {
	return envOr("DOP_RUNNER_TEST_IMAGE", "alpine/git:latest")
}
