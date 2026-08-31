package contract_test

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/objectstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// O adaptador de sistema de arquivos roda sempre — é ele que faz o self-hosted
// existir sem GCS. O do GCS roda contra o emulador, sob a tag `integration`.
func TestObjectStoreContract(t *testing.T) {
	contract.ObjectStoreSuite(t, "fs", func(t *testing.T) (ports.ObjectStore, []string) {
		// t.TempDir some ao fim do teste: nada vaza entre execuções.
		fs, err := objectstore.NewFS(t.TempDir())
		if err != nil {
			t.Fatalf("não foi possível criar o objectstore de arquivos: %v", err)
		}
		return fs, []string{"balde-a", "balde-b"}
	})
}
