package secretstore

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// A garantia 5 da porta: referência da conta A jamais resolve segredo da conta
// B. O NOME é o que sustenta isso — então referências distintas precisam
// produzir Secrets distintos.
func TestNomeNaoColideEntreContas(t *testing.T) {
	k := &K8s{}
	a := k.secretName(ports.SecretRef{AccountID: "a-b", Kind: "c", OwnerID: "d"})
	b := k.secretName(ports.SecretRef{AccountID: "a", Kind: "b-c", OwnerID: "d"})
	t.Logf("A = %s", a)
	t.Logf("B = %s", b)
	if a == b {
		t.Fatalf("COLISÃO: referências de contas diferentes dão o mesmo Secret: %q", a)
	}
}
