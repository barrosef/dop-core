//go:build integration

// A MESMA suíte de contrato, agora contra o Secret Manager.
//
//	go test ./test/contract/ -tags=integration -v -run SecretStore
//
// Localmente o alvo é o emulador da comunidade (não há oficial; P-17 no
// ROADMAP). Apontando SECRET_MANAGER_EMULATOR_HOST para vazio e dando
// credencial, a MESMA função roda contra o GCP de verdade — que é o único jeito
// de descobrir as divergências listadas no cabeçalho do adaptador.
//
// ATENÇÃO ao ler um PASS aqui: o emulador é mais permissivo que o GCP real em
// nove pontos documentados em internal/adapter/secretstore/gcp.go, e um deles
// é a garantia 1 da porta — leitura-após-escrita, que o Google NÃO promete pelo
// alias `latest`. Verde aqui não é prova de verde lá.
package contract_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/secretstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestSecretStoreContractGCP(t *testing.T) {
	endpoint := os.Getenv("SECRET_MANAGER_EMULATOR_HOST")
	if endpoint == "" {
		endpoint = "127.0.0.1:8085"
	}

	// Um PROJETO por chamada de newStore. A suíte chama newStore uma vez por
	// subteste e conta com estado limpo — o subteste `exists_reflete_estado`
	// exige que a referência A não exista, e ela já foi gravada por quatro
	// subtestes anteriores. Contra um backend em memória isso é automático;
	// contra um backend REAL, o isolamento tem que vir de algum lugar, e aqui
	// vem do nome do projeto. (No GCP de verdade um projeto por subteste é
	// impraticável: ver a nota no fim deste arquivo.)
	var n int
	contract.SecretStoreSuite(t, "gcp-emulado", func(t *testing.T) ports.SecretStore {
		n++
		project := fmt.Sprintf("dop-contract-%d-%d", time.Now().UnixNano(), n)

		ctx := context.Background()
		s, err := secretstore.NewGCP(ctx, secretstore.GCPConfig{
			ProjectID: project,
			Endpoint:  endpoint,
			// Curto de propósito: contra o emulador a primeira tentativa já
			// basta, e um teto grande transformaria "emulador mudo" em quatro
			// minutos de espera antes do erro.
			Propagation: 5 * time.Second,
		})
		if err != nil {
			t.Skipf("Secret Manager indisponível em %s: %v", endpoint, err)
		}
		t.Cleanup(func() { _ = s.Close() })

		// O cliente gRPC conecta preguiçosamente: NewGCP não falha com o
		// emulador desligado. Uma chamada de verdade é o que descobre isso — e
		// sem esta sonda o teste morreria com um erro de rede no meio de um
		// subteste, em vez de pular com mensagem clara.
		//
		// O PULO É ESTREITO DE PROPÓSITO: só KindUnavailable (não respondeu)
		// vira Skip. Qualquer OUTRO erro é falha do adaptador e precisa
		// REPROVAR. A primeira versão desta sonda pulava com erro nenhum, e o
		// experimento de quebra provou o estrago: ao quebrar a garantia 2 de
		// propósito — Get de referência ausente devolvendo erro — a suíte
		// inteira PULOU e o `go test` imprimiu "ok". Um teste que se desliga
		// justamente quando o código quebra é pior que teste nenhum.
		probe := ports.SecretRef{AccountID: "sonda", Kind: "integration_credential", OwnerID: "sonda"}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		switch _, err := s.Get(pctx, probe); {
		case err == nil:
		case errs.KindOf(err) == errs.KindUnavailable:
			t.Skipf("emulador do Secret Manager não respondeu em %s: %v\n"+
				"Suba o ambiente local (dop-infra: make up) e faça\n"+
				"  kubectl port-forward -n dop-local svc/secretmanager 8085:9090",
				endpoint, err)
		default:
			t.Fatalf("o adaptador falhou na sonda com erro que NÃO é de "+
				"indisponibilidade — isto é defeito, não ambiente ausente: %v", err)
		}
		return s
	})
}

// Por que não há um teste equivalente contra o GCP REAL neste arquivo, e o que
// mudaria se houvesse — está aqui para não ser redescoberto do zero:
//
//   - a suíte grava a MESMA referência várias vezes em sequência. O GCP real
//     limita AddSecretVersion a 2 qps / 120 qpm POR SEGREDO e
//     DestroySecretVersion a 1 qps POR VERSÃO. A suíte esbarra nisso;
//   - a suíte apaga e recria o mesmo nome (subteste 3 seguido do 4). No GCP
//     real DeleteSecret é irreversível e imediato, mas os metadados são
//     eventualmente consistentes: recriar em seguida pode dar AlreadyExists;
//   - o subteste 1 exige leitura-após-escrita imediata. O Google só promete
//     isso para acesso POR NÚMERO de versão, e a porta não tem onde guardar
//     número. É a descoberta de arquitetura registrada no adaptador.
//
// Ou seja: esta suíte, como está, passa no emulador e NÃO é confiável contra o
// GCP real. Consertar isso é mudar a porta, não o teste.
//
// E o que a suíte NÃO cobre, descoberto quebrando garantias de propósito para
// ver o que ela pega:
//
//   - QUE O Put DESTRUA O VALOR ANTERIOR. Removendo a destruição das versões
//     antigas do adaptador do GCP, os sete subtestes continuam VERDES: o
//     subteste 4 confere que o Get devolve o valor novo, e não que o antigo
//     deixou de ser legível. No GCP real a versão antiga continuaria acessível
//     por número — e "rotacionei a credencial vazada" passaria a significar
//     coisas diferentes em cada adaptador. Cobrir isso exige a porta expor algo
//     que ela hoje esconde, ou o teste conhecer o adaptador;
//
//   - ISOLAMENTO POR IAM. Aqui a garantia 5 só é exercitada na metade que vive
//     no NOME. A outra metade, em produção, é a política de IAM da conta de
//     serviço — e o emulador não tem controle de acesso nenhum.
