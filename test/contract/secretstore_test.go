package contract_test

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/secretstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// O adaptador em memória roda sempre. O k8s roda quando há cluster
// (ver secretstore_k8s_test.go) e o do GCP quando há credencial.
func TestSecretStoreContract(t *testing.T) {
	contract.SecretStoreSuite(t, "memoria", func(t *testing.T) ports.SecretStore {
		return secretstore.NewMemory()
	})
}
