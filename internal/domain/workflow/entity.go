// Package workflow is the domain of the work flow: the typed sequence of stages
// a demand goes through, and the inheritance chain that decides WHICH sequence
// applies to each demand (ADR-0014).
//
// House rule: this package knows nothing of Postgres, gRPC or any SDK. It
// declares what it needs as a PORT (repository.go) and the composition root
// wires it.
//
// Two invariants hold everything here up:
//
//  1. A VERSION IS IMMUTABLE. Updating a flow CREATES a new version; the
//     previous one stays as it is, forever. A demand in progress points at the
//     version it froze on start (ADR-0014 §4) — changing that version in place
//     would rewrite its past, and the history would stop explaining what
//     happened.
//  2. RESOLVING IS OVERLAYING, WITH PROVENANCE. A demand's effective flow is the
//     overlay of the chain platform ◁ account ◁ workspace ◁ project ◁ demand.
//     Keeping only the result would throw away the one piece of information that
//     answers "why did this demand follow a flow nobody remembers writing".
package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/reaction"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── the chain's scopes ───────────────────────────────────────────────────────

// Scope is one level of the inheritance chain (ADR-0014 §3).
type Scope string

const (
	ScopePlatform  Scope = "platform"
	ScopeAccount   Scope = "account"
	ScopeWorkspace Scope = "workspace"
	ScopeProject   Scope = "project"
	ScopeDemand    Scope = "demand"
)

// Chain is the whole chain, from the MOST GENERIC to the MOST SPECIFIC. The
// order of this slice is the definition of inheritance: whoever comes later
// overlays whoever came before.
var Chain = []Scope{ScopePlatform, ScopeAccount, ScopeWorkspace, ScopeProject, ScopeDemand}

// Rank is the position in the chain; -1 for an unknown scope. It is what lets
// us say "above" and "below" without scattering switches everywhere.
func Rank(s Scope) int {
	for i, c := range Chain {
		if c == s {
			return i
		}
	}
	return -1
}

func ValidScope(s Scope) bool { return Rank(s) >= 0 }

// Label is the label that appears in the trail on screen.
//
// It is English developer text, NOT the translation: the trail is a sentence
// assembled in the core and shown to a person, which means the cockpit should be
// composing it from structured data instead. That is recorded as pending work —
// until then, this reads in one language, and English is the one the code speaks.
func (s Scope) Label() string {
	switch s {
	case ScopePlatform:
		return "platform"
	case ScopeAccount:
		return "account"
	case ScopeWorkspace:
		return "workspace"
	case ScopeProject:
		return "project"
	case ScopeDemand:
		return "demand"
	}
	return string(s)
}

// ScopeRef addresses ONE concrete level of the chain. An empty ID is only
// legitimate at the platform level, which is unique by definition.
type ScopeRef struct {
	Scope Scope
	ID    string
}

func (r ScopeRef) String() string {
	if r.ID == "" {
		return r.Scope.Label()
	}
	return r.Scope.Label() + " " + r.ID
}

// ── the stages' vocabulary ───────────────────────────────────────────────────

// StageType is the stage's semantic type. The vocabulary belongs to the
// PLATFORM: the type decides the renderer on screen and the agent's behaviour
// (which artifact to produce, where to stop). A new type requires the platform
// to evolve; a new composition does not (ADR-0014 §1) — and that is why an
// unknown type is a contract error, not user data.
type StageType string

const (
	TypeContext         StageType = "context"
	TypeSpec            StageType = "spec"
	TypePlan            StageType = "plan"
	TypeImplementation  StageType = "implementation"
	TypeTest            StageType = "test"
	TypeHumanValidation StageType = "human_validation"
	TypeFinalization    StageType = "finalization"
	TypeGeneric         StageType = "generic"
)

func ValidStageType(t StageType) bool {
	switch t {
	case TypeContext, TypeSpec, TypePlan, TypeImplementation,
		TypeTest, TypeHumanValidation, TypeFinalization, TypeGeneric:
		return true
	}
	return false
}

// ArtifactKind is what the stage puts on the table: produced by the agent or
// signed off by the human. It is what the screen renders when the stage opens.
type ArtifactKind string

const (
	ArtifactDocument ArtifactKind = "document"
	ArtifactSpec     ArtifactKind = "spec"
	ArtifactPlan     ArtifactKind = "plan"
	ArtifactTestPlan ArtifactKind = "test_plan"
	ArtifactDiagram  ArtifactKind = "diagram"
	ArtifactReport   ArtifactKind = "report"
)

func ValidArtifactKind(a ArtifactKind) bool {
	switch a {
	case ArtifactDocument, ArtifactSpec, ArtifactPlan,
		ArtifactTestPlan, ArtifactDiagram, ArtifactReport:
		return true
	}
	return false
}

// GateKind says whether the stage STOPS for somebody to decide. Two values, on
// purpose: a gate with a conditional rule is a workflow engine, explicitly out
// of v1 (ADR-0014, alternatives considered).
type GateKind string

const (
	GateNone  GateKind = "none"
	GateHuman GateKind = "human"
)

func ValidGate(g GateKind) bool { return g == GateNone || g == GateHuman }

// StageMoment is when a stage's action fires.
//
// Both are derivable from the `from`/`to` the `dop.demand.stage.advanced` event
// already carries, so nothing changes in the emitter. Two moments and not one
// because the two real cases need both: "provision the bench when implementation
// ENDS" is an exit, and "tell the dev when the demand ENTERS spec" is an entry.
// Folding entry into "the exit of the previous stage" is correct and unreadable
// — and unreadable is fatal here, because the flow is authored through a chat.
type StageMoment string

const (
	MomentEnter StageMoment = "enter"
	MomentExit  StageMoment = "exit"
)

func ValidStageMoment(m StageMoment) bool { return m == MomentEnter || m == MomentExit }

// StageAction is what a stage asks the platform to do when it is entered or
// left. The vocabulary is the reaction domain's (reaction.ActionName), so a
// flow cannot ask for something no handler implements — the same closed list
// the outside-the-demand decider uses (P-29).
type StageAction struct {
	On     StageMoment
	Name   string
	Params map[string]string
}

// StageSpec is one stage. No conditionals, no parallelism, no DSL: v1 is
// deliberately a sequence (ADR-0014 §2).
type StageSpec struct {
	// Key is the stage's address. A demand's advance is an EVENT, and the event
	// points at the stage by key — which is why it is required and unique.
	Key       string
	Name      string // free-form, the author's
	Type      StageType
	Artifacts []ArtifactKind
	Gate      GateKind
	Subtypes  []string      // e.g. test → aaa, e2e, integration
	Actions   []StageAction // what fires on enter/exit — declaration, not a branch (ADR-0014)
}

// Flow is a FROZEN version of a flow at one level of the chain.
//
// Version is not metadata: it is part of the identity. Two versions of the same
// ID are two different documents, and the demand that pointed at version 3 keeps
// seeing version 3 after 4 is born.
type Flow struct {
	ID          string
	AccountID   string // empty ONLY in the platform catalogue, which has no owner
	OwnerScope  Scope
	OwnerID     string // empty at the platform level
	Name        string
	Description string
	Version     int32
	Stages      []StageSpec
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Origin is the provenance carried by a flow DERIVED from another account's
	// publication — nil for a flow written directly in this account.
	Origin *Origin
	// RevokedAt marks a derived copy whose grant was revoked under `drain` or
	// `terminate`. It is a state change, never a deletion: the adopter's own
	// edits and the audit of a demand that already ran under it have to survive.
	RevokedAt time.Time
}

func (f Flow) Ref() ScopeRef { return ScopeRef{Scope: f.OwnerScope, ID: f.OwnerID} }

// Normalize trims what came from the client and applies the only default there
// is: a stage with no declared gate does not stop. Demanding an explicit "none"
// on every stage would be noise — the silence already says what is expected.
func (f *Flow) Normalize() {
	f.Name = strings.TrimSpace(f.Name)
	f.Description = strings.TrimSpace(f.Description)
	for i := range f.Stages {
		f.Stages[i].Key = strings.TrimSpace(f.Stages[i].Key)
		f.Stages[i].Name = strings.TrimSpace(f.Stages[i].Name)
		if f.Stages[i].Gate == "" {
			f.Stages[i].Gate = GateNone
		}
		if f.Stages[i].Name == "" {
			f.Stages[i].Name = f.Stages[i].Key
		}
	}
}

// SameStages compares the executable content of two flows. It is what separates
// a real update from a resend — and a resend must not create a version,
// otherwise a client with automatic retries would version the flow forever.
func (f Flow) SameStages(other Flow) bool {
	if f.Name != other.Name || f.Description != other.Description ||
		len(f.Stages) != len(other.Stages) {
		return false
	}
	for i := range f.Stages {
		a, b := f.Stages[i], other.Stages[i]
		if a.Key != b.Key || a.Name != b.Name || a.Type != b.Type || a.Gate != b.Gate ||
			!sameArtifacts(a.Artifacts, b.Artifacts) || !sameStrings(a.Subtypes, b.Subtypes) ||
			!sameActions(a.Actions, b.Actions) {
			return false
		}
	}
	return true
}

// ── effective flow ───────────────────────────────────────────────────────────

// StageOrigin is ONE stage's provenance: which level of the chain the version
// that survived the overlay came from.
type StageOrigin struct {
	Key  string
	From ScopeRef
}

// EffectiveFlow is the chain's result — and the trail of how it was reached.
//
// Contributors carries the levels that actually declared something, from the
// most specific to the most generic; Origins answers stage by stage. Without
// those two fields support has no way to explain a flow nobody remembers writing
// (ADR-0014, consequences).
type EffectiveFlow struct {
	Flow         Flow
	Contributors []ScopeRef
	Origins      []StageOrigin
	ResolvedFrom string
}

// OriginOf returns where the stage came from.
func (e EffectiveFlow) OriginOf(key string) (ScopeRef, bool) {
	for _, o := range e.Origins {
		if o.Key == key {
			return o.From, true
		}
	}
	return ScopeRef{}, false
}

// MergeChain overlays the chain's flows. levels comes from the MOST GENERIC to
// the MOST SPECIFIC — the order is this function's contract.
//
// The rules, in full:
//
//   - a level that declares no stage at all INHERITS by omission (it is skipped
//     here);
//   - a stage whose key was already seen is REPLACED — the more specific level
//     wins;
//   - a stage with a new key goes to the END;
//   - the position belongs to whoever INTRODUCED the stage. Reordering what was
//     inherited is not expressible in v1: whoever needs a different order
//     declares the whole flow with their own keys. It is the same choice as
//     ADR-0014 §2 — the structure stays simple and what is missing evolves on top
//     of it, instead of being born as an ordering merge nobody can predict in
//     their head.
func MergeChain(levels []Flow) EffectiveFlow {
	var (
		eff     EffectiveFlow
		stages  []StageSpec
		posOf   = map[string]int{}
		fromOf  = map[string]ScopeRef{}
		sources []ScopeRef
	)
	for _, lv := range levels {
		if len(lv.Stages) == 0 {
			continue // inherits by omission
		}
		ref := lv.Ref()
		sources = append(sources, ref)

		// The effective flow's identity is that of the most specific level that
		// declared: it is the document the user opens to edit.
		eff.Flow.ID, eff.Flow.Version = lv.ID, lv.Version
		eff.Flow.AccountID = lv.AccountID
		eff.Flow.OwnerScope, eff.Flow.OwnerID = lv.OwnerScope, lv.OwnerID
		eff.Flow.Name, eff.Flow.Description = lv.Name, lv.Description
		eff.Flow.CreatedBy = lv.CreatedBy
		eff.Flow.CreatedAt, eff.Flow.UpdatedAt = lv.CreatedAt, lv.UpdatedAt

		for _, st := range lv.Stages {
			if i, seen := posOf[st.Key]; seen {
				stages[i] = st
			} else {
				posOf[st.Key] = len(stages)
				stages = append(stages, st)
			}
			fromOf[st.Key] = ref
		}
	}

	eff.Flow.Stages = stages
	eff.Origins = make([]StageOrigin, 0, len(stages))
	for _, st := range stages {
		eff.Origins = append(eff.Origins, StageOrigin{Key: st.Key, From: fromOf[st.Key]})
	}
	// From the most specific to the most generic: that is how the sentence reads
	// on screen.
	for i := len(sources) - 1; i >= 0; i-- {
		eff.Contributors = append(eff.Contributors, sources[i])
	}
	eff.ResolvedFrom = renderTrail(eff.Contributors, eff.Origins)
	return eff
}

// maxTrailStages bounds the per-stage detail: the trail is a sentence on a
// screen, not a report.
const maxTrailStages = 12

// renderTrail writes the visible trail: "project ◂ workspace ◂ account". When
// the stages come from different levels, the per-stage detail goes in with it —
// which is exactly the case where the question "where did THAT come from?"
// arises.
func renderTrail(sources []ScopeRef, origins []StageOrigin) string {
	if len(sources) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sources))
	for _, s := range sources {
		parts = append(parts, s.Scope.Label())
	}
	trail := strings.Join(parts, " ◂ ")
	if len(sources) == 1 {
		return trail
	}

	detail := make([]string, 0, len(origins))
	for i, o := range origins {
		if i == maxTrailStages {
			detail = append(detail, "…")
			break
		}
		detail = append(detail, fmt.Sprintf("%s (%s)", o.Key, o.From.Scope.Label()))
	}
	return trail + " — stages: " + strings.Join(detail, ", ")
}

// ── validation ───────────────────────────────────────────────────────────────

const (
	maxStages     = 50
	maxStageKey   = 40
	maxNameLength = 120
)

// Report is ValidateFlow's result: what BLOCKS and what merely bothers.
//
// The distinction is operational: an error refuses the write, a warning appears
// on screen and the author decides. A flow with no spec stage works — it is just
// usually an oversight.
//
// The messages here are free-form ENGLISH, and that is a known limit: they are
// read by the person writing the flow, so they ought to be translation keys with
// parameters. Doing that means changing the proto (`repeated string errors`) and
// regenerating both sides — work of its own, recorded as pending rather than
// half-done here.
type Report struct {
	Errors   []string
	Warnings []string
}

func (r Report) Valid() bool { return len(r.Errors) == 0 }

// Err turns the report into a domain error. Refusing an invalid flow is a CLIENT
// error, with the whole list: returning one problem at a time would make the
// author fix the same document seven times.
func (r Report) Err() error {
	if r.Valid() {
		return nil
	}
	return errs.Invalid("invalid flow: %s", strings.Join(r.Errors, "; "))
}

func (r *Report) errf(format string, a ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
}
func (r *Report) warnf(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// Validate refuses what would break a demand IN EXECUTION.
//
// Every rule's criterion is the same: is there a demand in progress for which
// this document has no answer? If there is, it is an error. Every message says
// WHICH stage and WHY — a validation message that does not locate the problem
// forces the author to guess.
//
// On "cycle" and "orphan stage": v1's structure has no edges (ADR-0014 §2), so
// the order is the sequence itself and the graph lives in the KEYS. A stage with
// no key is genuinely orphaned — the advance event addresses by key, and no
// event can point at it. A repeated key is the cycle — advancing by key goes
// back to a stage already passed and the screen's ruler never closes. Those are
// the two graph defects this structure is capable of having.
func Validate(f Flow) Report {
	var r Report

	if strings.TrimSpace(f.Name) == "" {
		r.errf("flow with no name: nobody can pick a flow on screen that is not called anything")
	}
	if len(f.Name) > maxNameLength {
		r.errf("the flow name is longer than %d characters", maxNameLength)
	}
	if len(f.Stages) == 0 {
		r.errf("flow with no stages at all: a demand adopting it would have nowhere to start")
		return r
	}
	if len(f.Stages) > maxStages {
		r.errf("a flow with %d stages exceeds the limit of %d", len(f.Stages), maxStages)
	}

	seen := make(map[string]int, len(f.Stages))
	hasSpec, hasGate := false, false

	for i, st := range f.Stages {
		where := fmt.Sprintf("%q", st.Key)
		if st.Key == "" {
			where = fmt.Sprintf("at position %d", i+1)
		}

		switch {
		case strings.TrimSpace(st.Key) == "":
			r.errf("stage %s is orphaned: a demand\u2019s advance is recorded BY KEY and it has no key — no event can point at that stage", where)
		case len(st.Key) > maxStageKey:
			r.errf("the key of stage %s is longer than %d characters", where, maxStageKey)
		case strings.ContainsAny(st.Key, " \t\n"):
			r.errf("the key of stage %s contains a space: the key travels in events and in message subjects, where a space does not survive", where)
		default:
			if prev, dup := seen[st.Key]; dup {
				r.errf("stage %q appears at positions %d and %d: that is a cycle — advancing by key would go back to a stage already passed and the demand\u2019s ruler would never close", st.Key, prev+1, i+1)
			} else {
				seen[st.Key] = i
			}
		}

		if !ValidStageType(st.Type) {
			r.errf("stage %s has an unknown type %q: the type decides the renderer on screen and the agent\u2019s behaviour, and the platform cannot execute a type that does not exist (ADR-0014 §1)", where, st.Type)
		}
		for _, a := range st.Artifacts {
			if !ValidArtifactKind(a) {
				r.errf("stage %s declares an unknown artifact %q: no screen knows how to render it", where, a)
			}
		}

		// A gate with no decider — the two shapes the structure allows.
		switch {
		case !ValidGate(st.Gate):
			r.errf("stage %s has an unknown gate %q: a gate can only be human or none", where, st.Gate)
		case st.Type == TypeHumanValidation && st.Gate != GateHuman:
			r.errf("stage %s is a human validation and has no gate: with no gate nobody is called to decide and the agent walks straight past — a validation that does not interrupt validates nothing", where)
		case st.Gate == GateHuman && len(st.Artifacts) == 0:
			r.errf("stage %s has a human gate and puts no artifact on the table: the decider opens the screen and there is nothing to decide about", where)
		}

		if len(st.Subtypes) > 0 && st.Type != TypeTest {
			r.warnf("stage %s declares substages outside a test stage: only the test type has a renderer for them today", where)
		}
		for _, sub := range st.Subtypes {
			if strings.TrimSpace(sub) == "" {
				r.errf("stage %s has a blank substage", where)
			}
		}

		// WHY a name may not repeat AT ONE MOMENT, so nobody "helpfully" relaxes
		// it: what marks an action as done is (event, rule_ref, action name),
		// and reaction.DecideStage builds rule_ref as
		// flow/version/stage/moment. Two actions sharing a name at one moment
		// therefore claim the SAME row — the first runs, the second is skipped
		// as already applied, forever, with no error anywhere. The moment is
		// part of rule_ref, which is why the scope is per moment and not per
		// stage: enter and exit are genuinely different rows.
		//
		// The alternative was an ordinal in that key. It was rejected because
		// it makes idempotency depend on ORDER: inserting an action in the
		// middle of the list would shift every later ordinal, make
		// already-applied actions look unapplied, and a redelivery would re-run
		// them.
		seenAction := map[string]bool{}
		for _, a := range st.Actions {
			if !ValidStageMoment(a.On) {
				r.errf("stage %s: %q is not a moment — use enter or exit", where, a.On)
			}
			if !reaction.ValidActionName(reaction.ActionName(a.Name)) {
				r.errf("stage %s: %q is not an action this platform implements", where, a.Name)
			}
			key := string(a.On) + "\x00" + a.Name
			if seenAction[key] {
				r.errf("stage %s declares %q twice on %s: what marks an action as done is "+
					"(event, stage, moment, action name), so the second one would be skipped "+
					"as already applied and would never run. The same name on the OTHER "+
					"moment is fine; to do %q twice at this one — to reach two recipients, "+
					"say — use two stages, or one action whose params name both",
					where, a.Name, a.On, a.Name)
			}
			seenAction[key] = true
		}

		if st.Type == TypeSpec {
			hasSpec = true
		}
		if st.Gate == GateHuman {
			hasGate = true
		}
	}

	if !hasSpec {
		r.warnf("flow with no spec stage: the agent will implement from the demand\u2019s statement, with no written acceptance criteria")
	}
	if !hasGate {
		r.warnf("flow with no human gate at all: the demand runs start to finish with nobody approving")
	}
	return r
}

// ── auxiliares ───────────────────────────────────────────────────────────────

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameArtifacts(a, b []ArtifactKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameActions compares a stage's declared actions — part of the EXECUTABLE
// content SameStages exists to detect a change in. Skipping it here would let
// somebody swap what a stage does on enter/exit without ever creating a new
// version, silently breaking invariant #1 at the top of this file.
func sameActions(a, b []StageAction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].On != b[i].On || a[i].Name != b[i].Name || !sameParams(a[i].Params, b[i].Params) {
			return false
		}
	}
	return true
}

func sameParams(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
