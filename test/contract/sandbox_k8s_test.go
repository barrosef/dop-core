//go:build integration

// A MESMA suíte de contrato, agora contra o Kubernetes de verdade.
//
//	kubectl proxy --port=8001 &
//	go test ./test/contract/ -tags=integration -run TestSandboxContractK8s -v
//
// É esta execução que dá sentido à regra dos dois adaptadores: rodar a suíte só
// contra o Docker provaria que o Docker é consistente consigo mesmo. As duas
// implementações não têm uma linha em comum — namespace e PVC de um lado,
// contêiner e volume do outro — e é exatamente por isso que passar nas mesmas
// treze verificações significa alguma coisa.
package contract_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/sandbox"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestSandboxContractK8s(t *testing.T) {
	api := os.Getenv("K8S_API_SERVER")
	if api == "" {
		// `kubectl proxy` é o caminho de menor atrito fora do cluster: ele já
		// resolve CA e credencial a partir do kubeconfig, e o adaptador
		// continua falando a MESMA API que falaria de dentro de um pod.
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
		t.Skipf("Kubernetes indisponível em %s: %v — rode `kubectl proxy --port=8001` "+
			"ou aponte K8S_API_SERVER/K8S_TOKEN para o cluster", api, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Skipf("a API do Kubernetes em %s respondeu HTTP %d — credencial ausente ou sem permissão",
			api, resp.StatusCode)
	}

	tiers, err := launcher.SupportedTiers(context.Background())
	if err != nil {
		t.Skipf("não foi possível descobrir os níveis de isolamento do cluster: %v", err)
	}
	t.Logf("níveis de isolamento oferecidos por este cluster: %v", tiers)

	contract.SandboxSuite(t, "kubernetes", func(t *testing.T) (ports.SandboxLauncher, contract.SandboxEnv) {
		return launcher, contract.SandboxEnv{
			NamespacePrefix: "dop-ct",
			Image:           imagemDeTeste(),
			Tier:            ports.TierNamespace,
			// k3d não traz RuntimeClass de Kata. Pedir isolamento de hardware
			// aqui precisa RECUSAR — é o R-4 da spec do substrato, e o caso em
			// que degradar em silêncio custaria caro em produção.
			Unsupported: ports.TierHardware,
			// Um pod puxa imagem e espera o provisionador de volume; o
			// contêiner local não faz nem uma coisa nem outra.
			Ready: 180 * time.Second,
		}
	})
}
