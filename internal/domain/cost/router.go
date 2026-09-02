package cost

import (
	"fmt"
	"strings"
)

// ════════════════════════════════════════════════════════════════════════════
// ModelRouter — the task → (model, effort) decision of ADR-0011 §3.
//
// THIS IS THE ADR'S DRAFT PART. The table below is an informed guess, not the
// result of measurement, and the code says so in three places on purpose: in the
// name of the provenance constant, in the justification that comes out of every
// decision, and in this comment.
//
// HOW TO RECALIBRATE (ROADMAP item P-7):
//
//	1. the telemetry comes from the cost events — cost_usage has model,
//	   input/output and the two cache fields per call, and SummarizeCost
//	   aggregates by scope and period;
//	2. change ROWS of `routingTable` and/or the `ModelCatalog`. Nothing else. If
//	   one day the decision needs an `if` outside here, the policy has stopped
//	   being a table and the change is one of design, not of calibration;
//	3. update `RoutingProvenance` so it stops saying "draft" — the justification
//	   that reaches the auditor changes with it, touching nothing else.
//
// Why a table and not a scattered heuristic: a policy born wrong has to be
// AUDITABLE and REPLACEABLE in one move. Fifteen `if`s spread across callers
// give the same answer today and are impossible to recalibrate tomorrow — and
// nobody can explain why an agent ran on the expensive model.
// ════════════════════════════════════════════════════════════════════════════

// TaskKind is the nature of the work. It is the decision's ONLY axis — ADR-0011
// explicitly discarded routing by prompt size: what matters is the task's
// nature, not its length.
type TaskKind string

const (
	// TaskMechanical: a commit, a log summary, a dossier, i18n. Work of form, not
	// of reasoning.
	TaskMechanical TaskKind = "mechanical"
	// TaskInvestigation: a subagent reading logs and doing forensics (ADR-0010).
	TaskInvestigation TaskKind = "investigation"
	// TaskImplementation: planning and writing the code.
	TaskImplementation TaskKind = "implementation"
	// TaskCritic: the ADR-0007 opinion. It is the flow's brake.
	TaskCritic TaskKind = "critic"
)

// ModelClass is a CLASS, not a model name. The leading model name changes every
// six months (ADR-0001) and does not survive in a policy table; the class is what
// the decision actually means. The concrete name comes from the catalogue.
type ModelClass string

const (
	ClassCheap  ModelClass = "cheap"  // the Haiku class
	ClassMedium ModelClass = "medium" // the Sonnet class
	ClassStrong ModelClass = "strong" // the Opus class
)

// Effort is the reasoning effort. It reduces preamble and tool calls WITHIN the
// class — the second axis, and the cheapest to adjust: changing effort does not
// change model, it only shortens the path to the answer.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// RoutingProvenance goes into EVERY justification. It exists so that whoever
// reads a decision in the audit log knows where it came from without opening the
// code — and so that the sentence "this has not been measured yet" is impossible
// to forget.
const RoutingProvenance = "ADR-0011 §3 (draft — calibrate with telemetry, P-7)"

// routingRule is a ROW of the decision table.
type routingRule struct {
	Kind   TaskKind
	Class  ModelClass
	Effort Effort
	// Why is the row's reason, not a restatement of what it does. Without it the
	// decision is not auditable: nobody can disagree with "haiku/low", but anybody
	// can disagree with "work of form does not pay for reasoning".
	Why string
}

// routingTable — THE TABLE. It mirrors ADR-0011 §3 row by row.
//
// A slice and not a map: the order is the ADR's, and reading the code side by
// side with the document has to be effortless. There are four rows; if one day
// there are forty, the problem is the policy, not the data structure.
var routingTable = []routingRule{
	{
		Kind: TaskMechanical, Class: ClassCheap, Effort: EffortLow,
		Why: "work of form (a commit, a log summary, a dossier, i18n) does not pay " +
			"for long reasoning; the spread between classes is ~5×",
	},
	{
		Kind: TaskInvestigation, Class: ClassMedium, Effort: EffortMedium,
		Why: "subagent forensics reads a lot and concludes little; the medium class " +
			"carries the volume without the strong class's price, and the finding is " +
			"what goes back to the main agent (ADR-0010)",
	},
	{
		Kind: TaskImplementation, Class: ClassStrong, Effort: EffortHigh,
		Why: "planning and implementing is where a mistake costs rework; saving " +
			"here gives the saving back in review",
	},
	{
		Kind: TaskCritic, Class: ClassStrong, Effort: EffortMax,
		// This row is the FIXED RULE of ADR-0011/0012, not a calibration point:
		// recalibrating the other three is expected, lowering this one is not.
		Why: "you do not save on the critic — it is the brake (ADR-0007); saving on " +
			"the brake gives the cost back as a rejected PR, the flow's most " +
			"expensive rework",
	},
}

// fallbackRule serves a kind of work outside the vocabulary.
//
// It falls to the EXPENSIVE side on purpose: an unknown kind is, by definition,
// work nobody classified, and erring cheap on work that needed reasoning costs
// rework — whereas erring expensive costs money and shows up in the measurement,
// which is exactly what exists here.
var fallbackRule = routingRule{
	Class: ClassStrong, Effort: EffortHigh,
	Why: "kind of work outside the vocabulary: when in doubt you do not save, and " +
		"the spend shows up in the measurement — the inverse does not",
}

// ModelCatalog resolves CLASS → concrete model name.
//
// Separate from the table because the two change for different reasons and at
// different rates: the policy (which class for which work) changes with
// telemetry; the catalogue (which model is the strong class today) changes when
// the vendor ships a new one. Tying them together would force a policy review at
// every release.
type ModelCatalog map[ModelClass]string

// DefaultCatalog is the starting point. Illustrative and replaceable names: the
// composition root can pass another catalogue without touching the policy.
func DefaultCatalog() ModelCatalog {
	return ModelCatalog{
		ClassCheap:  "claude-haiku",
		ClassMedium: "claude-sonnet",
		ClassStrong: "claude-opus",
	}
}

// Decision is the decision returned — and it CARRIES THE REASON.
//
// Reason is not decoration: without it nobody audits ("why did this demand run
// on the expensive model?") and nobody calibrates ("why is this row of the table
// wrong?"). A decision with no justification is a number you cannot contest.
type Decision struct {
	TaskKind TaskKind
	Class    ModelClass
	Model    string
	Effort   Effort
	Reason   string
}

// Router applies the table. No state, no I/O, no clock: the same input always
// gives the same output, and that is what makes the policy testable and
// auditable.
type Router struct {
	catalog ModelCatalog
	table   []routingRule
}

// NewRouter accepts a nil catalogue and falls back to the default — unlike the
// clock, which is a PORT and refuses nil. The difference is real: a nil clock
// switches an abstraction off in silence and takes the test back to the wall
// clock; a nil catalogue merely uses the draft policy the ADR already wrote.
func NewRouter(catalog ModelCatalog) *Router {
	if len(catalog) == 0 {
		catalog = DefaultCatalog()
	}
	return &Router{catalog: catalog, table: routingTable}
}

// Route is the decision. It errors only when there is nothing to decide: an
// EMPTY kind is an incomplete request, whereas an unknown kind is new vocabulary
// — and new vocabulary must not stop work in progress, it falls back and says it
// fell back.
func (r *Router) Route(kind TaskKind) (Decision, error) {
	k := TaskKind(strings.ToLower(strings.TrimSpace(string(kind))))
	if k == "" {
		return Decision{}, fmt.Errorf("kind of work not provided")
	}

	rule := fallbackRule
	rule.Kind = k
	for _, cand := range r.table {
		if cand.Kind == k {
			rule = cand
			break
		}
	}

	model, ok := r.catalog[rule.Class]
	if !ok {
		// An incomplete catalogue is a wiring error, but it must not become
		// stopped work: it returns the class as the name and says so in the
		// justification.
		model = string(rule.Class)
	}

	return Decision{
		TaskKind: k,
		Class:    rule.Class,
		Model:    model,
		Effort:   rule.Effort,
		Reason:   fmt.Sprintf("%s: %s", RoutingProvenance, rule.Why),
	}, nil
}

// Table returns a readable copy of the policy, for auditing and for P-7's
// calibration screen. A copy, not the table: a policy the caller can edit in
// memory stops being a policy.
func (r *Router) Table() []Decision {
	out := make([]Decision, 0, len(r.table))
	for _, rule := range r.table {
		d, err := r.Route(rule.Kind)
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out
}
