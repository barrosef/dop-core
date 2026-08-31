//go:build integration

// A suíte de contrato do substrato contra o Docker do host.
//
//	go test ./test/contract/ -tags=integration -run TestSandboxContractDocker -v
//
// Este é o adaptador que faz o desenvolvimento da plataforma existir sem
// cluster — e é rodando ele e o do Kubernetes pela MESMA suíte que se descobre
// onde os dois divergem. Sem isso, a porta sairia no formato de quem a inspirou.
package contract_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/sandbox"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestSandboxContractDocker(t *testing.T) {
	socket := sandbox.DefaultDockerSocket
	if v := os.Getenv("DOCKER_HOST"); strings.HasPrefix(v, "unix://") {
		socket = strings.TrimPrefix(v, "unix://")
	}
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Skipf("Docker indisponível em %s: %v — suba o daemon ou aponte DOCKER_HOST", socket, err)
	}
	_ = conn.Close()

	launcher := sandbox.NewDocker(sandbox.DockerConfig{Socket: socket})
	tiers, err := launcher.SupportedTiers(context.Background())
	if err != nil {
		t.Skipf("o daemon do Docker não respondeu: %v", err)
	}
	t.Logf("níveis de isolamento oferecidos por este host: %v", tiers)

	contract.SandboxSuite(t, "docker", func(t *testing.T) (ports.SandboxLauncher, contract.SandboxEnv) {
		return launcher, contract.SandboxEnv{
			NamespacePrefix: "dop-ct",
			Image:           imagemDeTeste(),
			Tier:            ports.TierNamespace,
			// Nenhum host de desenvolvimento tem Kata registrado no Docker. É
			// justamente o caso que a garantia 1 existe para cobrir: pedir
			// microVM aqui tem de RECUSAR, não entregar um contêiner comum.
			Unsupported: ports.TierHardware,
			Ready:       90 * time.Second,
		}
	})
}

// imagemDeTeste é minúscula de propósito: a suíte precisa rodar no laptop de
// quem mexe no adaptador, não só na esteira.
func imagemDeTeste() string {
	if v := os.Getenv("DOP_SANDBOX_TEST_IMAGE"); v != "" {
		return v
	}
	return "busybox:1.36"
}
