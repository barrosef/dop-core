//go:build integration

// A MESMA suíte de contrato, contra a API do Kubernetes de verdade.
//
//	go test ./test/contract/ -tags=integration -v -run SecretStore
//
// Este arquivo estava PROMETIDO num comentário de `secretstore_test.go` e não
// existia: o adaptador k8s — que é o de produção do ambiente self-hosted, e
// cujo cabeçalho afirma ser "exercitado TODO DIA" — nunca havia passado pela
// suíte. Só o duplo em memória passava, o que provava que o duplo é consistente
// consigo mesmo.
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
		// `kubectl proxy --port=8001` é o caminho de menor atrito fora do
		// cluster: ele já resolve autenticação e TLS.
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
		// Sonda barata: se a API não responde, PULA com motivo — em vez de
		// deixar cada subteste falhar por indisponibilidade e parecer defeito
		// do adaptador.
		if _, err := k.Exists(t.Context(), ports.SecretRef{
			AccountID: "sonda", Kind: "sonda", OwnerID: "sonda",
		}); err != nil {
			t.Skipf("API do Kubernetes indisponível em %s: %v", api, err)
		}
		return k
	})
}
