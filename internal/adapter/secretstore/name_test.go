package secretstore

import (
	"testing"

	"github.com/barrosef/dop-core/internal/domain/ports"
)

// The port's guarantee 5: account A's reference never resolves account B's
// secret. The NAME is what holds that up — so distinct references have to
// produce distinct Secrets.
func TestNameDoesNotCollideBetweenAccounts(t *testing.T) {
	k := &K8s{}
	a := k.secretName(ports.SecretRef{AccountID: "a-b", Kind: "c", OwnerID: "d"})
	b := k.secretName(ports.SecretRef{AccountID: "a", Kind: "b-c", OwnerID: "d"})
	t.Logf("A = %s", a)
	t.Logf("B = %s", b)
	if a == b {
		t.Fatalf("COLLISION: references from different accounts give the same Secret: %q", a)
	}
}
