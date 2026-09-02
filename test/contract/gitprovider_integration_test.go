//go:build integration

package contract_test

// THE SAME contract suite, now against a REAL GitHub and a REAL GitLab.
//
//	go test ./test/contract/ -tags=integration -count=1 -v -run GitProvider
//
// It is this run that gives the local double its meaning. The double proves both
// adapters agree with the SAME reading of the documentation; only the real
// provider proves the reading was right. The two points where the documentation
// is silent — the body of GitHub's 422 for a duplicate PR and the text of the
// GraphQL error on a conflicted rebase — are only settled HERE.
//
// With no credential it SKIPS with the recipe of what is missing. An `ok` with
// everything skipped proves nothing: run with -v and check what actually ran.
//
// ── Variables ────────────────────────────────────────────────────────────────
//
// Read-only (safe in any repository):
//
//	GITHUB_TOKEN            a token with read access to the repository
//	GITHUB_TEST_REPO        "owner/name"
//	GITHUB_TEST_ACTOR       the identifier of the actor owning the token (ADR-0003)
//	GITHUB_API              optional; default https://api.github.com
//	GITHUB_GRAPHQL          optional; on Enterprise it is .../api/graphql
//	GITHUB_TEST_REPO_WITH_QUEUE / _WITHOUT_QUEUE / _WITHOUT_RULES_PERMISSION
//
//	GITLAB_TOKEN, GITLAB_TEST_PROJECT ("group/project" or an id), GITLAB_TEST_ACTOR
//	GITLAB_API              optional; default https://gitlab.com/api/v4
//	GITLAB_TEST_PROJECT_WITH_TRAIN / _WITHOUT_TRAIN / _WITHOUT_SCOPE
//
// ── And the ones that WRITE ──────────────────────────────────────────────────
//
//	GITHUB_TEST_BRANCHES / GITLAB_TEST_BRANCHES = "source:target[,source:target…]"
//
// They are separate on purpose, and off by default. The subtests that depend on
// them OPEN PRs AND MERGE — in a real repository, with real effect. Doing that
// by default would mean that running the suite on a badly configured machine
// merges something in production; the disposable repository has to be a
// DECISION of whoever runs it, never the default.
//
// Each pair is consumed ONCE. The suite asks for several pairs (each subtest
// wants fresh branches), and when they run out the following subtests SKIP with
// the count — instead of reusing an already merged branch and failing for a
// reason that is not the test's.

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
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return nil
	}
	var mu sync.Mutex
	var pairs [][2]string
	for _, item := range strings.Split(raw, ",") {
		p := strings.SplitN(strings.TrimSpace(item), ":", 2)
		if len(p) != 2 || p[0] == "" || p[1] == "" {
			t.Fatalf("%s is malformed at %q: use \"source:target,source:target\"", env, item)
		}
		pairs = append(pairs, [2]string{p[0], p[1]})
	}
	used := 0
	return func(t *testing.T) (string, string) {
		mu.Lock()
		defer mu.Unlock()
		if used >= len(pairs) {
			t.Skipf("%s offered %d pair(s) and all of them have been consumed — "+
				"add more DISPOSABLE branch pairs to cover this case",
				env, len(pairs))
		}
		p := pairs[used]
		used++
		t.Logf("using real branches %s → %s (this subtest WRITES to the repository)", p[0], p[1])
		return p[0], p[1]
	}
}

func TestGitProviderContractGitHubReal(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	repo := os.Getenv("GITHUB_TEST_REPO")
	actor := os.Getenv("GITHUB_TEST_ACTOR")
	if token == "" || repo == "" {
		t.Skip("GITHUB_TOKEN and GITHUB_TEST_REPO (\"owner/name\") are not set — " +
			"without them there is no GitHub to exercise. A token with read access " +
			"to the repository already covers the native-queue, absence, credential " +
			"and leak subtests; for the ones that open PRs and merge, also set " +
			"GITHUB_TEST_BRANCHES with DISPOSABLE branches.")
	}
	if actor == "" {
		actor = "test-actor"
	}
	base := os.Getenv("GITHUB_API")
	if base == "" {
		base = "https://api.github.com"
	}

	build := func(a, tk string) delivery.GitProvider {
		return gitprovider.NewGitHub(gitprovider.GitHubConfig{
			APIBase: base, GraphQLURL: os.Getenv("GITHUB_GRAPHQL"),
			Token: tk, ActorID: a,
			Timeout: 30 * time.Second, RebaseTimeout: 3 * time.Minute,
		})
	}
	// A cheap probe before the suite: if the token cannot reach the repository,
	// it SKIPS with the reason instead of letting sixteen subtests fail looking
	// like a defect of the adapter.
	if _, err := build(actor, token).HasNativeQueue(t.Context(), repo); err != nil {
		t.Skipf("GitHub at %s did not answer for %s: %v", base, repo, err)
	}

	pairs := pairsFrom(t, "GITHUB_TEST_BRANCHES")
	if pairs == nil {
		t.Log("GITHUB_TEST_BRANCHES is not set: the subtests that OPEN PRs and " +
			"MERGE will be SKIPPED. What runs here is the read-only path.")
	}
	contract.GitProviderSuite(t, "github-real", func(t *testing.T) contract.GitProviderEnv {
		return contract.GitProviderEnv{
			Connect:                  func(t *testing.T, a string) delivery.GitProvider { return build(a, token) },
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider { return build(actor, "ghp_invalid_on_purpose") },
			Actor:                    actor,
			SentinelToken:            token,
			Repo:                     repo,
			InvisibleRepo:            env("GITHUB_TEST_REPO_INVISIBLE", "dop-does-not-exist/repo-does-not-exist-"+t.Name()),
			RepoWithNativeQueue:      os.Getenv("GITHUB_TEST_REPO_WITH_QUEUE"),
			RepoWithoutNativeQueue:   os.Getenv("GITHUB_TEST_REPO_WITHOUT_QUEUE"),
			RepoWithUnreadableQueue:  os.Getenv("GITHUB_TEST_REPO_WITHOUT_RULES_PERMISSION"),
			Pair:                     pairs,
			// A real conflict and a real block require PREPARED branches (one
			// that diverges from the target, another covered by a required
			// check). There is no way to fabricate them through the port, so
			// they stay out until somebody prepares them — and the subtest SKIPS
			// saying so.
			ConflictingPair:   pairsFrom(t, "GITHUB_TEST_BRANCHES_CONFLICTING"),
			BlockedPair:       pairsFrom(t, "GITHUB_TEST_BRANCHES_BLOCKED"),
			PairWithNoCommits: pairsFrom(t, "GITHUB_TEST_BRANCHES_WITHOUT_COMMITS"),
			Wait:              3 * time.Minute,
		}
	})
}

func TestGitProviderContractGitLabReal(t *testing.T) {
	token := os.Getenv("GITLAB_TOKEN")
	proj := os.Getenv("GITLAB_TEST_PROJECT")
	actor := os.Getenv("GITLAB_TEST_ACTOR")
	if token == "" || proj == "" {
		t.Skip("GITLAB_TOKEN and GITLAB_TEST_PROJECT (\"group/project\" or a numeric id) " +
			"are not set — without them there is no GitLab to exercise. For the " +
			"subtests that open MRs and merge, also set GITLAB_TEST_BRANCHES " +
			"with DISPOSABLE branches.")
	}
	if actor == "" {
		actor = "test-actor"
	}
	base := env("GITLAB_API", "https://gitlab.com/api/v4")

	build := func(a, tk string) delivery.GitProvider {
		return gitprovider.NewGitLab(gitprovider.GitLabConfig{
			APIBase: base, Token: tk, ActorID: a,
			UseBearer: os.Getenv("GITLAB_TOKEN_OAUTH") == "1",
			Timeout:   30 * time.Second, RebaseTimeout: 3 * time.Minute,
		})
	}
	if _, err := build(actor, token).HasNativeQueue(t.Context(), proj); err != nil {
		t.Skipf("GitLab at %s did not answer for %s: %v — if the credential is an "+
			"OAuth token rather than a personal one, set GITLAB_TOKEN_OAUTH=1", base, proj, err)
	}

	pairs := pairsFrom(t, "GITLAB_TEST_BRANCHES")
	if pairs == nil {
		t.Log("GITLAB_TEST_BRANCHES is not set: the subtests that OPEN MRs and " +
			"MERGE will be SKIPPED.")
	}
	contract.GitProviderSuite(t, "gitlab-real", func(t *testing.T) contract.GitProviderEnv {
		return contract.GitProviderEnv{
			Connect:                  func(t *testing.T, a string) delivery.GitProvider { return build(a, token) },
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider { return build(actor, "glpat-invalid-on-purpose") },
			Actor:                    actor,
			SentinelToken:            token,
			Repo:                     proj,
			InvisibleRepo:            env("GITLAB_TEST_PROJECT_INVISIBLE", "dop-does-not-exist/project-does-not-exist"),
			RepoWithNativeQueue:      os.Getenv("GITLAB_TEST_PROJECT_WITH_TRAIN"),
			RepoWithoutNativeQueue:   os.Getenv("GITLAB_TEST_PROJECT_WITHOUT_TRAIN"),
			RepoWithUnreadableQueue:  os.Getenv("GITLAB_TEST_PROJECT_WITHOUT_SCOPE"),
			Pair:                     pairs,
			ConflictingPair:          pairsFrom(t, "GITLAB_TEST_BRANCHES_CONFLICTING"),
			BlockedPair:              pairsFrom(t, "GITLAB_TEST_BRANCHES_BLOCKED"),
			PairWithNoCommits:        pairsFrom(t, "GITLAB_TEST_BRANCHES_WITHOUT_COMMITS"),
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
