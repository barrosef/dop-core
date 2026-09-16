//go:build integration

// The SAME contract suite, now against real Kubernetes.
//
//	kubectl proxy --port=8001 --reject-paths='^$' &
//	go test ./test/contract/ -tags=integration -run TestSandboxContractK8s -v
//
// ── `--reject-paths` IS NOT OPTIONAL ────────────────────────────────────────
//
// `kubectl proxy` refuses the exec and attach paths by DEFAULT: the default for
// `--reject-paths` is `^/api/.*/pods/.*/exec,^/api/.*/pods/.*/attach`. Without
// the flag, the Exec subtests come back 403 at the handshake, BEFORE any
// WebSocket exists — and the proxy's 403 does not say what to do. It is recorded
// here, in internal/adapter/sandbox/websocket.go's header and in the adapter's
// own error message, because it is the kind of detail that costs an afternoon
// when it is written down nowhere. Inside the cluster (the real deployment, with
// a service account) the question does not arise: there is no proxy in the way.
//
// It is this run that gives the two-adapter rule its meaning: running the suite
// only against Docker would prove Docker is consistent with itself. The two
// implementations do not have one line in common — a namespace and a PVC on one
// side, a container and a volume on the other — and that is exactly why passing
// the same thirteen checks means something.
package contract_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/adapter/sandbox"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/test/contract"
)

func TestSandboxContractK8s(t *testing.T) {
	api := os.Getenv("K8S_API_SERVER")
	if api == "" {
		// `kubectl proxy` is the path of least friction outside the cluster: it
		// already resolves the CA and the credential from the kubeconfig, and
		// the adapter goes on speaking the SAME API it would speak from inside a
		// pod.
		api = "http://127.0.0.1:8001"
	}
	launcher := sandbox.NewK8s(sandbox.K8sConfig{
		APIServer:     api,
		Token:         os.Getenv("K8S_TOKEN"),
		WorkspaceSize: "1Gi",
		StorageClass:  os.Getenv("K8S_STORAGE_CLASS"),
		Timeout:       30 * time.Second,
	})

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
		t.Skipf("the Kubernetes API at %s answered HTTP %d — a missing credential or no permission",
			api, resp.StatusCode)
	}

	tiers, err := launcher.SupportedTiers(context.Background())
	if err != nil {
		t.Skipf("could not discover the cluster's isolation levels: %v", err)
	}
	t.Logf("isolation levels this cluster offers: %v", tiers)

	contract.SandboxSuite(t, "kubernetes", func(t *testing.T) (ports.SandboxLauncher, contract.SandboxEnv) {
		return launcher, contract.SandboxEnv{
			NamespacePrefix: "dop-ct",
			Image:           testImage(),
			Tier:            ports.TierNamespace,
			// k3d ships no Kata RuntimeClass. Asking for hardware isolation here
			// has to REFUSE — it is the execution spec's R-4, and the case where
			// degrading in silence would cost dearly in production.
			Unsupported: ports.TierHardware,
			// A pod pulls an image and waits for the volume provisioner; the
			// local container does neither.
			Ready: 180 * time.Second,
			// The k3d network's gateway: `host.k3d.internal` is on the NODE's
			// /etc/hosts and pods do not resolve it. Override with
			// SANDBOX_GIT_HOST on a cluster whose network differs.
			GitHost: envOr("SANDBOX_GIT_HOST", "172.24.0.1"),
		}
	})
}
