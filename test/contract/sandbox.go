package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// SandboxEnv describes what THIS substrate has for the suite to work with.
//
// It exists because the two things that change between a cluster and the host's
// Docker are not behaviour, they are environment: the name of the space to
// create in and which isolation level the machine really offers. Everything else
// — the command, the workspace marker, the timings — belongs to the suite, so
// both adapters are measured with the SAME ruler.
type SandboxEnv struct {
	// NamespacePrefix has to be valid under DNS-1123: k8s requires it, Docker
	// accepts it, and using the stricter rule on both is what keeps the same
	// test running on either side.
	NamespacePrefix string
	Image           string
	// Tier is what this substrate delivers. Unsupported is one it does NOT
	// deliver — it is the case that proves the refusal instead of the
	// degradation.
	Tier        ports.IsolationTier
	Unsupported ports.IsolationTier
	// Ready is how long to wait for a phase change. A pod pulling an image takes
	// far longer than a local container.
	Ready time.Duration
}

// SandboxSuite verifies the twelve guarantees documented on the SandboxLauncher
// port.
//
// ADR-0001's discipline: a port with a single adapter is guesswork. Kubernetes
// and Docker do not have ONE line in common in their implementations — it is
// only by putting both through this suite that "changing substrate does not
// change the behaviour" stops being a promise and becomes a verified fact.
func SandboxSuite(t *testing.T, name string, newLauncher func(t *testing.T) (ports.SandboxLauncher, SandboxEnv)) {
	t.Run(name, func(t *testing.T) {
		t.Run("3_supported_tiers_is_never_empty", func(t *testing.T) {
			l, env := newLauncher(t)
			tiers, err := l.SupportedTiers(context.Background())
			if err != nil {
				t.Fatalf("SupportedTiers: %v", err)
			}
			if len(tiers) == 0 {
				t.Fatal("an empty list with no error: a substrate with no level at all is an unavailable substrate")
			}
			for _, tr := range tiers {
				if !ports.ValidIsolationTier(tr) {
					t.Errorf("tier outside the vocabulary: %q", tr)
				}
			}
			if !containsTier(tiers, env.Tier) {
				t.Errorf("the environment says it delivers %q, the substrate does not list it: %v", env.Tier, tiers)
			}
			if containsTier(tiers, env.Unsupported) {
				t.Errorf("the environment says it does NOT deliver %q, but the substrate lists it", env.Unsupported)
			}
		})

		t.Run("1_the_tier_delivered_is_the_one_declared", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			st, err := l.Launch(ctx, spec)
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if st.Tier != spec.Tier {
				t.Fatalf("SILENT DEGRADATION: I asked for %q, I got %q", spec.Tier, st.Tier)
			}
			st = waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			if st.Tier != spec.Tier {
				t.Fatalf("the tier changed after provisioning: %q != %q", st.Tier, spec.Tier)
			}
		})

		t.Run("2_an_unsupported_tier_refuses_with_no_trace", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)
			spec.Tier = env.Unsupported

			if _, err := l.Launch(ctx, spec); err == nil {
				t.Fatal("it accepted a tier the substrate does not offer — degrading in silence is forbidden")
			} else if k := errs.KindOf(err); k != errs.KindPrecondition {
				t.Fatalf("expected KindPrecondition with a message, got %s: %v", k, err)
			}
			// A refusal that provisions half is worse than no refusal at all.
			if _, err := l.Describe(ctx, spec.SandboxHandle); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("the refusal left a trace: Describe returned %v", err)
			}
		})

		t.Run("4_idempotent_launch", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("1º Launch: %v", err)
			}
			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("the 2nd Launch should return the existing one: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// A single Destroy has to be enough: if the relaunch had created a
			// second sandbox, something would survive it.
			if err := l.Destroy(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("Destroy: %v", err)
			}
			waitGone(t, l, spec.SandboxHandle, env.Ready)
		})

		t.Run("5and6_suspend_preserves_the_workspace_and_resume_picks_it_up", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			waitLog(t, l, spec.SandboxHandle, markerAbsent, env.Ready)

			if err := l.Suspend(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			st := waitPhase(t, l, spec.SandboxHandle, ports.PhaseSuspended, env.Ready)
			if st.Tier != spec.Tier {
				t.Errorf("suspenso perdeu o tier declarado: %q != %q", st.Tier, spec.Tier)
			}

			if _, err := l.Resume(ctx, spec); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// GUARANTEE 5, and only it: what was under SandboxWorkspacePath
			// survived. Nothing OUTSIDE it is verified here, on purpose — the
			// k8s adapter deletes the whole pod on suspension and Docker's keeps
			// the container's writable layer. Demanding Docker's behaviour would
			// make the suite fail k8s for delivering the port.
			waitLog(t, l, spec.SandboxHandle, markerPresent, env.Ready)
		})

		t.Run("7_idempotent_suspend_and_resume", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			if _, err := l.Resume(ctx, spec); err != nil {
				t.Fatalf("a Resume of an active sandbox should be harmless: %v", err)
			}
			if err := l.Suspend(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("1º Suspend: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseSuspended, env.Ready)
			if err := l.Suspend(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("the 2nd Suspend should be harmless: %v", err)
			}
		})

		t.Run("8_destroy_is_irreversible_and_idempotent", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			if err := l.Destroy(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("Destroy: %v", err)
			}
			waitGone(t, l, spec.SandboxHandle, env.Ready)

			// Irreversible means there is no way back THROUGH THE PORT.
			if _, err := l.Resume(ctx, spec); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("the destroyed one came back to life: Resume returned %v", err)
			}
			if err := l.Destroy(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("the 2nd Destroy should be harmless: %v", err)
			}
		})

		t.Run("9_nonexistent", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env) // never launched

			if _, err := l.Describe(ctx, spec.SandboxHandle); errs.KindOf(err) != errs.KindNotFound {
				t.Errorf("Describe: expected KindNotFound, got %v", err)
			}
			if err := l.Suspend(ctx, spec.SandboxHandle); errs.KindOf(err) != errs.KindNotFound {
				t.Errorf("Suspend: expected KindNotFound, got %v", err)
			}
			if _, err := l.Resume(ctx, spec); errs.KindOf(err) != errs.KindNotFound {
				t.Errorf("Resume: expected KindNotFound, got %v", err)
			}
			// Only Destroy treats absence as success: there, absence is the
			// desired result.
			if err := l.Destroy(ctx, spec.SandboxHandle); err != nil {
				t.Errorf("a Destroy of a nonexistent sandbox should be harmless: %v", err)
			}
		})

		t.Run("10_sandboxes_do_not_interfere", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			a, b := newSpec(t, l, env), newSpec(t, l, env)

			if _, err := l.Launch(ctx, a); err != nil {
				t.Fatalf("Launch A: %v", err)
			}
			if _, err := l.Launch(ctx, b); err != nil {
				t.Fatalf("Launch B: %v", err)
			}
			waitPhase(t, l, a.SandboxHandle, ports.PhaseActive, env.Ready)
			waitPhase(t, l, b.SandboxHandle, ports.PhaseActive, env.Ready)

			if err := l.Destroy(ctx, a.SandboxHandle); err != nil {
				t.Fatalf("Destroy A: %v", err)
			}
			waitGone(t, l, a.SandboxHandle, env.Ready)

			st, err := l.Describe(ctx, b.SandboxHandle)
			if err != nil {
				t.Fatalf("destruir A afetou B: %v", err)
			}
			if st.Phase != ports.PhaseActive {
				t.Fatalf("B left the active phase because of A: %s", st.Phase)
			}
		})

		t.Run("11_tail_dies_along_with_the_client", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- l.Tail(ctx, spec.SandboxHandle, ports.LogQuery{Follow: true},
					func(ports.LogLine) error { return nil })
			}()
			// Let the follow catch the stream before pulling the rug.
			time.Sleep(500 * time.Millisecond)
			cancel()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the client's cancellation is not a failure: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("Tail did not die with the client — a leaked goroutine per call")
			}
		})

		t.Run("12_an_emit_error_interrupts_the_tail", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			waitLog(t, l, spec.SandboxHandle, markerAbsent, env.Ready)

			boom := errs.Internal("cliente sumiu")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := l.Tail(ctx, spec.SandboxHandle, ports.LogQuery{Follow: true},
				func(ports.LogLine) error { return boom })
			if err == nil {
				t.Fatal("the emit error has to go up: it is how the server learns the client is gone")
			}
		})

		t.Run("empty_endpoints_with_no_published_port", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			st := waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			// An invented endpoint makes the cockpit offer a link that does not open.
			if len(st.Endpoints) != 0 {
				t.Fatalf("a sandbox with no published port returned %d endpoint(s): %+v",
					len(st.Endpoints), st.Endpoints)
			}
		})

		// ── Exec: guarantees 13 to 18 ───────────────────────────────────────
		//
		// They came in when exec came into the port, and each exists because the
		// corresponding naive implementation PASSES without them: an exec that
		// reads only the stream returns a zero exit code for every failed
		// command; one that trusts the core process's `Env` hands the process's
		// credential to the agent's code; one that reads to EOF with no cap
		// turns a `cat` of a log into an invoice.

		t.Run("13and14_exec_runs_inside_the_sandbox_and_in_the_workspace", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			// The test sandbox writes `marker` into the workspace when it comes
			// up; waiting for its line is waiting for the file to exist.
			waitLog(t, l, spec.SandboxHandle, markerAbsent, env.Ready)

			// `pwd` proves guarantee 14 (the working directory is the workspace,
			// and neither adapter received that as a parameter — both pin it on
			// the container). `cat marker` with no absolute path proves both at
			// once: it only works if the command started there.
			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "pwd; cat marker"},
			})
			if res.ExitCode != 0 {
				t.Fatalf("exit %d, stderr=%q", res.ExitCode, res.Stderr)
			}
			if !strings.Contains(res.Stdout, ports.SandboxWorkspacePath) {
				t.Fatalf("the command did not start in %s: pwd said %q",
					ports.SandboxWorkspacePath, res.Stdout)
			}
			if !strings.Contains(res.Stdout, "ok") {
				t.Fatalf("the command did not see the sandbox's workspace: %q", res.Stdout)
			}
		})

		t.Run("15_the_exit_code_is_not_a_port_error", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// It is the guarantee that holds the whole tool loop up: the model
			// needs to SEE that the command failed in order to fix it. A
			// transport error in its place would erase the difference between
			// "the test failed" and "the substrate went down".
			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "exit 7"},
			})
			if res.ExitCode != 7 {
				t.Fatalf("EXIT CODE LOST: got %d, expected 7 — an exec that reads only the "+
					"stream returns 0 for everything, and the agent reads failure as success", res.ExitCode)
			}
			// And zero stays zero: without this half, an adapter that always
			// returned -1 would pass the one above.
			if ok := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"true"},
			}); ok.ExitCode != 0 {
				t.Fatalf("a successful command returned code %d", ok.ExitCode)
			}
		})

		t.Run("16_stdout_and_stderr_are_separate", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// Unlike Tail, where k8s merges the two and the port promises
			// nothing: in exec both substrates really separate them.
			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "echo STDOUT-LINE; echo STDERR-LINE 1>&2"},
			})
			if !strings.Contains(res.Stdout, "STDOUT-LINE") {
				t.Fatalf("stdout did not carry the stdout line: %q", res.Stdout)
			}
			if !strings.Contains(res.Stderr, "STDERR-LINE") {
				t.Fatalf("stderr did not carry the stderr line: %q", res.Stderr)
			}
			if strings.Contains(res.Stdout, "STDERR-LINE") {
				t.Fatalf("the two streams came back merged into stdout: %q", res.Stdout)
			}
		})

		t.Run("17_the_output_is_capped_and_the_deadline_respected", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// ~22 KB of output against a 1 KB cap. Plain shell on purpose:
			// `yes | head` kills the producer with SIGPIPE and would measure
			// something else.
			big := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c",
					"i=0; while [ $i -lt 2000 ]; do echo 0123456789; i=$((i+1)); done"},
				MaxOutputBytes: 1024,
			})
			if len(big.Stdout) > 1024 {
				t.Fatalf("OUTPUT WITH NO CAP: %d bytes came back against a cap of 1024. A "+
					"tool's output becomes model context, and context is an invoice (ADR-0011)",
					len(big.Stdout))
			}
			if !big.Truncated {
				t.Fatal("the output was cut and Truncated came back false: an agent that " +
					"concludes from cut output without knowing concludes wrongly")
			}
			// The exit code still comes back even with the output cut — it is
			// what proves the adapter kept draining the stream instead of
			// closing the connection at the cap.
			if big.ExitCode != 0 {
				t.Fatalf("with the output cut, the exit code was lost: %d", big.ExitCode)
			}

			inicio := time.Now()
			pendurado := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "sleep 60"}, TimeoutSeconds: 3,
			})
			if !pendurado.TimedOut {
				t.Fatal("the command did not finish within the deadline and TimedOut came back false")
			}
			if pendurado.ExitCode == 0 {
				t.Fatal("a hung command returned code 0: zero asserts success, and there was none")
			}
			if elapsed := time.Since(inicio); elapsed > 40*time.Second {
				t.Fatalf("the 3s deadline was ignored: the call took %s", elapsed)
			}
		})

		t.Run("18_exec_on_a_suspended_and_on_a_nonexistent_sandbox", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			cmd := ports.ExecRequest{Command: []string{"true"}}

			// Inexistente: NotFound, como Describe.
			if _, err := l.Exec(context.Background(), spec.SandboxHandle, cmd); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("exec on a nonexistent sandbox: expected KindNotFound, got %v", err)
			}

			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			if err := l.Suspend(context.Background(), spec.SandboxHandle); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseSuspended, env.Ready)

			// Suspended: a PRECONDITION, and never an invented exit code. A
			// substrate with no execution does not run a command, and saying
			// that is different from saying the command failed.
			res, err := l.Exec(context.Background(), spec.SandboxHandle, cmd)
			if err == nil {
				t.Fatalf("exec on a SUSPENDED sandbox returned a result: %+v", res)
			}
			if k := errs.KindOf(err); k != errs.KindPrecondition {
				t.Fatalf("exec on a suspended sandbox: expected KindPrecondition, got %s: %v", k, err)
			}
		})

		t.Run("exec_does_not_carry_the_cores_environment_inside", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// The core is the process that HAS the credentials — the model
			// provider's key comes out of the vault and lives in its memory
			// (ADR-0023). The sandbox runs agent code, which reads untrusted
			// content (substrate spec §6). An adapter that passed `os.Environ()`
			// to the exec — which is the shortest path and what an SDK would do
			// for convenience — would hand the two to each other, and nothing
			// would fail.
			const sentinela = "SENTINELA_DO_NUCLEO_NAO_PODE_ENTRAR_NO_SANDBOX"
			t.Setenv("DOP_SENTINELA_DE_CREDENCIAL", sentinela)

			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "env; echo ---; set"},
			})
			if strings.Contains(res.Stdout, sentinela) {
				t.Fatal("THE CORE PROCESS'S ENVIRONMENT LEAKED INTO THE SANDBOX: " +
					"that is where the agent provider's credential lives, and that is where " +
					"the code that reads untrusted content runs")
			}
		})

		t.Run("an_unknown_process_in_the_tail_is_not_found", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			err := l.Tail(context.Background(), spec.SandboxHandle,
				ports.LogQuery{Service: "does-not-exist"}, func(ports.LogLine) error { return nil })
			if errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("expected KindNotFound for a nonexistent process, got %v", err)
			}
		})

		// ── the project's documents (18 to 21) ───────────────────────────────

		t.Run("18_the_documents_are_readable_where_the_port_says", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			spec.Documents = []ports.SandboxFile{
				{Path: "spec.md", Content: []byte("# the demand's spec\n")},
				// A subdirectory is the case that separates a real design from
				// one that works only for flat files — on Kubernetes a
				// ConfigMap key cannot carry a `/`.
				{Path: "adr/0024.md", Content: []byte("a microVM per demand\n")},
			}
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			for _, want := range []struct{ path, content string }{
				{"spec.md", "# the demand's spec\n"},
				{"adr/0024.md", "a microVM per demand\n"},
			} {
				res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
					Command: []string{"cat", ports.SandboxDocumentsPath + "/" + want.path},
				})
				if res.ExitCode != 0 {
					t.Fatalf("cat %s: exit %d, stderr %q", want.path, res.ExitCode, res.Stderr)
				}
				if res.Stdout != want.content {
					t.Errorf("%s = %q, want %q", want.path, res.Stdout, want.content)
				}
			}
		})

		t.Run("18_with_no_documents_the_path_is_not_mounted", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env) // no Documents
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"ls", ports.SandboxDocumentsPath + "/spec.md"},
			})
			if res.ExitCode == 0 {
				t.Errorf("a project with no documents got a mount with content: %q", res.Stdout)
			}
		})

		t.Run("19_the_documents_are_not_writable_from_inside", func(t *testing.T) {
			// It is what keeps an artefact from existing with nobody having
			// recorded that it does: what the agent produces goes back through
			// the core, where it gets a version and an event (ADR-0006).
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			spec.Documents = []ports.SandboxFile{
				{Path: "spec.md", Content: []byte("original\n")},
			}
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			target := ports.SandboxDocumentsPath + "/spec.md"
			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "echo tampered > " + target},
			})
			if res.ExitCode == 0 {
				t.Fatal("the sandbox wrote over a document")
			}
			// And what is there is still the original — a failed write that
			// truncated the file would be worse than one that succeeded.
			back := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"cat", target},
			})
			if back.Stdout != "original\n" {
				t.Errorf("the document changed after the attempt: %q", back.Stdout)
			}
		})

		t.Run("20_resume_rebuilds_the_documents_from_the_new_spec", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			spec.Documents = []ports.SandboxFile{
				{Path: "spec.md", Content: []byte("first version\n")},
				{Path: "gone.md", Content: []byte("this one leaves\n")},
			}
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			if err := l.Suspend(context.Background(), spec.SandboxHandle); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseSuspended, env.Ready)

			// The project moved on while the sandbox slept.
			spec.Documents = []ports.SandboxFile{
				{Path: "spec.md", Content: []byte("second version\n")},
			}
			if _, err := l.Resume(context.Background(), spec); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"cat", ports.SandboxDocumentsPath + "/spec.md"},
			})
			if res.Stdout != "second version\n" {
				t.Errorf("it came back with an outdated document: %q", res.Stdout)
			}
			// What left the project has to leave the sandbox: a document the
			// agent reads and nobody maintains is worse than no document.
			old := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"cat", ports.SandboxDocumentsPath + "/gone.md"},
			})
			if old.ExitCode == 0 {
				t.Errorf("a removed document survived the resume: %q", old.Stdout)
			}
		})

		t.Run("21_a_document_path_that_escapes_is_refused", func(t *testing.T) {
			// The list comes from a project's data, and whoever names a file
			// chooses the name.
			for _, bad := range []string{"/etc/passwd", "../escape.md", "", "a/../../b"} {
				l, env := newLauncher(t)
				spec := newSpec(t, l, env)
				spec.Documents = []ports.SandboxFile{{Path: bad, Content: []byte("x")}}

				_, err := l.Launch(context.Background(), spec)
				if errs.KindOf(err) != errs.KindInvalid {
					t.Errorf("path %q was accepted (err=%v)", bad, err)
				}
			}
		})
	})
}

// ── o sandbox de teste ───────────────────────────────────────────────────────

// The two phrases the test sandbox prints when it comes up. They are guarantee
// 5's instrument: the marker only exists in the workspace, so the second phrase
// only appears if the workspace survived the suspension.
const (
	markerAbsent  = "MARKER=absent"
	markerPresent = "MARKER=written"
)

// sandboxCommand prints the marker's state, writes it and stays alive.
//
// The `[infra]` at the end is on purpose: it is the domain's log classification
// convention, and seeing it cross the whole substrate proves neither adapter
// touches the line's text.
func sandboxCommand() []string {
	return []string{"sh", "-c",
		"if [ -f " + ports.SandboxWorkspacePath + "/marker ]; then echo " + markerPresent +
			"; else echo " + markerAbsent + "; fi; " +
			"echo ok > " + ports.SandboxWorkspacePath + "/marker; " +
			"echo '[infra] sandbox pronto'; sleep 900"}
}

// newSpec builds a fresh sandbox and registers the cleanup. Every subtest gets
// its own: a namespace shared between tests would hide precisely the
// interference guarantee 10 exists to detect.
func newSpec(t *testing.T, l ports.SandboxLauncher, env SandboxEnv) ports.SandboxSpec {
	t.Helper()
	id := randomID()
	spec := ports.SandboxSpec{
		SandboxHandle: ports.SandboxHandle{
			ID:        id,
			Namespace: env.NamespacePrefix + "-" + id,
		},
		AccountID: "contract-account",
		DemandID:  "demanda-" + id,
		Tier:      env.Tier,
		Image:     env.Image,
		Command:   sandboxCommand(),
		Env:       map[string]string{"DOP_SANDBOX_ID": id},
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = l.Destroy(ctx, spec.SandboxHandle)
	})
	return spec
}

// execOK runs a command and requires the PORT not to have failed.
//
// Note what it does NOT check: the exit code. An error here is a substrate
// failure; what the command did — failing included — is the caller's business.
// Mixing the two in this helper would erase guarantee 15 from every subtest that
// uses it.
func execOK(t *testing.T, l ports.SandboxLauncher, h ports.SandboxHandle, req ports.ExecRequest) *ports.ExecResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := l.Exec(ctx, h, req)
	if err != nil {
		t.Fatalf("Exec(%v): %v", req.Command, err)
	}
	if res == nil {
		t.Fatalf("Exec(%v) returned a nil result with no error", req.Command)
	}
	return res
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func containsTier(list []ports.IsolationTier, want ports.IsolationTier) bool {
	for _, t := range list {
		if t == want {
			return true
		}
	}
	return false
}

// ── waiting ──────────────────────────────────────────────────────────────────
//
// Provisioning is asynchronous in both substrates: Launch returns when the
// request was accepted, not when the process came up. Waiting here, and not
// inside the adapter, is what keeps the port non-blocking for real callers.

func waitPhase(t *testing.T, l ports.SandboxLauncher, h ports.SandboxHandle, want ports.SandboxPhase, d time.Duration) *ports.SandboxStatus {
	t.Helper()
	if d <= 0 {
		d = 90 * time.Second
	}
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		st, err := l.Describe(context.Background(), h)
		if err != nil {
			last = err.Error()
		} else {
			last = string(st.Phase)
			if st.Phase == want {
				return st
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the sandbox did not reach %q in %s (last: %s)", want, d, last)
	return nil
}

func waitGone(t *testing.T, l ports.SandboxLauncher, h ports.SandboxHandle, d time.Duration) {
	t.Helper()
	if d <= 0 {
		d = 90 * time.Second
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := l.Describe(context.Background(), h); errs.KindOf(err) == errs.KindNotFound {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the sandbox went on existing %s after being destroyed", d)
}

// waitLog waits for a phrase to appear in the log. It uses Follow=false on
// purpose: it is a read of what the substrate ALREADY has, which is what matters
// for proving workspace persistence.
func waitLog(t *testing.T, l ports.SandboxLauncher, h ports.SandboxHandle, want string, d time.Duration) {
	t.Helper()
	if d <= 0 {
		d = 90 * time.Second
	}
	deadline := time.Now().Add(d)
	var seen []string
	for time.Now().Before(deadline) {
		seen = seen[:0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := l.Tail(ctx, h, ports.LogQuery{Follow: false}, func(line ports.LogLine) error {
			seen = append(seen, line.Text)
			return nil
		})
		cancel()
		if err == nil {
			for _, line := range seen {
				if strings.Contains(line, want) {
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("the phrase %q did not appear in the log in %s (lines seen: %v)", want, d, seen)
}
