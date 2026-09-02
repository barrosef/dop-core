//go:build integration

package contract_test

// A MESMA suíte de contrato, agora contra GitHub e GitLab de VERDADE.
//
//	go test ./test/contract/ -tags=integration -count=1 -v -run GitProvider
//
// É esta execução que dá sentido ao duplo local. O duplo prova que os dois
// adaptadores concordam com a MESMA leitura da documentação; só o provedor real
// prova que a leitura estava certa. Os dois pontos em que a documentação é
// omissa — o body do 422 de PR duplicado no GitHub e o texto do fail de
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
//	GITHUB_TEST_BRANCHES / GITLAB_TEST_BRANCHES = "source:target[,source:target…]"
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

// pairsFrom turns "a:main,b:main" into a factory that hands each pair out once
// only. It returns nil when the variable is empty — and a nil field in the env
// makes the suite SKIP the subtest with a record, which is the right behaviour:
// against a real provider you cannot fabricate a branch on demand.
func pairsFrom(t *testing.T, env string) func(*testing.T) (string, string) {
	bruto := strings.TrimSpace(os.Getenv(env))
	if bruto == "" {
		return nil
	}
	var mu sync.Mutex
	var pares [][2]string
	for _, item := range strings.Split(bruto, ",") {
		p := strings.SplitN(strings.TrimSpace(item), ":", 2)
		if len(p) != 2 || p[0] == "" || p[1] == "" {
			t.Fatalf("%s malformada em %q: use \"source:target,source:target\"", env, item)
		}
		pares = append(pares, [2]string{p[0], p[1]})
	}
	usados := 0
	return func(t *testing.T) (string, string) {
		mu.Lock()
		defer mu.Unlock()
		if usados >= len(pares) {
			t.Skipf("%s offered %d pair(s) and all of them have been consumed — "+
				"add more DISPOSABLE branch pairs to cover this case",
				env, len(pares))
		}
		p := pares[usados]
		usados++
		t.Logf("using real branches %s → %s (this subtest WRITES to the repository)", p[0], p[1])
		return p[0], p[1]
	}
}

func TestGitProviderContractGitHubReal(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	repo := os.Getenv("GITHUB_TEST_REPO")
	ator := os.Getenv("GITHUB_TEST_ACTOR")
	if token == "" || repo == "" {
		t.Skip("GITHUB_TOKEN and GITHUB_TEST_REPO (\"owner/name\") are not set — " +
			"without them there is no GitHub to exercise. A token with read access " +
			"to the repository already covers the native-queue, absence, credential " +
			"and leak subtests; for the ones that open PRs and merge, also set " +
			"GITHUB_TEST_BRANCHES with DISPOSABLE branches.")
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
	// A cheap probe before the suite: if the token cannot reach the repository,
	// it SKIPS with the reason instead of letting sixteen subtests fail looking
	// like a defect of the adapter.
	if _, err := novo(ator, token).HasNativeQueue(t.Context(), repo); err != nil {
		t.Skipf("GitHub at %s did not answer for %s: %v", base, repo, err)
	}

	pares := pairsFrom(t, "GITHUB_TEST_BRANCHES")
	if pares == nil {
		t.Log("GITHUB_TEST_BRANCHES is not set: the subtests that OPEN PRs and " +
			"MERGE will be SKIPPED. What runs here is the read-only path.")
	}
	contract.GitProviderSuite(t, "github-real", func(t *testing.T) contract.GitProviderEnv {
		return contract.GitProviderEnv{
			Connect:                  func(t *testing.T, a string) delivery.GitProvider { return novo(a, token) },
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider { return novo(ator, "ghp_invalido_de_proposito") },
			Actor:                    ator,
			SentinelToken:            token,
			Repo:                     repo,
			InvisibleRepo:            env("GITHUB_TEST_REPO_INVISIVEL", "dop-does-not-exist/repo-does-not-exist-"+t.Name()),
			RepoWithNativeQueue:      os.Getenv("GITHUB_TEST_REPO_COM_FILA"),
			RepoWithoutNativeQueue:   os.Getenv("GITHUB_TEST_REPO_SEM_FILA"),
			RepoWithUnreadableQueue:  os.Getenv("GITHUB_TEST_REPO_SEM_PERMISSAO_DE_REGRAS"),
			Pair:                     pares,
			// A real conflict and a real block require PREPARED branches (one
			// that diverges from the target, another covered by a required
			// check). There is no way to fabricate them through the port, so
			// they stay out until somebody prepares them — and the subtest SKIPS
			// saying so.
			ConflictingPair:   pairsFrom(t, "GITHUB_TEST_BRANCHES_CONFLITANTES"),
			BlockedPair:       pairsFrom(t, "GITHUB_TEST_BRANCHES_BLOQUEADAS"),
			PairWithNoCommits: pairsFrom(t, "GITHUB_TEST_BRANCHES_SEM_COMMITS"),
			Wait:              3 * time.Minute,
		}
	})
}

func TestGitProviderContractGitLabReal(t *testing.T) {
	token := os.Getenv("GITLAB_TOKEN")
	proj := os.Getenv("GITLAB_TEST_PROJECT")
	ator := os.Getenv("GITLAB_TEST_ACTOR")
	if token == "" || proj == "" {
		t.Skip("GITLAB_TOKEN and GITLAB_TEST_PROJECT (\"group/project\" or a numeric id) " +
			"are not set — without them there is no GitLab to exercise. For the " +
			"subtests that open MRs and merge, also set GITLAB_TEST_BRANCHES " +
			"with DISPOSABLE branches.")
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
		t.Skipf("GitLab at %s did not answer for %s: %v — if the credential is an "+
			"OAuth em vez de token pessoal, defina GITLAB_TOKEN_OAUTH=1", base, proj, err)
	}

	pares := pairsFrom(t, "GITLAB_TEST_BRANCHES")
	if pares == nil {
		t.Log("GITLAB_TEST_BRANCHES is not set: the subtests that OPEN MRs and " +
			"MERGE will be SKIPPED.")
	}
	contract.GitProviderSuite(t, "gitlab-real", func(t *testing.T) contract.GitProviderEnv {
		return contract.GitProviderEnv{
			Connect:                  func(t *testing.T, a string) delivery.GitProvider { return novo(a, token) },
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider { return novo(ator, "glpat-invalido-de-proposito") },
			Actor:                    ator,
			SentinelToken:            token,
			Repo:                     proj,
			InvisibleRepo:            env("GITLAB_TEST_PROJECT_INVISIVEL", "dop-does-not-exist/project-does-not-exist"),
			RepoWithNativeQueue:      os.Getenv("GITLAB_TEST_PROJECT_COM_TREM"),
			RepoWithoutNativeQueue:   os.Getenv("GITLAB_TEST_PROJECT_SEM_TREM"),
			RepoWithUnreadableQueue:  os.Getenv("GITLAB_TEST_PROJECT_SEM_ESCOPO"),
			Pair:                     pares,
			ConflictingPair:          pairsFrom(t, "GITLAB_TEST_BRANCHES_CONFLITANTES"),
			BlockedPair:              pairsFrom(t, "GITLAB_TEST_BRANCHES_BLOQUEADAS"),
			PairWithNoCommits:        pairsFrom(t, "GITLAB_TEST_BRANCHES_SEM_COMMITS"),
			Wait:                     3 * time.Minute,
		}
	})
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
