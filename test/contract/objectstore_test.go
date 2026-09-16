package contract_test

import (
	"testing"

	"github.com/barrosef/dop-core/internal/adapter/objectstore"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/test/contract"
)

// The filesystem adapter always runs — it is what makes self-hosted exist with
// no GCS. The GCS one runs against the emulator, under the `integration` tag.
func TestObjectStoreContract(t *testing.T) {
	contract.ObjectStoreSuite(t, "fs", func(t *testing.T) (ports.ObjectStore, []string) {
		// t.TempDir disappears at the end of the test: nothing leaks between runs.
		fs, err := objectstore.NewFS(t.TempDir())
		if err != nil {
			t.Fatalf("could not create the file object store: %v", err)
		}
		return fs, []string{"bucket-a", "bucket-b"}
	})
}
