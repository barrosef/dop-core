//go:build integration

// The verification runner's contract suite against a real Kubernetes cluster.
//
//	kubectl proxy --port=8001 &
//	go test ./test/contract/ -tags=integration -run TestRunnerContractK8s -v
//
// The proxy is the same path of least friction the sandbox suite uses: it
// already resolves the CA and the credential from the kubeconfig, and the
// adapter goes on speaking the SAME API it would speak from inside a pod. This
// suite needs no `--reject-paths`: the runner has no exec.
//
// ── K8S_GIT_HOST, and why the default may be wrong for your cluster ─────────
//
// A runner has to reach the suite's git server, which listens on THIS machine.
// The default below is k3d's documented alias — and k3d only injects it into
// CoreDNS on some versions. When it is missing, every subtest fails with
// `clone failed`, which reads like an adapter bug and is not one. The cluster's
// gateway is the reliable answer:
//
//	docker network inspect k3d-<cluster> --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}'
//	K8S_GIT_HOST=172.24.0.1 go test ...
//
// It is the second adapter ADR-0001 demands, and it is what turns the port from
// a guess into a contract: the pod and the host's daemon share the run SCRIPT
// and nothing else.
package contract_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/verification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestRunnerContractK8s(t *testing.T) {
	api := os.Getenv("K8S_API_SERVER")
	if api == "" {
		api = "http://127.0.0.1:8001"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api+"/version", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Kubernetes unavailable at %s: %v — run `kubectl proxy --port=8001` "+
			"or point K8S_API_SERVER/K8S_TOKEN at the cluster", api, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Skipf("the Kubernetes API at %s answered HTTP %d", api, resp.StatusCode)
	}

	runner := verification.NewK8s(verification.K8sConfig{
		APIServer: api,
		Token:     os.Getenv("K8S_TOKEN"),
		// The suite's cluster is k3d, whose local-path provisioner binds a
		// volume only when a pod consumes it. That is fine here: the cache
		// guarantee is about two runs of one account seeing the same data, and
		// local-path gives that on a single node.
		StorageClass: os.Getenv("K8S_STORAGE_CLASS"),
		CacheSize:    "1Gi",
	})
	contract.RunnerSuite(t, "kubernetes", func(t *testing.T) (ports.VerificationRunner, contract.RunnerEnv) {
		return runner, contract.RunnerEnv{
			NamespacePrefix: "dop-rn",
			Image:           runnerTestImage(),
			DepImage:        envOr("DOP_RUNNER_DEP_IMAGE", "redis:7-alpine"),
			DepPort:         6379,
			// A pod pulling an image takes far longer than a local container.
			Ready:   240 * time.Second,
			GitHost: envOr("K8S_GIT_HOST", "host.k3d.internal"),
		}
	})
}
