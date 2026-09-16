package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/adapter/verification"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// RunnerEnv is what THIS executor offers the suite.
//
// As with SandboxEnv, what changes between a cluster and the host's daemon is
// environment and not behaviour: which image to use, where the space is, and how
// a runner reaches a server listening on this machine. The guarantees, the
// commands and the timings belong to the suite, so both adapters are measured
// with the same ruler.
type RunnerEnv struct {
	NamespacePrefix string
	// Image needs `git`, `nc` and a POSIX shell — what the script's first step
	// checks for by name. The platform's real runner image is fat; the suite's
	// is tiny, because the suite has to run on the laptop of whoever touches the
	// adapter.
	Image string
	// DepImage is a published image that LISTENS on DepPort. It is a real
	// dependency, not a double: guarantee 6 is about waiting for something that
	// genuinely takes time to come up.
	DepImage string
	DepPort  int32
	Ready    time.Duration
	// GitHost is where a runner on this executor reaches this machine.
	GitHost string
}

// RunnerSuite verifies the ten guarantees documented on the VerificationRunner
// port.
//
// ADR-0001's discipline, applied to the newest port: Kubernetes and Docker share
// the run SCRIPT and nothing else. It is only by putting both through this suite
// that "the evidence does not depend on the executor" stops being a sentence in
// an ADR.
func RunnerSuite(t *testing.T, name string, newRunner func(t *testing.T) (ports.VerificationRunner, RunnerEnv)) {
	t.Run(name, func(t *testing.T) {
		// A dependency that never answers must fail in seconds here, not in two
		// minutes: a suite nobody runs proves nothing.
		old := verification.ReadyAttempts
		verification.ReadyAttempts = 8
		t.Cleanup(func() { verification.ReadyAttempts = old })

		t.Run("1_builds_the_commit_it_was_given_not_the_tip", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			first := headOf(t, git, project)
			git.commit(t, project, "marker.txt", "two") // the tip moves on

			st := runToEnd(t, r, env, runSpec(t, env, git, project, first,
				runCheck("aaa", "grep -q one marker.txt")))
			if st.Phase != ports.RunnerSucceeded {
				t.Fatalf("the run checked out something other than the commit it was given: %s (%s)",
					st.Phase, st.FailedStep)
			}
		})

		t.Run("2_starts_from_nothing", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)

			s1 := runSpec(t, env, git, project, c, runCheck("aaa", "touch /tmp/left-behind"))
			if st := runToEnd(t, r, env, s1); st.Phase != ports.RunnerSucceeded {
				t.Fatalf("the first run failed: %s (%s)", st.Phase, st.FailedStep)
			}
			// A second run, of the same commit and the same account, must not
			// find what the first one left outside the cache.
			s2 := runSpec(t, env, git, project, c, runCheck("aaa", "! test -e /tmp/left-behind"))
			s2.AccountID, s2.Namespace = s1.AccountID, s1.Namespace
			if st := runToEnd(t, r, env, s2); st.Phase != ports.RunnerSucceeded {
				t.Fatal("a runner inherited a file from a previous run: it is not starting from nothing")
			}
		})

		t.Run("3_the_accounts_cache_survives_between_runs", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)
			account := "acct-" + runNonce()

			s1 := runSpec(t, env, git, project, c, runCheck("aaa", "echo warm > /cache/proof"))
			s1.AccountID, s1.CachePaths = account, []string{"/cache"}
			s1.Namespace = env.NamespacePrefix + "-" + account
			if st := runToEnd(t, r, env, s1); st.Phase != ports.RunnerSucceeded {
				t.Fatalf("the run that fills the cache failed: %s (%s)", st.Phase, st.FailedStep)
			}
			s2 := runSpec(t, env, git, project, c, runCheck("aaa", "grep -q warm /cache/proof"))
			s2.AccountID, s2.CachePaths = account, []string{"/cache"}
			s2.Namespace = s1.Namespace
			if st := runToEnd(t, r, env, s2); st.Phase != ports.RunnerSucceeded {
				t.Fatal("the second run did not find the cache the first one left: " +
					"without it, building from source is slower than what it replaced")
			}
		})

		t.Run("4_5_a_failing_step_stops_the_sequence_and_is_not_a_port_error", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)

			st := runToEnd(t, r, env, runSpec(t, env, git, project, c,
				runCheck("aaa", "exit 3"),
				runCheck("e2e", "echo this-must-not-run")))
			if st.Phase != ports.RunnerFailed {
				t.Fatalf("a failing step did not fail the run: %s", st.Phase)
			}
			if st.FailedStep != "aaa" {
				t.Fatalf("the run does not say which step failed: %q", st.FailedStep)
			}
			for _, s := range st.Steps {
				if s.Name == "e2e" {
					t.Fatal("the step after the failure ran: a failing step has to stop the sequence")
				}
				if s.Name == "aaa" && s.ExitCode != 3 {
					t.Fatalf("the command's exit code was lost: %d", s.ExitCode)
				}
			}
		})

		t.Run("6_a_dependency_that_never_answers_fails_the_run_by_name", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)

			sp := runSpec(t, env, git, project, c, runCheck("aaa", "true"))
			// The runner's own image never listens on this port: it is a
			// dependency that comes up and answers nothing.
			sp.Dependencies = []ports.RunnerDependency{{Name: "silent", Image: env.Image, Port: 5599}}
			st := runToEnd(t, r, env, sp)
			if st.Phase != ports.RunnerFailed {
				t.Fatalf("a dependency that never answered did not fail the run: %s", st.Phase)
			}
			logs := runCollect(t, r, sp.RunnerHandle)
			if !strings.Contains(logs, "silent") {
				t.Fatalf("the failure does not name the dependency — the log said only:\n%s", runTail(logs))
			}
		})

		t.Run("6b_a_real_dependency_is_ready_before_the_checks", func(t *testing.T) {
			r, env := newRunner(t)
			if env.DepImage == "" {
				t.Skip("RunnerEnv.DepImage not set")
			}
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)

			sp := runSpec(t, env, git, project, c,
				runCheck("integration", fmt.Sprintf("nc -z -w2 127.0.0.1 %d", env.DepPort)))
			sp.Dependencies = []ports.RunnerDependency{
				{Name: "dep", Image: env.DepImage, Port: env.DepPort},
			}
			if st := runToEnd(t, r, env, sp); st.Phase != ports.RunnerSucceeded {
				t.Fatalf("a declared dependency was not reachable from the check: %s (%s)\n%s",
					st.Phase, st.FailedStep, runTail(runCollect(t, r, sp.RunnerHandle)))
			}
		})

		t.Run("7_logs_dies_with_the_caller", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)
			sp := runSpec(t, env, git, project, c, runCheck("aaa", "true"))
			if _, err := r.Start(context.Background(), sp); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Destroy(context.Background(), sp.RunnerHandle) })

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- r.Logs(ctx, sp.RunnerHandle, ports.LogQuery{Follow: true}, func(ports.LogLine) error { return nil })
			}()
			time.Sleep(500 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("a cancelled follow returned an error: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("Logs did not die with the caller: the goroutine is still there")
			}
		})

		t.Run("8_destroy_is_irreversible_and_idempotent", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)
			sp := runSpec(t, env, git, project, c, runCheck("aaa", "true"))
			if _, err := r.Start(context.Background(), sp); err != nil {
				t.Fatal(err)
			}
			if err := r.Destroy(context.Background(), sp.RunnerHandle); err != nil {
				t.Fatalf("destroy: %v", err)
			}
			if err := r.Destroy(context.Background(), sp.RunnerHandle); err != nil {
				t.Fatalf("destroying what no longer exists has to be success: %v", err)
			}
			if _, err := r.Status(context.Background(), sp.RunnerHandle); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("after Destroy, Status has to be not-found; it was %v", err)
			}
		})

		t.Run("9_two_runs_of_the_same_commit_coexist", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)

			a := runSpec(t, env, git, project, c, runCheck("aaa", "grep -q one marker.txt"))
			b := runSpec(t, env, git, project, c, runCheck("aaa", "grep -q one marker.txt"))
			// The SAME account's space: coexistence has to hold where it is
			// hardest, which is two runs sharing a namespace and a cache volume.
			b.AccountID, b.Namespace = a.AccountID, a.Namespace
			t.Cleanup(func() {
				_ = r.Destroy(context.Background(), a.RunnerHandle)
				_ = r.Destroy(context.Background(), b.RunnerHandle)
			})
			if _, err := r.Start(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Start(context.Background(), b); err != nil {
				t.Fatalf("a second run of the same commit was refused: %v", err)
			}
			for _, h := range []ports.RunnerHandle{a.RunnerHandle, b.RunnerHandle} {
				if st := runWait(t, r, h, env.Ready); st.Phase != ports.RunnerSucceeded {
					t.Fatalf("run %s ended in %s (%s)", h.ID, st.Phase, st.FailedStep)
				}
			}
		})

		t.Run("10_a_run_with_no_checks_holds_as_a_dev_session", func(t *testing.T) {
			r, env := newRunner(t)
			git := newGitServerAt(t, env.GitHost)
			project := "p" + runNonce()
			git.commit(t, project, "marker.txt", "one")
			c := headOf(t, git, project)

			sp := runSpec(t, env, git, project, c)
			sp.AppPort = 8080
			sp.Steps = []ports.RunnerStep{
				// A LOOP, not a single `nc -l`: busybox's serves one connection
				// and exits, so the readiness probe itself would kill the
				// application and the session would end the moment it began.
				{Name: "start", Kind: ports.StepStart, Command: "while true; do nc -l -p 8080 </dev/null >/dev/null 2>&1; done"},
			}
			if _, err := r.Start(context.Background(), sp); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Destroy(context.Background(), sp.RunnerHandle) })

			st := runWaitPhase(t, r, sp.RunnerHandle, env.Ready, ports.RunnerHolding)
			if st.Phase != ports.RunnerHolding {
				t.Fatalf("a run with no checks did not hold: %s (%s)\n%s",
					st.Phase, st.FailedStep, runTail(runCollect(t, r, sp.RunnerHandle)))
			}
			if st.Endpoint == nil || st.Endpoint.Port == 0 {
				t.Fatal("a dev session published no port: nobody can look at the application")
			}
			// It holds: it is not going to finish on its own.
			time.Sleep(2 * time.Second)
			if st2, err := r.Status(context.Background(), sp.RunnerHandle); err != nil || st2.Phase != ports.RunnerHolding {
				t.Fatalf("the session ended by itself: %v / %v", st2, err)
			}
		})
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

func runNonce() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func runCheck(kind, cmd string) ports.RunnerStep {
	return ports.RunnerStep{Name: kind, Kind: ports.StepCheck, Command: cmd}
}

func headOf(t *testing.T, g *gitServer, project string) string {
	t.Helper()
	sha, err := g.repos.Commit(context.Background(), project, ports.RepositoryCommit{
		Files:   []ports.RepositoryFile{{Path: "head.txt", Content: []byte(runNonce())}},
		Message: "suite: head",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func runSpec(t *testing.T, env RunnerEnv, g *gitServer, project, commit string, steps ...ports.RunnerStep) ports.RunnerSpec {
	t.Helper()
	id := runNonce() + runNonce()
	info, err := g.repos.Ensure(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := g.repos.IssueToken(context.Background(), project, "demand-"+id[:8], time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// The namespace is the ACCOUNT's, and the id tells the runs apart: it is what
	// lets the account's cache outlive one run (guarantee 3).
	account := "acct-" + id[:8]
	return ports.RunnerSpec{
		RunnerHandle: ports.RunnerHandle{ID: id, Namespace: env.NamespacePrefix + "-" + account},
		AccountID:    account,
		DemandID:     "demand-" + id[:8],
		Image:        env.Image,
		Repository:   ports.RunnerRepository{CloneURL: info.CloneURL, Token: tok, Commit: commit},
		Steps:        steps,
	}
}

func runToEnd(t *testing.T, r ports.VerificationRunner, env RunnerEnv, sp ports.RunnerSpec) *ports.RunnerStatus {
	t.Helper()
	if _, err := r.Start(context.Background(), sp); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = r.Destroy(context.Background(), sp.RunnerHandle) })
	return runWait(t, r, sp.RunnerHandle, env.Ready)
}

func runWait(t *testing.T, r ports.VerificationRunner, h ports.RunnerHandle, d time.Duration) *ports.RunnerStatus {
	t.Helper()
	deadline := time.Now().Add(d)
	var last *ports.RunnerStatus
	for time.Now().Before(deadline) {
		st, err := r.Status(context.Background(), h)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		last = st
		if st.Phase == ports.RunnerSucceeded || st.Phase == ports.RunnerFailed {
			return st
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the run did not finish within %s (last: %v)\n%s", d, last, runTail(runCollect(t, r, h)))
	return nil
}

func runWaitPhase(t *testing.T, r ports.VerificationRunner, h ports.RunnerHandle, d time.Duration, want ports.RunnerPhase) *ports.RunnerStatus {
	t.Helper()
	deadline := time.Now().Add(d)
	var last *ports.RunnerStatus
	for time.Now().Before(deadline) {
		st, err := r.Status(context.Background(), h)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		last = st
		if st.Phase == want || st.Phase == ports.RunnerFailed {
			return st
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}

func runCollect(t *testing.T, r ports.VerificationRunner, h ports.RunnerHandle) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var b strings.Builder
	_ = r.Logs(ctx, h, ports.LogQuery{}, func(l ports.LogLine) error {
		b.WriteString(l.Text)
		b.WriteByte('\n')
		return nil
	})
	return b.String()
}

func runTail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return "─── the run's last lines ───\n" + strings.Join(lines, "\n")
}
