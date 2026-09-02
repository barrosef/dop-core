//go:build integration

package contract_test

// A MESMA suíte de contrato, agora contra GitHub e GitLab de VERDADE.
//
//	go test ./test/contract/ -tags=integration -count=1 -v -run GitProvider
//
// É esta execução que dá sentido ao duplo local. O duplo prova que os dois
// adaptadores concordam com a MESMA leitura da documentação; só o provedor real
// prova que a leitura estava certa. Os dois pontos em que a documentação é
// omissa — o corpo do 422 de PR duplicado no GitHub e o texto do erro de
// GraphQL num rebase conflitado — só se resolvem AQUI.
//
// Sem credencial, PULA com a receita do que falta. `ok` com tudo pulado não
// prova nada: rode com -v e confira o que executou de verdade.
//
// ── Variáveis ────────────────────────────────────────────────────────────────
//
// Somente leitura (seguras em qualquer repositório):
//
//	GITHUB_TOKEN            token com leitura do repositório
//	GITHUB_TEST_REPO        "dono/nome"
//	GITHUB_TEST_ACTOR       identificador do ator dono do token (ADR-0003)
//	GITHUB_API              opcional; default https://api.github.com
//	GITHUB_GRAPHQL          opcional; no Enterprise é .../api/graphql
//	GITHUB_TEST_REPO_COM_FILA / _SEM_FILA / _SEM_PERMISSAO_DE_REGRAS
//
//	GITLAB_TOKEN, GITLAB_TEST_PROJECT ("grupo/projeto" ou id), GITLAB_TEST_ACTOR
//	GITLAB_API              opcional; default https://gitlab.com/api/v4
//	GITLAB_TEST_PROJECT_COM_TREM / _SEM_TREM / _SEM_ESCOPO
//
// ── E as que ESCREVEM ────────────────────────────────────────────────────────
//
//	GITHUB_TEST_BRANCHES / GITLAB_TEST_BRANCHES = "origem:destino[,origem:destino…]"
//
// Elas são separadas de propósito, e ficam desligadas por padrão. Os subtestes
// que dependem delas ABREM PR E MERGEIAM — num repositório de verdade, com
// efeito de verdade. Fazer isso por default significaria que rodar a suíte
// numa máquina mal configurada mergeia coisa em produção; o repositório
// descartável tem de ser uma DECISÃO de quem roda, nunca o default.
//
// Cada par é consumido UMA vez. A suíte pede vários pares (cada subteste quer
// branches novos), e quando eles acabam os subtestes seguintes PULAM com a
// contagem — em vez de reusar um branch já mergeado e reprovar por um motivo
// que não é o do teste.

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/gitprovider"
	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// paresDe transforma "a:main,b:main" numa fábrica que entrega cada par uma vez
// só. Devolve nil quando a variável está vazia — e campo nil na env faz a suíte
// PULAR o subteste com registro, que é o comportamento certo: contra um
// provedor real não dá para fabricar branch sob demanda.
func paresDe(t *testing.T, env string) func(*testing.T) (string, string) {
	bruto := strings.TrimSpace(os.Getenv(env))
	if bruto == "" {
		return nil
	}
	var mu sync.Mutex
	var pares [][2]string
	for _, item := range strings.Split(bruto, ",") {
		p := strings.SplitN(strings.TrimSpace(item), ":", 2)
		if len(p) != 2 || p[0] == "" || p[1] == "" {
			t.Fatalf("%s malformada em %q: use \"origem:destino,origem:destino\"", env, item)
		}
		pares = append(pares, [2]string{p[0], p[1]})
	}
	usados := 0
	return func(t *testing.T) (string, string) {
		mu.Lock()
		defer mu.Unlock()
		if usados >= len(pares) {
			t.Skipf("%s ofereceu %d par(es) e todos já foram consumidos — "+
				"acrescente mais pares de branches DESCARTÁVEIS para cobrir este caso",
				env, len(pares))
		}
		p := pares[usados]
		usados++
		t.Logf("usando branches reais %s → %s (este subteste ESCREVE no repositório)", p[0], p[1])
		return p[0], p[1]
	}
}

func TestGitProviderContractGitHubReal(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	repo := os.Getenv("GITHUB_TEST_REPO")
	ator := os.Getenv("GITHUB_TEST_ACTOR")
	if token == "" || repo == "" {
		t.Skip("GITHUB_TOKEN e GITHUB_TEST_REPO (\"dono/nome\") não definidas — " +
			"sem elas não há GitHub para exercitar. Um token com leitura do " +
			"repositório já cobre os subtestes de fila nativa, ausência, " +
			"credencial e vazamento; para os que abrem PR e mergeiam, defina " +
			"também GITHUB_TEST_BRANCHES com branches DESCARTÁVEIS.")
	}
	if ator == "" {
		ator = "ator-de-teste"
	}
	base := os.Getenv("GITHUB_API")
	if base == "" {
		base = "https://api.github.com"
	}

	novo := func(a, tk string) delivery.GitProvider {
		return gitprovider.NewGitHub(gitprovider.GitHubConfig{
			APIBase: base, GraphQLURL: os.Getenv("GITHUB_GRAPHQL"),
			Token: tk, ActorID: a,
			Timeout: 30 * time.Second, RebaseTimeout: 3 * time.Minute,
		})
	}
	// Sonda barata antes da suíte: se o token não alcança o repositório, PULA
	// com o motivo em vez de deixar dezesseis subtestes falharem parecendo
	// defeito do adaptador.
	if _, err := novo(ator, token).HasNativeQueue(t.Context(), repo); err != nil {
		t.Skipf("o GitHub em %s não respondeu por %s: %v", base, repo, err)
	}

	pares := paresDe(t, "GITHUB_TEST_BRANCHES")
	if pares == nil {
		t.Log("GITHUB_TEST_BRANCHES não definida: os subtestes que ABREM PR e " +
			"MERGEIAM serão PULADOS. O que roda aqui é o caminho somente-leitura.")
	}
	contract.GitProviderSuite(t, "github-real", func(t *testing.T) contract.GitProviderEnv {
		return contract.GitProviderEnv{
			Connect:              func(t *testing.T, a string) delivery.GitProvider { return novo(a, token) },
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider { return novo(ator, "ghp_invalido_de_proposito") },
			Actor:                  ator,
			SentinelToken:        token,
			Repo:                  repo,
			InvisibleRepo:         env("GITHUB_TEST_REPO_INVISIVEL", "dop-nao-existe/repo-nao-existe-"+t.Name()),
			RepoWithNativeQueue:     os.Getenv("GITHUB_TEST_REPO_COM_FILA"),
			RepoWithoutNativeQueue:     os.Getenv("GITHUB_TEST_REPO_SEM_FILA"),
			RepoWithUnreadableQueue:      os.Getenv("GITHUB_TEST_REPO_SEM_PERMISSAO_DE_REGRAS"),
			Pair:                   pares,
			// Conflito e bloqueio de verdade exigem branches PREPARADOS (um que
			// diverge do destino, outro coberto por checagem obrigatória). Não
			// há como fabricá-los pela porta, então ficam de fora até que
			// alguém os prepare — e o subteste PULA dizendo isso.
			ConflictingPair: paresDe(t, "GITHUB_TEST_BRANCHES_CONFLITANTES"),
			BlockedPair:   paresDe(t, "GITHUB_TEST_BRANCHES_BLOQUEADAS"),
			PairWithNoCommits:  paresDe(t, "GITHUB_TEST_BRANCHES_SEM_COMMITS"),
			Wait:         3 * time.Minute,
		}
	})
}

func TestGitProviderContractGitLabReal(t *testing.T) {
	token := os.Getenv("GITLAB_TOKEN")
	proj := os.Getenv("GITLAB_TEST_PROJECT")
	ator := os.Getenv("GITLAB_TEST_ACTOR")
	if token == "" || proj == "" {
		t.Skip("GITLAB_TOKEN e GITLAB_TEST_PROJECT (\"grupo/projeto\" ou id numérico) " +
			"não definidas — sem elas não há GitLab para exercitar. Para os " +
			"subtestes que abrem MR e mergeiam, defina também GITLAB_TEST_BRANCHES " +
			"com branches DESCARTÁVEIS.")
	}
	if ator == "" {
		ator = "ator-de-teste"
	}
	base := env("GITLAB_API", "https://gitlab.com/api/v4")

	novo := func(a, tk string) delivery.GitProvider {
		return gitprovider.NewGitLab(gitprovider.GitLabConfig{
			APIBase: base, Token: tk, ActorID: a,
			UseBearer: os.Getenv("GITLAB_TOKEN_OAUTH") == "1",
			Timeout:   30 * time.Second, RebaseTimeout: 3 * time.Minute,
		})
	}
	if _, err := novo(ator, token).HasNativeQueue(t.Context(), proj); err != nil {
		t.Skipf("o GitLab em %s não respondeu por %s: %v — se a credencial for de "+
			"OAuth em vez de token pessoal, defina GITLAB_TOKEN_OAUTH=1", base, proj, err)
	}

	pares := paresDe(t, "GITLAB_TEST_BRANCHES")
	if pares == nil {
		t.Log("GITLAB_TEST_BRANCHES não definida: os subtestes que ABREM MR e " +
			"MERGEIAM serão PULADOS.")
	}
	contract.GitProviderSuite(t, "gitlab-real", func(t *testing.T) contract.GitProviderEnv {
		return contract.GitProviderEnv{
			Connect:              func(t *testing.T, a string) delivery.GitProvider { return novo(a, token) },
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider { return novo(ator, "glpat-invalido-de-proposito") },
			Actor:                  ator,
			SentinelToken:        token,
			Repo:                  proj,
			InvisibleRepo:         env("GITLAB_TEST_PROJECT_INVISIVEL", "dop-nao-existe/projeto-nao-existe"),
			RepoWithNativeQueue:     os.Getenv("GITLAB_TEST_PROJECT_COM_TREM"),
			RepoWithoutNativeQueue:     os.Getenv("GITLAB_TEST_PROJECT_SEM_TREM"),
			RepoWithUnreadableQueue:      os.Getenv("GITLAB_TEST_PROJECT_SEM_ESCOPO"),
			Pair:                   pares,
			ConflictingPair:        paresDe(t, "GITLAB_TEST_BRANCHES_CONFLITANTES"),
			BlockedPair:          paresDe(t, "GITLAB_TEST_BRANCHES_BLOQUEADAS"),
			PairWithNoCommits:         paresDe(t, "GITLAB_TEST_BRANCHES_SEM_COMMITS"),
			Wait:                3 * time.Minute,
		}
	})
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
