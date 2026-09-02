package contract

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// GitProviderEnv is what THIS provider offers for the suite to work with.
//
// It exists for SandboxEnv's same reason: what changes between GitHub and GitLab
// is not behaviour — it is the NAME of things. "org/repo" against
// "group%2Fproject", a branch that conflicts, a repository the token cannot see.
// Everything else belongs to the suite, so both adapters are measured with the
// SAME ruler.
//
// An empty function field means "this environment cannot produce that case": the
// subtest is SKIPPED with a record, never in silence. It is the identity suite's
// `Mint` discipline — against a real provider you cannot fabricate a conflict on
// demand without pushing commits, and lying about the coverage is worse than
// admitting the hole.
type GitProviderEnv struct {
	// Connect builds a connection that speaks for the given actor. An empty
	// actor = the environment's default actor.
	Connect func(t *testing.T, actorID string) delivery.GitProvider

	// Actor is the actor the default connection was built for (guarantee 14).
	Actor string

	// SentinelToken is the EXACT token the default connection carries. The suite
	// sweeps every error output looking for it (guarantee 13). Empty = the sweep
	// is skipped with a SHOUTED warning: it is the guarantee whose failure costs
	// the whole account.
	SentinelToken string

	// ConnectWithoutCredential builds a connection with an invalid token — the
	// KindUnauthorized case of guarantee 11.
	ConnectWithoutCredential func(t *testing.T) delivery.GitProvider

	// Repo is the repository where the suite opens and merges PRs.
	Repo string
	// InvisibleRepo does not exist, or exists and the token cannot reach it.
	// Both cases have to come out as KindNotFound (guarantee 12).
	InvisibleRepo string

	// RepoWithNativeQueue and RepoWithoutNativeQueue are repositories whose
	// HasNativeQueue answer is KNOWN. Empty = the subtest is skipped.
	RepoWithNativeQueue    string
	RepoWithoutNativeQueue string
	// RepoWithUnreadableQueue is the repository the adapter CANNOT look at (no
	// permission, a plan feature absent). It is guarantee 15's case: it has to
	// come out as an ERROR, never as `false`.
	RepoWithUnreadableQueue string

	// Pair returns a NEW (source, target) pair on every call, one that
	// integrates cleanly. New on every call is mandatory: against a real
	// provider the suite's second run would find the first run's PR, and the
	// idempotency subtest would pass by accident.
	Pair func(t *testing.T) (source, target string)
	// ConflictingPair returns a pair the provider REFUSES over a conflict.
	ConflictingPair func(t *testing.T) (source, target string)
	// BlockedPair returns a pair whose merge is refused for a reason that is NOT
	// a conflict — a running pipeline, a missing approval (guarantee 8).
	BlockedPair func(t *testing.T) (source, target string)
	// PairWithNoCommits returns a pair whose source does not exist or has
	// nothing to integrate: the case where opening a PR has to FAIL, and fail
	// with an error (not with a PR whose ExternalID is empty).
	PairWithNoCommits func(t *testing.T) (source, target string)

	// Wait is how long to tolerate an asynchronous rebase.
	Wait time.Duration
}

// GitProviderSuite verifies the seventeen guarantees documented on the
// delivery.GitProvider port.
//
// ADR-0001's discipline: a port with a single adapter is guesswork. GitHub and
// GitLab do not have ONE line in common — a PR number against an MR iid,
// `merged` against `state`, 422 against 409 for the same fact — and it is only
// by putting both through this suite that "changing provider is wiring" stops
// being a promise.
func GitProviderSuite(t *testing.T, name string, env func(t *testing.T) GitProviderEnv) {
	t.Run(name, func(t *testing.T) {
		e := env(t)
		if e.Wait <= 0 {
			e.Wait = 2 * time.Minute
		}
		conectar := func(t *testing.T) delivery.GitProvider {
			t.Helper()
			return e.Connect(t, e.Actor)
		}

		// ── guarantees 1 and 2: a conflict is DATA, with a Detail ───────────

		t.Run("1_a_conflicted_rebase_is_data_not_an_error", func(t *testing.T) {
			if e.ConflictingPair == nil {
				t.Skip("this environment cannot fabricate a conflict — the case is not verifiable here")
			}
			p := conectar(t)
			source, target := e.ConflictingPair(t)
			ctx, cancel := context.WithTimeout(context.Background(), e.Wait)
			defer cancel()

			// The PR MUST exist first — and that is not test ceremony, it is a
			// discovery about both providers. Neither offers a "branch rebase":
			// GitHub reapplies a PR's branch (through GraphQL) and GitLab
			// reapplies an MR's branch (through a REST route), ALWAYS on top of
			// that PR/MR's target. `RebaseSpec` looks like a git operation and
			// is not.
			openForRebase(t, p, e, source, target)

			r, err := p.Rebase(ctx, delivery.RebaseSpec{
				RepoExternalID: e.Repo, Branch: source, Onto: target})
			if err != nil {
				t.Fatalf("A CONFLICT BECAME AN ERROR: ADR-0008 §2's flow depends on the "+
					"conflict arriving as data so it can become the agent's task and an "+
					"attention-box item; an error becomes an infra retry and disappears: %v", err)
			}
			if !r.Conflicted {
				t.Fatalf("the provider refused the reapplication and the result came back clean: %+v", r)
			}
			// Guarantee 2: Files is best-effort (neither provider publishes the
			// list), but Detail is mandatory — it is what the attention box shows
			// for the human to decide without archaeology.
			if strings.TrimSpace(r.Detail) == "" {
				t.Error("a conflict with no Detail: the attention box would receive an empty alarm")
			}
			if len(r.Files) > 0 {
				t.Logf("this provider reported %d conflicted file(s): %v", len(r.Files), r.Files)
			} else {
				t.Log("no file listed — as expected: the list does not come out of either " +
					"provider's public PR/MR API (guarantee 2)")
			}
		})

		t.Run("1b_a_clean_rebase_brings_both_commits", func(t *testing.T) {
			if e.Pair == nil {
				t.Skip("an environment with no branch factory")
			}
			p := conectar(t)
			source, target := e.Pair(t)
			ctx, cancel := context.WithTimeout(context.Background(), e.Wait)
			defer cancel()
			openForRebase(t, p, e, source, target)

			r, err := p.Rebase(ctx, delivery.RebaseSpec{
				RepoExternalID: e.Repo, Branch: source, Onto: target})
			if err != nil {
				t.Fatalf("Rebase: %v", err)
			}
			if r.Conflicted {
				t.Fatalf("a pair that integrates cleanly came back as a conflict: %+v", r)
			}
			// HeadCommit is what the queue re-verifies in the next position
			// (ADR-0008 §1). Without it, "green is always about a state of the
			// code" loses the state.
			if strings.TrimSpace(r.HeadCommit) == "" {
				t.Error("a clean rebase with no HeadCommit: the queue would have no commit to re-verify on")
			}
		})

		// ── garantias 3, 4 e 5: abrir PR ────────────────────────────────────

		t.Run("3_opening_a_pr_is_idempotent_per_branch", func(t *testing.T) {
			if e.Pair == nil {
				t.Skip("an environment with no branch factory")
			}
			p := conectar(t)
			source, target := e.Pair(t)
			ctx := context.Background()

			spec := delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: source, TargetBranch: target,
				Title:   "demanda 1: primeira tentativa",
				Body:    "acceptance: 12/12; critic: approved; trace: dop://t/1",
				ActorID: e.Actor,
			}
			first, err := p.OpenPullRequest(ctx, spec)
			if err != nil {
				t.Fatalf("1ª abertura: %v", err)
			}

			// Guarantee 4: a DIFFERENT title and body on the second call. If the
			// idempotency were "it overwrites", ADR-0007 §4's evidence package
			// would be traded for a network retry.
			spec.Title = "demand 1: a retry after a timeout"
			spec.Body = "a different body, which must NOT replace the evidence package"
			second, err := p.OpenPullRequest(ctx, spec)
			if err != nil {
				t.Fatalf("REOPENING BECAME AN ERROR: a network timeout in a fleet of "+
					"agents would become an attention item about a PR that was opened "+
					"sucesso: %v", err)
			}
			if first.ExternalID != second.ExternalID {
				t.Fatalf("TWO PRs FOR THE SAME BRANCH: %q and %q",
					first.ExternalID, second.ExternalID)
			}
			if first.URL != second.URL {
				t.Errorf("mesma identidade, URLs diferentes: %q e %q", first.URL, second.URL)
			}
			// Guarantee 4, the half observable through the port: the PR returned
			// is the one that already existed, with the original creation date.
			// The other half — "no update request was sent" — is only observable
			// on the adapter's side, and it is in the local double's test.
			if !first.CreatedAt.Equal(second.CreatedAt) {
				t.Errorf("the PR was recreated or rewritten: created at %v, then at %v",
					first.CreatedAt, second.CreatedAt)
			}
		})

		t.Run("5_the_returned_pr_has_an_identity", func(t *testing.T) {
			if e.Pair == nil {
				t.Skip("an environment with no branch factory")
			}
			p := conectar(t)
			source, target := e.Pair(t)
			pr, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: source, TargetBranch: target,
				Title: "identity", Body: "evidence", ActorID: e.Actor})
			if err != nil {
				t.Fatalf("OpenPullRequest: %v", err)
			}
			if strings.TrimSpace(pr.ExternalID) == "" {
				t.Error("a PR with no ExternalID: it is what Merge receives later — without " +
					"it the opened PR has no way of being merged by the queue")
			}
			if strings.TrimSpace(pr.URL) == "" {
				t.Error("a PR with no URL: it is the only address the human in the attention box opens")
			}
			if pr.TargetBranch != target {
				t.Errorf("divergent target: I asked for %q, got %q", target, pr.TargetBranch)
			}
			if pr.CreatedAt.IsZero() {
				t.Error("a PR with no creation date")
			}
		})

		t.Run("5b_a_pr_with_nothing_to_integrate_fails_with_an_error", func(t *testing.T) {
			if e.PairWithNoCommits == nil {
				t.Skip("this environment cannot fabricate a branch with nothing to integrate")
			}
			p := conectar(t)
			source, target := e.PairWithNoCommits(t)
			pr, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: source, TargetBranch: target,
				Title: "no commits", ActorID: e.Actor})
			if err == nil {
				t.Fatalf("a PR opened on a branch that does not exist or has nothing to integrate: %+v", pr)
			}
			// The point is NOT the Kind, it is the absence of a middle ground:
			// the port must not return an empty ProviderPR with a nil error (the
			// mirror of ObjectStore's guarantee 8 about SignedPutURL).
			if pr.ExternalID != "" {
				t.Errorf("an error AND a PR at the same time: %+v", pr)
			}
		})

		// ── garantias 6, 7 e 8: merge ───────────────────────────────────────

		t.Run("6_merge_is_idempotent", func(t *testing.T) {
			if e.Pair == nil {
				t.Skip("an environment with no branch factory")
			}
			p := conectar(t)
			source, target := e.Pair(t)
			ctx := context.Background()
			pr, err := p.OpenPullRequest(ctx, delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: source, TargetBranch: target,
				Title: "merge idempotente", ActorID: e.Actor})
			if err != nil {
				t.Fatalf("OpenPullRequest: %v", err)
			}
			spec := delivery.MergeSpec{
				RepoExternalID: e.Repo, PRExternalID: pr.ExternalID, ActorID: e.Actor}

			first, err := p.Merge(ctx, spec)
			if err != nil {
				t.Fatalf("1º Merge: %v", err)
			}
			if !first.Merged {
				t.Fatalf("a PR with nothing in its way did not merge: %+v", first)
			}
			// Garantia 7.
			if strings.TrimSpace(first.MergeCommit) == "" {
				t.Error("Merged=true with no MergeCommit: 'I accepted your request' and 'it is " +
					"on main' are different facts, and the queue releases the next position by the second")
			}
			if first.MergedAtUnix == 0 {
				t.Error("a confirmed merge with no instant")
			}

			// Guarantee 6. Delivering this costs an extra read: BOTH providers
			// refuse an already merged PR with the SAME HTTP code they use for
			// "there is a conflict".
			second, err := p.Merge(ctx, spec)
			if err != nil {
				t.Fatalf("A REMERGE BECAME AN ERROR: the queue reprocesses the position "+
					"after a crash and has to recognize what already went in: %v", err)
			}
			if !second.Merged {
				t.Errorf("an already merged PR returned Merged=false: %+v", second)
			}
			if second.Conflicted {
				t.Errorf("an already merged PR returned a CONFLICT — it is the 405's "+
					"ambiguity leaking through the port: %+v", second)
			}
			if second.MergeCommit != first.MergeCommit {
				t.Errorf("the merge commit changed between two reads: %q and %q",
					first.MergeCommit, second.MergeCommit)
			}
		})

		t.Run("8_not_merged_without_being_a_conflict_is_legitimate", func(t *testing.T) {
			if e.BlockedPair == nil {
				t.Skip("this environment cannot fabricate a PR blocked for a reason that is not a conflict")
			}
			p := conectar(t)
			source, target := e.BlockedPair(t)
			ctx := context.Background()
			pr, err := p.OpenPullRequest(ctx, delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: source, TargetBranch: target,
				Title: "blocked", ActorID: e.Actor})
			if err != nil {
				t.Fatalf("OpenPullRequest: %v", err)
			}
			r, err := p.Merge(ctx, delivery.MergeSpec{
				RepoExternalID: e.Repo, PRExternalID: pr.ExternalID, ActorID: e.Actor})
			if err != nil {
				t.Fatalf("a block became an error — 'it cannot merge yet' is a state of the "+
					"flow, not an infrastructure failure: %v", err)
			}
			if r.Merged {
				t.Fatalf("the provider refused the merge and the result says it merged: %+v", r)
			}
			if r.Conflicted {
				t.Fatal("bloqueio classificado como CONFLITO: 'conflito' viraria o balde " +
					"of everything that did not merge, and the attention box would call a human " +
					"to solve a pipeline that is still running")
			}
			if strings.TrimSpace(r.Detail) == "" {
				t.Error("it did not merge, it did not conflict and it did not say why")
			}
		})

		// ── guarantees 9 and 10: the rebase is synchronous, and requires an open PR ──

		t.Run("9_a_cancelled_context_never_becomes_no_conflict", func(t *testing.T) {
			if e.Pair == nil {
				t.Skip("an environment with no branch factory")
			}
			p := conectar(t)
			source, target := e.Pair(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // it is born cancelled

			r, err := p.Rebase(ctx, delivery.RebaseSpec{
				RepoExternalID: e.Repo, Branch: source, Onto: target})
			if err == nil {
				t.Fatalf("the context was cancelled and the rebase answered anyway: %+v — "+
					"Conflicted=false would mean 'it did not conflict' when what happened "+
					"was 'I do not know'", r)
			}
			if k := errs.KindOf(err); k != errs.KindUnavailable {
				t.Errorf("a cancellation should be %s, got %s: %v", errs.KindUnavailable, k, err)
			}
		})

		// ── guarantee 11: the provider's vocabulary does not cross ──────────

		t.Run("11_the_externalid_is_opaque_and_round_trips", func(t *testing.T) {
			if e.Pair == nil {
				t.Skip("an environment with no branch factory")
			}
			p := conectar(t)
			source, target := e.Pair(t)
			ctx := context.Background()
			pr, err := p.OpenPullRequest(ctx, delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: source, TargetBranch: target,
				Title: "opacidade", ActorID: e.Actor})
			if err != nil {
				t.Fatalf("OpenPullRequest: %v", err)
			}
			// The opacity test is the ROUND TRIP: what the port returned goes
			// back into it and works, without the suite needing to know whether
			// that is a GitHub PR number or a GitLab MR iid. A suite that
			// asserted the format would be writing one of the two vocabularies
			// into the port.
			r, err := p.Merge(ctx, delivery.MergeSpec{
				RepoExternalID: e.Repo, PRExternalID: pr.ExternalID, ActorID: e.Actor})
			if err != nil {
				t.Fatalf("the ExternalID the port returned was not accepted back by it: %v", err)
			}
			if !r.Merged {
				t.Fatalf("the ExternalID round trip did not complete: %+v", r)
			}
		})

		// ── guarantee 12: absence, permission and credential ────────────────

		t.Run("12_an_invisible_repository_is_notfound", func(t *testing.T) {
			if e.InvisibleRepo == "" {
				t.Skip("an environment with no invisible repository declared")
			}
			p := conectar(t)
			_, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
				RepoExternalID: e.InvisibleRepo, SourceBranch: "x", TargetBranch: "main",
				Title: "should not open", ActorID: e.Actor})
			if err == nil {
				t.Fatal("a PR opened in a nonexistent repository")
			}
			if k := errs.KindOf(err); k != errs.KindNotFound {
				t.Errorf("expected %s, got %s: %v", errs.KindNotFound, k, err)
			}
		})

		t.Run("12b_an_invalid_credential_is_unauthorized", func(t *testing.T) {
			if e.ConnectWithoutCredential == nil {
				t.Skip("this environment cannot build a connection with an invalid credential")
			}
			p := e.ConnectWithoutCredential(t)
			_, err := p.HasNativeQueue(context.Background(), e.Repo)
			if err == nil {
				t.Fatal("an invalid credential was accepted")
			}
			if k := errs.KindOf(err); k != errs.KindUnauthorized {
				t.Errorf("expected %s — the decision for whoever operates is 'the credential "+
					"does not work', and confusing it with an unavailability sends the team "+
					"hunting the defect in the wrong place. Got %s: %v", errs.KindUnauthorized, k, err)
			}
		})

		// ── guarantee 13: the token appears nowhere ─────────────────────────

		t.Run("13_the_token_leaks_neither_in_an_error_nor_in_text", func(t *testing.T) {
			if e.SentinelToken == "" {
				t.Skip("WARNING: an environment with no sentinel token — the guarantee whose " +
					"failure hands over the whole account was NOT verified here")
			}
			p := conectar(t)
			ctx := context.Background()

			// Errors from several paths: each formats its message on its own, and
			// ONE forgetting is enough.
			var errList []error
			if e.InvisibleRepo != "" {
				_, err := p.HasNativeQueue(ctx, e.InvisibleRepo)
				errList = append(errList, err)
				_, err = p.OpenPullRequest(ctx, delivery.OpenPRSpec{
					RepoExternalID: e.InvisibleRepo, SourceBranch: "b", TargetBranch: "main",
					ActorID: e.Actor})
				errList = append(errList, err)
				_, err = p.Merge(ctx, delivery.MergeSpec{
					RepoExternalID: e.InvisibleRepo, PRExternalID: "1", ActorID: e.Actor})
				errList = append(errList, err)
				_, err = p.Rebase(ctx, delivery.RebaseSpec{
					RepoExternalID: e.InvisibleRepo, Branch: "b", Onto: "main"})
				errList = append(errList, err)
			}
			if e.ConnectWithoutCredential != nil {
				ruim := e.ConnectWithoutCredential(t)
				_, err := ruim.HasNativeQueue(ctx, e.Repo)
				errList = append(errList, err)
			}
			_, err := p.OpenPullRequest(ctx, delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: "b", TargetBranch: "main",
				ActorID: "some-other-actor"})
			errList = append(errList, err)

			seen := 0
			for _, err := range errList {
				if err == nil {
					continue
				}
				seen++
				if strings.Contains(err.Error(), e.SentinelToken) {
					// Without printing the error: printing it would put the token
					// in the test's own log.
					t.Fatal("LEAK: the error message carries the provider's token — " +
						"an error goes up to a log, and a token in a log is a credential at rest")
				}
			}
			if seen == 0 {
				t.Fatal("no error was provoked: the sweep verified nothing")
			}

			// And the adapter itself, formatted. `%+v` reads UNEXPORTED fields
			// through reflection and cannot call their String() — which is why
			// the token lives in a closure, and not in a field.
			for _, s := range []string{fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p)} {
				if strings.Contains(s, e.SentinelToken) {
					t.Fatal("VAZAMENTO: formatar o adaptador revela o token")
				}
			}
			t.Logf("%d error message(s) swept with no sign of the token", seen)
		})

		// ── guarantee 14: the actor is checked ──────────────────────────────

		t.Run("14_an_actor_other_than_the_connections_is_refused", func(t *testing.T) {
			if e.Actor == "" {
				t.Skip("an environment with no actor declared")
			}
			p := conectar(t)
			_, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: "qualquer", TargetBranch: "main",
				Title: "on somebody else's behalf", ActorID: e.Actor + "-impostor"})
			if err == nil {
				t.Fatal("a PR opened on behalf of an actor that is NOT the credential's: the PR " +
					"would go out signed by whoever owns the borrowed token, and the " +
					"ADR-0003 ('whoever conducted signs') would become a silent lie")
			}
			if k := errs.KindOf(err); k != errs.KindPermission {
				t.Errorf("expected %s, got %s: %v", errs.KindPermission, k, err)
			}
			// And the same for the merge: signing the merge is signing too.
			_, err = p.Merge(context.Background(), delivery.MergeSpec{
				RepoExternalID: e.Repo, PRExternalID: "1", ActorID: e.Actor + "-impostor"})
			if k := errs.KindOf(err); err == nil || k != errs.KindPermission {
				t.Errorf("a Merge on behalf of another actor was not refused (%v)", err)
			}
		})

		// ── garantias 15 e 16: fila nativa ──────────────────────────────────

		t.Run("15_the_native_queue_does_not_invent_an_answer", func(t *testing.T) {
			p := conectar(t)
			ctx := context.Background()
			verified := 0

			if e.RepoWithNativeQueue != "" {
				ok, err := p.HasNativeQueue(ctx, e.RepoWithNativeQueue)
				if err != nil {
					t.Errorf("a repository with a native queue: %v", err)
				} else if !ok {
					t.Error("a repository WITH a native queue answered false: DOP's queue " +
						"would orchestrate on top of the provider's and the two would merge " +
						"the same repository (ADR-0008 §4)")
				}
				verified++
			}
			if e.RepoWithoutNativeQueue != "" {
				ok, err := p.HasNativeQueue(ctx, e.RepoWithoutNativeQueue)
				if err != nil {
					t.Errorf("a repository with no native queue: %v", err)
				} else if ok {
					t.Error("a repository WITHOUT a native queue answered true: DOP would step " +
						"aside and nobody would serialize the merges")
				}
				verified++
			}
			if e.RepoWithUnreadableQueue != "" {
				ok, err := p.HasNativeQueue(ctx, e.RepoWithUnreadableQueue)
				if err == nil {
					t.Errorf("the adapter COULD NOT look and answered %v anyway. "+
						"`false` is an ASSERTION — 'you may orchestrate on top' — and asserting it "+
						"without having looked is the same defect as degrading isolation in silence", ok)
				}
				if ok {
					t.Error("a true answer TOGETHER with an error: whoever ignores the error steps aside")
				}
				verified++
			}
			if verified == 0 {
				t.Skip("the environment declared no repository with a known native-queue answer")
			}
		})

		t.Run("16_the_native_queue_is_a_read_and_is_stable", func(t *testing.T) {
			if e.RepoWithoutNativeQueue == "" {
				t.Skip("an environment with no repository of known queue status")
			}
			p := conectar(t)
			ctx := context.Background()
			a, err := p.HasNativeQueue(ctx, e.RepoWithoutNativeQueue)
			if err != nil {
				t.Fatalf("HasNativeQueue: %v", err)
			}
			b, err := p.HasNativeQueue(ctx, e.RepoWithoutNativeQueue)
			if err != nil {
				t.Fatalf("2ª HasNativeQueue: %v", err)
			}
			if a != b {
				t.Fatalf("asking changed the answer: %v and then %v", a, b)
			}
		})

		// ── guarantee 17: concurrency ───────────────────────────────────────

		t.Run("17_safe_for_concurrent_use", func(t *testing.T) {
			if e.Pair == nil {
				t.Skip("an environment with no branch factory")
			}
			p := conectar(t)
			const n = 6
			var wg sync.WaitGroup
			ids := make([]string, n)
			errList := make([]error, n)
			source, target := e.Pair(t)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					// ALL the goroutines open the SAME PR: besides exercising the
					// race in the HTTP client, it is guarantee 3's idempotency
					// under concurrency — which is how it really happens in a
					// fleet of agents with retries.
					pr, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
						RepoExternalID: e.Repo, SourceBranch: source, TargetBranch: target,
						Title: fmt.Sprintf("concorrente %d", i), ActorID: e.Actor})
					ids[i], errList[i] = pr.ExternalID, err
				}(i)
			}
			wg.Wait()
			for i, err := range errList {
				if err != nil {
					t.Fatalf("concurrent opening %d failed: %v", i, err)
				}
			}
			for i := 1; i < n; i++ {
				if ids[i] != ids[0] {
					t.Fatalf("the race created different PRs for the same branch: %q and %q",
						ids[0], ids[i])
				}
			}
		})
	})
}

// openForRebase ensures the PR both providers require before reapplying.
//
// It is here, and not in the environment, because the requirement is the
// PROVIDERS' and holds for both — it is part of what the suite discovered, not
// configuration from whoever assembles it.
func openForRebase(t *testing.T, p delivery.GitProvider, e GitProviderEnv, source, target string) {
	t.Helper()
	if _, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
		RepoExternalID: e.Repo, SourceBranch: source, TargetBranch: target,
		Title: "the rebase's prerequisite", ActorID: e.Actor,
	}); err != nil {
		t.Fatalf("could not prepare the PR the rebase requires: %v", err)
	}
}

// RebaseWithoutPRError is the check that the requirement above is REFUSED with a
// message, and not with a silent rebase onto the wrong place. It is separate
// from the main suite because it only makes sense where the environment can
// guarantee there is NO PR for the pair — against a real provider, that requires
// a virgin branch.
func GitProviderRebaseSemPR(t *testing.T, p delivery.GitProvider, e GitProviderEnv, source, target string) {
	t.Helper()
	_, err := p.Rebase(context.Background(), delivery.RebaseSpec{
		RepoExternalID: e.Repo, Branch: source, Onto: target})
	if err == nil {
		t.Fatal("it reapplied a branch with no open PR: neither provider does that, " +
			"so whatever happened was not what the domain asked for")
	}
	if k := errs.KindOf(err); k != errs.KindPrecondition {
		t.Errorf("expected %s, got %s: %v", errs.KindPrecondition, k, err)
	}
}
