package contract_test

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/secretstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// The in-memory adapter always runs. The k8s one runs when there is a cluster
// (see secretstore_k8s_test.go) and GCP's when there is a credential.
func TestSecretStoreContract(t *testing.T) {
	contract.SecretStoreSuite(t, "memory", func(t *testing.T) ports.SecretStore {
		return secretstore.NewMemory()
	})
}
