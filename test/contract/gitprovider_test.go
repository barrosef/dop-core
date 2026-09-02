package contract_test

// A suíte de contrato do GitProvider contra os DOIS duplos locais.
//
//	go test ./test/contract/ -run GitProvider -v
//
// Roda sempre, sem infra e sem token. É a única forma de a suíte existir de
// verdade: nem a esteira nem o laptop de quem mexe no adaptador têm GitHub ou
// GitLab, e uma suíte que só roda com credencial de produção é uma suíte que
// não roda (ver o cabeçalho dos duplos para o limite do que eles provam, e
// gitprovider_integration_test.go para o caminho contra o provedor real).

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/gitprovider"
	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// tokenFalso é a SENTINELA da garantia 12. Precisa ser uma sequência
// improvável e reconhecível: a suíte varre toda mensagem de erro atrás dela.
const tokenFalso = "ghp-SENTINELA-NAO-PODE-APARECER-EM-LUGAR-NENHUM-0001"

const atorDeTeste = "usr-ana"

var branchSeq atomic.Int64

// branches devolve um par novo a cada chamada. Novo a cada chamada é requisito
// da suíte, não conveniência: com nomes fixos, o subteste de idempotência
// encontraria o PR deixado pelo subteste anterior e passaria por acidente.
func branches(marca string) (string, string) {
	n := branchSeq.Add(1)
	if marca != "" {
		marca = marca + "-"
	}
	return fmt.Sprintf("demanda/%s%d", marca, n), "main"
}

func TestGitProviderContractGitHub(t *testing.T) {
	f := contract.NewGitHubFake(t, tokenFalso)
	contract.GitProviderSuite(t, "github", func(t *testing.T) contract.GitProviderEnv {
		novo := func(t *testing.T, ator, token string) delivery.GitProvider {
			return gitprovider.NewGitHub(gitprovider.GitHubConfig{
				APIBase:    f.URL(),
				GraphQLURL: f.GraphQLURL(),
				Token:      token,
				ActorID:    ator,
				// Prazos curtos: contra um duplo, esperar é só desperdiçar
				// tempo de esteira. Contra o provedor real eles são os do
				// adaptador.
				RebaseTimeout: 5 * time.Second,
				Poll:          time.Millisecond,
			})
		}
		return contract.GitProviderEnv{
			Connect: func(t *testing.T, ator string) delivery.GitProvider {
				return novo(t, ator, tokenFalso)
			},
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider {
				return novo(t, atorDeTeste, "token-que-nao-serve")
			},
			Actor:              atorDeTeste,
			SentinelToken:    tokenFalso,
			Repo:              contract.GHRepoOK,
			InvisibleRepo:     contract.GHRepoInvisivel,
			RepoWithNativeQueue: contract.GHRepoComFila,
			RepoWithoutNativeQueue: contract.GHRepoSemFila,
			RepoWithUnreadableQueue:  contract.GHRepoFilaProibida,
			Pair:               func(t *testing.T) (string, string) { return branches("") },
			ConflictingPair:    func(t *testing.T) (string, string) { return branches(contract.MarcaConflito) },
			BlockedPair:      func(t *testing.T) (string, string) { return branches(contract.MarcaBloqueado) },
			PairWithNoCommits:     func(t *testing.T) (string, string) { return branches(contract.MarcaSemCommit) },
			Wait:            10 * time.Second,
		}
	})
}

func TestGitProviderContractGitLab(t *testing.T) {
	f := contract.NewGitLabFake(t, tokenFalso)
	contract.GitProviderSuite(t, "gitlab", func(t *testing.T) contract.GitProviderEnv {
		novo := func(ator, token string) delivery.GitProvider {
			return gitprovider.NewGitLab(gitprovider.GitLabConfig{
				APIBase:       f.URL(),
				Token:         token,
				ActorID:       ator,
				RebaseTimeout: 5 * time.Second,
				Poll:          time.Millisecond,
			})
		}
		return contract.GitProviderEnv{
			Connect: func(t *testing.T, ator string) delivery.GitProvider {
				return novo(ator, tokenFalso)
			},
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider {
				return novo(atorDeTeste, "token-que-nao-serve")
			},
			Actor:              atorDeTeste,
			SentinelToken:    tokenFalso,
			Repo:              contract.GLProjOK,
			InvisibleRepo:     contract.GLProjInvisivel,
			RepoWithNativeQueue: contract.GLProjComTrem,
			RepoWithoutNativeQueue: contract.GLProjSemTrem,
			RepoWithUnreadableQueue:  contract.GLProjEscopoRuim,
			Pair:               func(t *testing.T) (string, string) { return branches("") },
			ConflictingPair:    func(t *testing.T) (string, string) { return branches(contract.MarcaConflito) },
			BlockedPair:      func(t *testing.T) (string, string) { return branches(contract.MarcaBloqueado) },
			PairWithNoCommits:     func(t *testing.T) (string, string) { return branches(contract.MarcaSemCommit) },
			Wait:            10 * time.Second,
		}
	})
}

// ── verificações que SÓ o duplo permite ──────────────────────────────────────
//
// Estas não estão na suíte compartilhada porque afirmam sobre o que foi ENVIADO
// ao provedor, e não sobre o que a porta devolveu. Contra um GitHub real não há
// como observar isso — mas é justamente aqui que mora a metade não observável
// da garantia 4 ("reabrir não reescreve") e da garantia 15 ("perguntar pela
// fila é leitura").

func TestGitProviderGitHubNaoReescreveNemMuda(t *testing.T) {
	f := contract.NewGitHubFake(t, tokenFalso)
	p := gitprovider.NewGitHub(gitprovider.GitHubConfig{
		APIBase: f.URL(), GraphQLURL: f.GraphQLURL(),
		Token: tokenFalso, ActorID: atorDeTeste, Poll: time.Millisecond,
	})
	origem, destino := branches("")
	ctx := context.Background()
	spec := delivery.OpenPRSpec{
		RepoExternalID: contract.GHRepoOK, SourceBranch: origem, TargetBranch: destino,
		Title: "original", Body: "PACOTE DE EVIDÊNCIA ORIGINAL", ActorID: atorDeTeste}
	if _, err := p.OpenPullRequest(ctx, spec); err != nil {
		t.Fatalf("1ª abertura: %v", err)
	}
	spec.Title, spec.Body = "reescrito", "corpo de um retry"
	if _, err := p.OpenPullRequest(ctx, spec); err != nil {
		t.Fatalf("2ª abertura: %v", err)
	}

	// Garantia 4, a metade que a porta não mostra: NENHUM pedido de atualização
	// saiu. Se a idempotência fosse "sobrescreve", o pacote de evidência da
	// ADR-0007 §4 teria sido trocado por um corpo de retry.
	for _, c := range f.Chamadas() {
		if strings.HasPrefix(c, "PATCH ") || strings.HasPrefix(c, "PUT ") {
			t.Errorf("a reabertura enviou um pedido de MUTAÇÃO ao GitHub: %q", c)
		}
	}
}

func TestGitProviderFilaNativaSoLe(t *testing.T) {
	// Garantia 15: perguntar se existe fila nativa não pode CRIAR nem
	// CONFIGURAR nada. É verificável de um jeito só — olhando os métodos HTTP.
	t.Run("github", func(t *testing.T) {
		f := contract.NewGitHubFake(t, tokenFalso)
		p := gitprovider.NewGitHub(gitprovider.GitHubConfig{
			APIBase: f.URL(), GraphQLURL: f.GraphQLURL(), Token: tokenFalso, ActorID: atorDeTeste})
		if _, err := p.HasNativeQueue(context.Background(), contract.GHRepoComFila); err != nil {
			t.Fatalf("HasNativeQueue: %v", err)
		}
		exigirSoLeitura(t, f.Chamadas())
	})
	t.Run("gitlab", func(t *testing.T) {
		f := contract.NewGitLabFake(t, tokenFalso)
		p := gitprovider.NewGitLab(gitprovider.GitLabConfig{
			APIBase: f.URL(), Token: tokenFalso, ActorID: atorDeTeste})
		if _, err := p.HasNativeQueue(context.Background(), contract.GLProjComTrem); err != nil {
			t.Fatalf("HasNativeQueue: %v", err)
		}
		exigirSoLeitura(t, f.Chamadas())
	})
}

func exigirSoLeitura(t *testing.T, chamadas []string) {
	t.Helper()
	if len(chamadas) == 0 {
		t.Fatal("nenhuma chamada registrada: a verificação não verificou nada")
	}
	for _, c := range chamadas {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("perguntar pela fila nativa não é leitura: %q", c)
		}
	}
	t.Logf("%d chamada(s), todas de leitura", len(chamadas))
}

// TestGitProviderRebaseSemPR prova a recusa que a descoberta sobre `RebaseSpec`
// exige: nenhum dos dois provedores reaplica um branch solto.
func TestGitProviderRebaseSemPR(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		f := contract.NewGitHubFake(t, tokenFalso)
		p := gitprovider.NewGitHub(gitprovider.GitHubConfig{
			APIBase: f.URL(), GraphQLURL: f.GraphQLURL(), Token: tokenFalso,
			ActorID: atorDeTeste, Poll: time.Millisecond})
		origem, destino := branches("virgem")
		contract.GitProviderRebaseSemPR(t, p,
			contract.GitProviderEnv{Repo: contract.GHRepoOK, Actor: atorDeTeste}, origem, destino)
	})
	t.Run("gitlab", func(t *testing.T) {
		f := contract.NewGitLabFake(t, tokenFalso)
		p := gitprovider.NewGitLab(gitprovider.GitLabConfig{
			APIBase: f.URL(), Token: tokenFalso, ActorID: atorDeTeste, Poll: time.Millisecond})
		origem, destino := branches("virgem")
		contract.GitProviderRebaseSemPR(t, p,
			contract.GitProviderEnv{Repo: contract.GLProjOK, Actor: atorDeTeste}, origem, destino)
	})
}

// TestGitProviderGitLabTriEstadoDoTrem isola a sutileza que quase passou: numa
// instalação sem licença de merge pipelines a chave `merge_trains_enabled` NÃO
// VEM `false` — ela SOME da resposta. Um cliente que desserializasse em `bool`
// leria `false` nos dois casos e nunca saberia a diferença.
func TestGitProviderGitLabTriEstadoDoTrem(t *testing.T) {
	f := contract.NewGitLabFake(t, tokenFalso)
	p := gitprovider.NewGitLab(gitprovider.GitLabConfig{
		APIBase: f.URL(), Token: tokenFalso, ActorID: atorDeTeste})
	casos := []struct {
		proj string
		quer bool
	}{
		{contract.GLProjComTrem, true},
		{contract.GLProjSemTrem, false},
		// Chave ausente: a instalação não oferece merge train nenhum, então
		// não há o que duplicar. É a única inferência do adaptador, e ela erra
		// para o lado de MANTER a fila do DOP.
		{contract.GLProjSemLicenca, false},
	}
	for _, c := range casos {
		got, err := p.HasNativeQueue(context.Background(), c.proj)
		if err != nil {
			t.Fatalf("%s: %v", c.proj, err)
		}
		if got != c.quer {
			t.Errorf("%s: esperava %v, veio %v", c.proj, c.quer, got)
		}
	}
	// E o caso em que NÃO dá para olhar continua sendo erro (garantia 14).
	if ok, err := p.HasNativeQueue(context.Background(), contract.GLProjEscopoRuim); err == nil || ok {
		t.Errorf("escopo insuficiente devolveu (%v, %v) em vez de erro", ok, err)
	} else if k := errs.KindOf(err); k != errs.KindPermission {
		t.Errorf("esperava %s, veio %s: %v", errs.KindPermission, k, err)
	}
}
