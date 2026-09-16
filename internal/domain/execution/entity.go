// Package execution is the EXECUTOR domain: where and how a demand executes.
//
// House rule: this package knows nothing of Kubernetes, Docker, Postgres or
// gRPC. It declares what it needs as a PORT (repository.go, plus
// ports.SandboxLauncher) and the composition root wires it.
//
// The distinction that organizes everything here is between the TWO things a
// sandbox has:
//
//   - the EXECUTION — the pod, the container, the running agent. Ephemeral by
//     nature, it costs money while it exists and is cheap to recreate;
//   - the WORKSPACE — the worktrees of the demand's branches. It is the work, and
//     it does not get recreated: redoing it costs the agent's time and the time
//     of the human who reviewed it.
//
// Suspending tears down the first and preserves the second. Destroying takes
// both. That difference lives in the MODEL (see Transition), not only in the
// method names: a wrong name the compiler accepts, a violated invariant it
// refuses.
package execution

import (
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// State is the sandbox's life cycle: active → suspended → destroyed (spec §3).
type State string

const (
	StateProvisioning State = "provisioning"
	StateActive       State = "active"
	StateSuspended    State = "suspended"
	StateDestroyed    State = "destroyed"
)

func ValidState(s State) bool {
	switch s {
	case StateProvisioning, StateActive, StateSuspended, StateDestroyed:
		return true
	}
	return false
}

// Transition describes what a state change does to the execution and to the
// workspace — the two things a sandbox has.
//
// This exists so the difference between suspending and destroying is a
// QUERYABLE FACT, not a detail every caller has to remember. It is the same
// reason EffectiveLevel lives in the resource domain: the rule that separates
// "saving money" from "irreversible loss" is too expensive to live scattered.
type Transition struct {
	To State
	// StopsRuntime: the execution stops. True in both — which is precisely what
	// makes them LOOK the same from outside.
	StopsRuntime bool
	// DiscardsWorkspace: the demand's work goes with it. THIS is where suspending
	// and destroying stop being the same operation.
	DiscardsWorkspace bool
	// Reversible: there is a way back through the domain itself.
	Reversible bool
}

var (
	// SuspendTransition is saving money: with no agent work and no dev connected,
	// the pod dies and the workspace survives on the PVC. Demands wait on humans
	// for hours — an idle sandbox is what separates real parallelism from a
	// drowning machine (spec §3).
	SuspendTransition = Transition{
		To: StateSuspended, StopsRuntime: true, DiscardsWorkspace: false, Reversible: true,
	}
	// DestroyTransition is deliberate loss: it takes the execution AND the
	// workspace, and does not come back. No transition leaves destroyed — the
	// invariant is enforced by a database trigger (migration 0010), because a
	// rule nobody may violate cannot depend on every code path remembering it.
	DestroyTransition = Transition{
		To: StateDestroyed, StopsRuntime: true, DiscardsWorkspace: true, Reversible: false,
	}
	// ResumeTransition recreates the execution OVER the existing workspace.
	ResumeTransition = Transition{
		To: StateActive, StopsRuntime: false, DiscardsWorkspace: false, Reversible: true,
	}
)

// IsTerminal: destroyed is absorbing. After it the sandbox is only history.
func (s State) IsTerminal() bool { return s == StateDestroyed }

// PreservesWork answers the question the user actually asks before clicking:
// "do I lose what has already been done?".
func (t Transition) PreservesWork() bool { return !t.DiscardsWorkspace }

// CanApply says whether the transition is legitimate from the current state.
//
// The order of the clauses is the rule:
//
//  1. nothing leaves destroyed. That is what irreversible means;
//  2. suspending only makes sense from active — suspending what is already
//     suspended is a repeat, treated as a no-op by the service, not as an error;
//  3. resuming requires a live workspace, that is, a suspended sandbox;
//  4. destroying is valid from any non-terminal state: destroying is always
//     possible.
func CanApply(from State, t Transition) bool {
	if from.IsTerminal() {
		return false
	}
	switch t.To {
	case StateSuspended:
		return from == StateActive
	case StateActive:
		return from == StateSuspended
	case StateDestroyed:
		return true
	}
	return false
}

// Sandbox is the executor's unit: one active demand, one sandbox (spec §1).
type Sandbox struct {
	ID        string
	AccountID string
	DemandID  string
	State     State
	// Tier is what was DELIVERED, not what was asked for. The client sees what it
	// received (spec §2) — which is why it is stored, not recomputed on read.
	Tier           ports.IsolationTier
	Namespace      string
	Endpoints      []Endpoint
	IdempotencyKey string
	LastActiveAt   time.Time
	SuspendedAt    time.Time
	DestroyedAt    time.Time
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Endpoint is a service of the demand's stack exposed to the dev.
type Endpoint struct {
	Name  string
	URL   string
	Port  int32
	State string // running | stopped
}

// Handle builds the reference the launcher understands.
func (s Sandbox) Handle() ports.SandboxHandle {
	return ports.SandboxHandle{ID: s.ID, Namespace: s.Namespace}
}

// IsLive: the sandbox still occupies room in the executor (execution, workspace, or both).
func (s Sandbox) IsLive() bool { return !s.State.IsTerminal() }

// IdleFor says how long the sandbox has been unused. It is the number the idle
// sweeper compares with IdleTimeout.
func (s Sandbox) IdleFor(now time.Time) time.Duration {
	if s.LastActiveAt.IsZero() {
		return 0
	}
	return now.Sub(s.LastActiveAt)
}

// IdleTimeout: with no agent work and no dev connected for this long, the
// sandbox suspends (spec §3). It lives in the domain because it is product
// policy — the adapter has no opinion about how long "idle" is.
const IdleTimeout = 30 * time.Minute

// ShouldSuspend is the saving policy as a pure function, testable with no
// executor at all.
func (s Sandbox) ShouldSuspend(now time.Time) bool {
	return s.State == StateActive && s.IdleFor(now) >= IdleTimeout
}

// ── namespace ────────────────────────────────────────────────────────────────

// NamespaceFor derives the demand's namespace: dop-<short-id>.
//
// Hierarchical identification (account, workspace, project, demand) is a LABEL,
// not a name (spec §1) — the name carries only what has to be unique and short,
// because a namespace name is limited to 63 characters in DNS-1123 and a full id
// with hyphens already eats half of that.
const namespaceIDLen = 8

func NamespaceFor(demandID string) string {
	short := strings.ToLower(strings.ReplaceAll(demandID, "-", ""))
	if len(short) > namespaceIDLen {
		short = short[:namespaceIDLen]
	}
	return "dop-" + short
}

// ── log classification ───────────────────────────────────────────────────────

// Source classifies where a log line inside the sandbox comes from.
type Source string

const (
	SourceApp   Source = "app"
	SourceTest  Source = "test"
	SourceInfra Source = "infra"
)

// TestType is the kind of test, when the line comes from a test run.
type TestType string

const (
	TestAAA         TestType = "aaa"
	TestE2E         TestType = "e2e"
	TestIntegration TestType = "integration"
)

// LogLine is the ALREADY classified line, the one that leaves through the RPC.
type LogLine struct {
	Source   Source
	Service  string
	TestType TestType
	Text     string
	At       time.Time
}

// Classify reads the convention prefix of a raw line from the executor.
//
// The convention lives HERE, and not in the adapter, for a practical reason:
// there are two adapters and one domain. Placed on the far side, the same prefix
// rule would exist twice and would diverge on the first adjustment — and the
// contract suite would not catch it, because log classification is not a
// executor guarantee.
//
// Accepted format, at the start of the line: "[app]", "[infra]", "[test:e2e]".
// A line with no prefix is infra: it is what the executor itself printed.
func Classify(raw string) (Source, TestType, string) {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "[") {
		return SourceInfra, "", raw
	}
	end := strings.IndexByte(text, ']')
	if end < 0 {
		return SourceInfra, "", raw
	}
	tag := text[1:end]
	rest := strings.TrimLeft(text[end+1:], " ")

	src, tt := tag, ""
	if colon := strings.IndexByte(tag, ':'); colon >= 0 {
		src, tt = tag[:colon], tag[colon+1:]
	}
	switch Source(src) {
	case SourceApp, SourceTest, SourceInfra:
		return Source(src), TestType(tt), rest
	}
	// A bracket that is not our tag: the line belongs to the process itself, intact.
	return SourceInfra, "", raw
}

// LogFilter is the slice the client asked for in StreamLogs.
type LogFilter struct {
	Source   Source
	Service  string
	TestType TestType
}

// Matches applies the filter. An empty field does not filter — asking for
// everything is the common case.
func (f LogFilter) Matches(l LogLine) bool {
	if f.Source != "" && f.Source != l.Source {
		return false
	}
	if f.TestType != "" && f.TestType != l.TestType {
		return false
	}
	if f.Service != "" && f.Service != l.Service {
		return false
	}
	return true
}

// ── validation ───────────────────────────────────────────────────────────────

// RequireTier is the rule the product owner asked for in writing: the isolation
// tier is DECLARED, never presumed.
//
// A caller that does not say the tier is REFUSED — it does not get a default.
// Choosing for them would be deciding, on somebody else's behalf, how much
// isolation their workload deserves; and the error of that choice only shows up
// once it has already cost dearly.
func RequireTier(t ports.IsolationTier) error {
	if t == ports.TierUnspecified {
		return errs.Invalid(
			"isolation tier not declared: provide min_tier (hardware, " +
				"kernel_emulated or namespace) — the executor does not choose for you")
	}
	if !ports.ValidIsolationTier(t) {
		return errs.Invalid("unknown isolation tier: %q", t)
	}
	return nil
}

// EndpointURL builds a demand service's public URL:
// <service>--<short-demand>.<domain> (spec §5).
//
// It lives in the domain because the naming is product policy: if each adapter
// built it, the same rule would exist in two places and the dev would see
// different URLs depending on where the sandbox came up.
func EndpointURL(baseDomain, demandID, service string) string {
	if baseDomain == "" || service == "" {
		return ""
	}
	short := strings.TrimPrefix(NamespaceFor(demandID), "dop-")
	return "https://" + service + "--" + short + "." + baseDomain
}
