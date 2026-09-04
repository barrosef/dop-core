// Package verification implements the VerificationRunner port (ADR-0030): the
// ephemeral environment that pulls a COMMIT, builds the application from source
// and runs the declared checks against it.
//
// Two adapters share everything that matters. The sequence is not driven from
// the core — it is a script that runs as the container's main process, emitting
// one marked line per step. That choice is what makes a run survive a core
// restart: the container IS the run, and its log is the record. The adapters
// only know how to create an environment and how to read a log.
package verification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// WorkDir is where the commit is cloned, inside the runner. Every step runs
// with this as its working directory.
const WorkDir = "/work"

// ReadyAttempts is how many one-second tries a dependency — or the application
// — gets before the run gives up on it.
//
// It is a variable and not a constant for one reason: the contract suite has to
// prove that a dependency which never answers FAILS THE RUN NAMING IT, and
// proving that at two minutes per case would make the suite something nobody
// runs. Production never touches it.
var ReadyAttempts = 120

// tokenPath holds the git credential during the clone. It is a file and not a
// bare environment variable so that a build dumping `env` into its log does not
// carry the token with it — the same reason the sandbox uses a file. It is NOT
// a security boundary here: the code being built runs in this container and
// could read the file. The boundary is that the token opens this project's
// repository and nothing else.
const tokenPath = "/tmp/.dop-git-token"

// marker prefixes the lines the script emits so Status can find them.
//
// It carries a NONCE derived from the run's ID: the application's own output
// shares this stdout, and a build that happens to print something shaped like a
// result must not be able to forge one.
func marker(h ports.RunnerHandle) string {
	sum := sha256.Sum256([]byte("dop-runner-step/" + h.ID))
	return "DOP-STEP-" + hex.EncodeToString(sum[:8])
}

// Script generates the whole run, in POSIX shell.
//
// It is one script and not a sequence of exec calls because a step that fails
// has to stop the ones after it, and "stop the sequence" is a property of the
// sequence — expressing it as a race between the core and a container is how a
// check ends up running against an application that never started.
func Script(spec ports.RunnerSpec) string {
	m := marker(spec.RunnerHandle)
	var b strings.Builder

	fmt.Fprintf(&b, `set -u
M=%q
emit() { echo "$M $1"; }
now() { date -u +%%s; }
fatal() { s=$1; n=$2; shift 2; echo "runner: $*" >&2; emit "step {\"name\":\"$n\",\"kind\":\"setup\",\"exit\":1,\"start\":$s,\"end\":$(now)}"; emit done; exit 1; }
`, m)

	// Step 0 — the tools the script itself needs. A missing tool has to be a
	// named refusal: "git: not found" three steps later, inside a build's log,
	// costs somebody an afternoon.
	b.WriteString(`S=$(now)
command -v git >/dev/null 2>&1 || fatal $S setup "this image has no git — a runner image has to provide git and nc"
command -v nc  >/dev/null 2>&1 || fatal $S setup "this image has no nc — it is how the runner waits for a dependency"
`)

	// The credential helper, then the clone. The token never reaches the command
	// line, so it does not show up in a process list inside the container.
	if spec.Repository.Token != "" {
		// Two ways in, because the substrates deliver it differently: Kubernetes
		// projects a Secret as a FILE (a variable would show up in `env`, and a
		// build that dumps its environment would carry the credential into its
		// log), while Docker has no projection and passes it in the environment.
		// The script normalizes both into one file before anything else runs.
		fmt.Fprintf(&b, `if [ -n "${DOP_GIT_TOKEN_FILE:-}" ]; then
  cat "$DOP_GIT_TOKEN_FILE" > %s
else
  printf '%%s' "${DOP_GIT_TOKEN:-}" > %s
fi
chmod 600 %s
unset DOP_GIT_TOKEN
git config --global credential.helper '!f(){ echo username=dop; echo "password=$(cat %s)"; }; f'
`, tokenPath, tokenPath, tokenPath, tokenPath)
	}
	fmt.Fprintf(&b, `git config --global advice.detachedHead false
git clone --quiet %q %s >/dev/null 2>&1 || fatal $S setup "clone failed: %s"
cd %s || fatal $S setup "the clone produced no working tree"
git checkout --quiet %q >/dev/null 2>&1 || fatal $S setup "commit %s is not in this repository"
`, spec.Repository.CloneURL, WorkDir, spec.Repository.CloneURL, WorkDir, spec.Repository.Commit, spec.Repository.Commit)

	// The dependencies. Readiness is a TCP connection, plus the optional command
	// the project declared. A dependency that never answers fails the run HERE,
	// with its name — not as a connection refused buried in the application's
	// log twenty seconds later.
	for _, d := range spec.Dependencies {
		// ALWAYS localhost, on both substrates: on Kubernetes the dependency is a
		// container of the same pod, and on Docker it joins the runner's network
		// namespace. If one of them resolved a name instead, the application's
		// configuration would have to know which substrate it was on — which is
		// the compose translation problem coming back in through the window.
		const host = "127.0.0.1"
		fmt.Fprintf(&b, `i=0
while ! nc -z -w1 %s %d 2>/dev/null; do
  i=$((i+1)); [ $i -ge %d ] && fatal $S setup "dependency %q never accepted a connection on %s:%d"
  sleep 1
done
`, host, d.Port, ReadyAttempts, d.Name, host, d.Port)
		if strings.TrimSpace(d.Ready) != "" {
			fmt.Fprintf(&b, `i=0
until %s >/dev/null 2>&1; do
  i=$((i+1)); [ $i -ge %d ] && fatal $S setup "dependency %q never became ready: %s"
  sleep 1
done
`, d.Ready, ReadyAttempts, d.Name, d.Ready)
		}
	}
	b.WriteString(emitStep("setup", "setup", "0"))

	// The declared steps. `start` is the only one that does not block: the
	// application has to be up WHILE the checks run.
	hasCheck := false
	for _, st := range spec.Steps {
		switch st.Kind {
		case ports.StepStart:
			fmt.Fprintf(&b, `S=$(now)
( %s ) &
APP=$!
i=0
while ! nc -z -w1 127.0.0.1 %d 2>/dev/null; do
  i=$((i+1))
  if ! kill -0 $APP 2>/dev/null; then fatal $S %q "the application exited before answering on port %d"; fi
  [ $i -ge %d ] && fatal $S %q "the application never answered on port %d"
  sleep 1
done
`, st.Command, spec.AppPort, st.Name, spec.AppPort, ReadyAttempts, st.Name, spec.AppPort)
			b.WriteString(emitStep(st.Name, string(st.Kind), "0"))
		default:
			if st.Kind == ports.StepCheck {
				hasCheck = true
			}
			fmt.Fprintf(&b, `S=$(now)
( %s )
C=$?
`, st.Command)
			b.WriteString(emitStep(st.Name, string(st.Kind), "$C"))
			// A failing step stops the sequence. Everything after it did not run,
			// and does not get to look like it passed.
			b.WriteString("if [ $C -ne 0 ]; then emit done; exit $C; fi\n")
		}
	}

	// No checks means a dev session: the run holds until somebody destroys it.
	// With checks, the sequence is over and the container exits — the log stays,
	// which is what Status reads.
	if !hasCheck && spec.AppPort > 0 {
		b.WriteString("emit holding\nwait $APP\n")
	} else {
		b.WriteString("emit done\n")
	}
	return b.String()
}

// emitStep writes the shell line that reports one step's result.
//
// The names go in through this function and NOT through %q, and the difference
// is not cosmetic: %q produces real double quotes, the shell eats them inside an
// already-quoted string, and the marked line comes out as `{"name":aaa}` —
// invalid JSON, silently skipped by the parser. The failing step then becomes
// invisible and the run reports success. It cost one contract-suite run to find,
// and it would have cost a production green that meant nothing.
func emitStep(name, kind, exit string) string {
	return fmt.Sprintf(
		"emit \"step {\\\"name\\\":\\\"%s\\\",\\\"kind\\\":\\\"%s\\\",\\\"exit\\\":%s,\\\"start\\\":$S,\\\"end\\\":$(now)}\"\n",
		shellSafe(name), shellSafe(kind), exit)
}

// shellSafe keeps a step's name from breaking out of the emitted line. Names
// come from the project's manifest, and a manifest is written by whoever the
// platform is running code for.
func shellSafe(s string) string {
	r := strings.NewReplacer(`"`, "", `\`, "", "$", "", "`", "", "\n", " ")
	return r.Replace(s)
}

// ─────────────────────────── reading the log back ───────────────────────────

type stepLine struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Exit  int    `json:"exit"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

// ParseLog turns a run's stdout into its status.
//
// Anything that is not a marked line is the build's own output and is ignored
// here — it reaches the user through Logs, whole.
func ParseLog(h ports.RunnerHandle, log string, alive bool) *ports.RunnerStatus {
	m := marker(h)
	st := &ports.RunnerStatus{Phase: ports.RunnerPending}
	var done, holding bool

	for _, raw := range strings.Split(log, "\n") {
		i := strings.Index(raw, m+" ")
		if i < 0 {
			continue
		}
		rest := raw[i+len(m)+1:]
		switch {
		case rest == "done":
			done = true
		case rest == "holding":
			holding = true
		case strings.HasPrefix(rest, "step "):
			var sl stepLine
			if json.Unmarshal([]byte(rest[len("step "):]), &sl) != nil {
				continue
			}
			st.Steps = append(st.Steps, ports.RunnerStepResult{
				Name:      sl.Name,
				Kind:      ports.RunnerStepKind(sl.Kind),
				ExitCode:  sl.Exit,
				StartedAt: time.Unix(sl.Start, 0).UTC(),
				EndedAt:   time.Unix(sl.End, 0).UTC(),
			})
		}
	}
	sort.SliceStable(st.Steps, func(i, j int) bool {
		return st.Steps[i].StartedAt.Before(st.Steps[j].StartedAt)
	})

	for _, s := range st.Steps {
		if s.Failed() {
			st.Phase, st.FailedStep = ports.RunnerFailed, s.Name
			return st
		}
	}
	switch {
	case holding && alive:
		st.Phase = ports.RunnerHolding
	case done:
		st.Phase = ports.RunnerSucceeded
	case !alive:
		// The container is gone and never said it finished: it was killed, or it
		// died in a way the script could not report. Calling that success would
		// be the worst possible lie for a verification.
		st.Phase, st.FailedStep = ports.RunnerFailed, "the run ended without finishing its sequence"
	case len(st.Steps) > 0:
		st.Phase = ports.RunnerRunning
	}
	return st
}
