//go:build integration

// The SAME contract suite, against the real Kubernetes API.
//
//	go test ./test/contract/ -tags=integration -v -run SecretStore
//
// This file was PROMISED in a comment in `secretstore_test.go` and did not
// exist: the k8s adapter — which is the production one for the self-hosted
// environment, and whose header claims to be "exercised EVERY DAY" — had never
// gone through the suite. Only the in-memory double passed, which proved the
// double is consistent with itself.
package contract_test

import (
	"os"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/secretstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestSecretStoreContractK8s(t *testing.T) {
	api := os.Getenv("K8S_API_SERVER")
	if api == "" {
		// `kubectl proxy --port=8001` is the path of least friction outside the
		// cluster: it already resolves authentication and TLS.
		api = "http://127.0.0.1:8001"
	}
	ns := os.Getenv("SECRET_NAMESPACE")
	if ns == "" {
		ns = "dop-local"
	}
	contract.SecretStoreSuite(t, "kubernetes", func(t *testing.T) ports.SecretStore {
		k := secretstore.NewK8s(secretstore.K8sConfig{
			APIServer: api,
			Token:     os.Getenv("K8S_TOKEN"),
			Namespace: ns,
		})
		// A cheap probe: if the API does not answer, it SKIPS with a reason —
		// instead of letting every subtest fail on unavailability and look like
		// a defect of the adapter.
		if _, err := k.Exists(t.Context(), ports.SecretRef{
			AccountID: "probe", Kind: "probe", OwnerID: "probe",
		}); err != nil {
			t.Skipf("Kubernetes API unavailable at %s: %v", api, err)
		}
		return k
	})
}
