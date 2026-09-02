package contract_test

// The GitProvider's contract suite against BOTH local doubles.
//
//	go test ./test/contract/ -run GitProvider -v
//
// It always runs, with no infrastructure and no token. It is the only way for
// the suite to really exist: neither CI nor the laptop of whoever touches the
// adapter has GitHub or GitLab, and a suite that only runs with a production
// credential is a suite that does not run (see the doubles' header for the limit
// of what they prove, and gitprovider_integration_test.go for the path against
// the real provider).

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

// fakeToken is guarantee 12's SENTINEL. It has to be an improbable and
// recognizable sequence: the suite sweeps every error message looking for it.
const fakeToken = "ghp-SENTINEL-MUST-NOT-APPEAR-ANYWHERE-0001"

const testActor = "usr-ana"

var branchSeq atomic.Int64

// branches returns a new pair on every call. New on every call is a requirement
// of the suite, not a convenience: with fixed names, the idempotency subtest
// would find the PR left by the previous subtest and pass by accident.
func branches(marker string) (string, string) {
	n := branchSeq.Add(1)
	if marker != "" {
		marker = marker + "-"
	}
	return fmt.Sprintf("demand/%s%d", marker, n), "main"
}

func TestGitProviderContractGitHub(t *testing.T) {
	f := contract.NewGitHubFake(t, fakeToken)
	contract.GitProviderSuite(t, "github", func(t *testing.T) contract.GitProviderEnv {
		newConn := func(t *testing.T, ator, token string) delivery.GitProvider {
			return gitprovider.NewGitHub(gitprovider.GitHubConfig{
				APIBase:    f.URL(),
				GraphQLURL: f.GraphQLURL(),
				Token:      token,
				ActorID:    ator,
				// Short deadlines: against a double, waiting only wastes CI
				// time. Against the real provider they are the adapter's.
				RebaseTimeout: 5 * time.Second,
				Poll:          time.Millisecond,
			})
		}
		return contract.GitProviderEnv{
			Connect: func(t *testing.T, ator string) delivery.GitProvider {
				return newConn(t, ator, fakeToken)
			},
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider {
				return newConn(t, testActor, "a-token-that-does-not-work")
			},
			Actor:                   testActor,
			SentinelToken:           fakeToken,
			Repo:                    contract.GHRepoOK,
			InvisibleRepo:           contract.GHRepoInvisible,
			RepoWithNativeQueue:     contract.GHRepoWithQueue,
			RepoWithoutNativeQueue:  contract.GHRepoWithoutQueue,
			RepoWithUnreadableQueue: contract.GHRepoQueueForbidden,
			Pair:                    func(t *testing.T) (string, string) { return branches("") },
			ConflictingPair:         func(t *testing.T) (string, string) { return branches(contract.MarkConflict) },
			BlockedPair:             func(t *testing.T) (string, string) { return branches(contract.MarkBlocked) },
			PairWithNoCommits:       func(t *testing.T) (string, string) { return branches(contract.MarkNoCommits) },
			Wait:                    10 * time.Second,
		}
	})
}

func TestGitProviderContractGitLab(t *testing.T) {
	f := contract.NewGitLabFake(t, fakeToken)
	contract.GitProviderSuite(t, "gitlab", func(t *testing.T) contract.GitProviderEnv {
		newConn := func(ator, token string) delivery.GitProvider {
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
				return newConn(ator, fakeToken)
			},
			ConnectWithoutCredential: func(t *testing.T) delivery.GitProvider {
				return newConn(testActor, "a-token-that-does-not-work")
			},
			Actor:                   testActor,
			SentinelToken:           fakeToken,
			Repo:                    contract.GLProjOK,
			InvisibleRepo:           contract.GLProjInvisible,
			RepoWithNativeQueue:     contract.GLProjWithTrain,
			RepoWithoutNativeQueue:  contract.GLProjWithoutTrain,
			RepoWithUnreadableQueue: contract.GLProjBadScope,
			Pair:                    func(t *testing.T) (string, string) { return branches("") },
			ConflictingPair:         func(t *testing.T) (string, string) { return branches(contract.MarkConflict) },
			BlockedPair:             func(t *testing.T) (string, string) { return branches(contract.MarkBlocked) },
			PairWithNoCommits:       func(t *testing.T) (string, string) { return branches(contract.MarkNoCommits) },
			Wait:                    10 * time.Second,
		}
	})
}

// ── checks only the double allows ────────────────────────────────────────────
//
// These are not in the shared suite because they assert about what was SENT to
// the provider, and not about what the port returned. Against a real GitHub
// there is no way to observe that — but it is exactly here that the unobservable
// half of guarantee 4 ("reopening does not rewrite") and of guarantee 15 ("asking
// about the queue is a read") lives.

func TestGitProviderGitHubNeitherRewritesNorChanges(t *testing.T) {
	f := contract.NewGitHubFake(t, fakeToken)
	p := gitprovider.NewGitHub(gitprovider.GitHubConfig{
		APIBase: f.URL(), GraphQLURL: f.GraphQLURL(),
		Token: fakeToken, ActorID: testActor, Poll: time.Millisecond,
	})
	source, target := branches("")
	ctx := context.Background()
	spec := delivery.OpenPRSpec{
		RepoExternalID: contract.GHRepoOK, SourceBranch: source, TargetBranch: target,
		Title: "original", Body: "THE ORIGINAL EVIDENCE PACKAGE", ActorID: testActor}
	if _, err := p.OpenPullRequest(ctx, spec); err != nil {
		t.Fatalf("1ª abertura: %v", err)
	}
	spec.Title, spec.Body = "reescrito", "body de um retry"
	if _, err := p.OpenPullRequest(ctx, spec); err != nil {
		t.Fatalf("2ª abertura: %v", err)
	}

	// Guarantee 4, the half the port does not show: NO update request went out.
	// If the idempotency were "it overwrites", ADR-0007 §4's evidence package
	// would have been traded for a retry's body.
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "PATCH ") || strings.HasPrefix(c, "PUT ") {
			t.Errorf("the reopening sent a MUTATION request to GitHub: %q", c)
		}
	}
}

func TestGitProviderTheNativeQueueOnlyReads(t *testing.T) {
	// Guarantee 15: asking whether a native queue exists must neither CREATE nor
	// CONFIGURE anything. It is verifiable in one way only — by looking at the
	// HTTP methods.
	t.Run("github", func(t *testing.T) {
		f := contract.NewGitHubFake(t, fakeToken)
		p := gitprovider.NewGitHub(gitprovider.GitHubConfig{
			APIBase: f.URL(), GraphQLURL: f.GraphQLURL(), Token: fakeToken, ActorID: testActor})
		if _, err := p.HasNativeQueue(context.Background(), contract.GHRepoWithQueue); err != nil {
			t.Fatalf("HasNativeQueue: %v", err)
		}
		exigirSoLeitura(t, f.Calls())
	})
	t.Run("gitlab", func(t *testing.T) {
		f := contract.NewGitLabFake(t, fakeToken)
		p := gitprovider.NewGitLab(gitprovider.GitLabConfig{
			APIBase: f.URL(), Token: fakeToken, ActorID: testActor})
		if _, err := p.HasNativeQueue(context.Background(), contract.GLProjWithTrain); err != nil {
			t.Fatalf("HasNativeQueue: %v", err)
		}
		exigirSoLeitura(t, f.Calls())
	})
}

func exigirSoLeitura(t *testing.T, calls []string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("no call recorded: the check verified nothing")
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("asking about the native queue is not a read: %q", c)
		}
	}
	t.Logf("%d call(s), all of them reads", len(calls))
}

// TestGitProviderRebaseWithoutPR proves the refusal the discovery about
// `RebaseSpec` requires: neither provider reapplies a loose branch.
func TestGitProviderRebaseWithoutPR(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		f := contract.NewGitHubFake(t, fakeToken)
		p := gitprovider.NewGitHub(gitprovider.GitHubConfig{
			APIBase: f.URL(), GraphQLURL: f.GraphQLURL(), Token: fakeToken,
			ActorID: testActor, Poll: time.Millisecond})
		source, target := branches("virgem")
		contract.GitProviderRebaseSemPR(t, p,
			contract.GitProviderEnv{Repo: contract.GHRepoOK, Actor: testActor}, source, target)
	})
	t.Run("gitlab", func(t *testing.T) {
		f := contract.NewGitLabFake(t, fakeToken)
		p := gitprovider.NewGitLab(gitprovider.GitLabConfig{
			APIBase: f.URL(), Token: fakeToken, ActorID: testActor, Poll: time.Millisecond})
		source, target := branches("virgem")
		contract.GitProviderRebaseSemPR(t, p,
			contract.GitProviderEnv{Repo: contract.GLProjOK, Actor: testActor}, source, target)
	})
}

// TestGitProviderGitLabTrainTriState isolates the subtlety that almost slipped
// through: in an installation with no merge pipelines licence the
// `merge_trains_enabled` key does NOT come back `false` — it DISAPPEARS from the
// response. A client deserializing into a `bool` would read `false` in both
// cases and never know the difference.
func TestGitProviderGitLabTrainTriState(t *testing.T) {
	f := contract.NewGitLabFake(t, fakeToken)
	p := gitprovider.NewGitLab(gitprovider.GitLabConfig{
		APIBase: f.URL(), Token: fakeToken, ActorID: testActor})
	casos := []struct {
		proj string
		want bool
	}{
		{contract.GLProjWithTrain, true},
		{contract.GLProjWithoutTrain, false},
		// An absent key: the installation offers no merge train at all, so there
		// is nothing to duplicate. It is the adapter's only inference, and it
		// errs towards KEEPING DOP's queue.
		{contract.GLProjNoLicence, false},
	}
	for _, c := range casos {
		got, err := p.HasNativeQueue(context.Background(), c.proj)
		if err != nil {
			t.Fatalf("%s: %v", c.proj, err)
		}
		if got != c.want {
			t.Errorf("%s: expected %v, got %v", c.proj, c.want, got)
		}
	}
	// And the case where we CANNOT look remains an error (guarantee 14).
	if ok, err := p.HasNativeQueue(context.Background(), contract.GLProjBadScope); err == nil || ok {
		t.Errorf("an insufficient scope returned (%v, %v) instead of an error", ok, err)
	} else if k := errs.KindOf(err); k != errs.KindPermission {
		t.Errorf("expected %s, got %s: %v", errs.KindPermission, k, err)
	}
}
