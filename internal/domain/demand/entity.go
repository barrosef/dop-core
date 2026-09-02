// Package demand is the demand domain — the platform's heart.
//
// The premise that organizes everything here comes from ADR-0006: a demand is
// NOT a row that changes status. It is an append-only log of events, and what is
// called "the demand's state" (current stage, open threads, published findings)
// is a PROJECTION of that log. That is why no operation in this package changes
// state without producing the corresponding event: the two travel together to
// the repository, which writes them in the SAME transaction (ADR-0019). An
// action with no event is a bug, not a detail.
//
// House rule: this package knows nothing of Postgres, gRPC or any SDK. What it
// needs from outside comes in as a PORT (repository.go) and the composition root
// wires it.
package demand

import (
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── vocabulary ───────────────────────────────────────────────────────────────

// DopStatus is the demand's status INSIDE the platform — distinct from the
// provider's status (Jira, ClickUp), which is free text and stays theirs.
type DopStatus string

const (
	StatusNew       DopStatus = "new"
	StatusDoing     DopStatus = "doing"
	StatusDone      DopStatus = "done"
	StatusDelivered DopStatus = "delivered"
)

type StageStatus string

const (
	StagePending StageStatus = "pending"
	StageRunning StageStatus = "running"
	StageBlocked StageStatus = "blocked"
	StageDone    StageStatus = "done"
)

// StageType is the platform's CLOSED vocabulary of types (ADR-0014): the type
// decides the renderer on screen and the agent's behaviour. A new composition is
// data; a new type is platform evolution — which is why the list lives in
// code.
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

// Gate says whether the stage ends on its own or depends on a human decision.
type Gate string

const (
	GateNone  Gate = "none"
	GateHuman Gate = "human"
)

// ThreadState is the conversation's cycle (conversation-and-attention spec §1):
// open → active → blocked → concluded. Concluding REQUIRES a published finding —
// the thread does not die in silence.
//
// These are stored values, compared by the code and by the database's CHECK and
// partial index. They are not what a person reads: the cockpit resolves a label
// from its catalogue. Migration 0015 moved them off Portuguese.
type ThreadState string

const (
	ThreadOpen      ThreadState = "open"
	ThreadActive    ThreadState = "active"
	ThreadBlocked   ThreadState = "blocked"
	ThreadConcluded ThreadState = "concluded"
)

type ArtifactKind string

const (
	ArtifactDocument ArtifactKind = "document"
	ArtifactSpec     ArtifactKind = "spec"
	ArtifactPlan     ArtifactKind = "plan"
	ArtifactTestPlan ArtifactKind = "test_plan"
	ArtifactDiagram  ArtifactKind = "diagram"
	ArtifactReport   ArtifactKind = "report"
)

// ── the frozen flow ──────────────────────────────────────────────────────────

// StageSpec is a stage of the FLOW (the mould), not of the demand (the instance).
type StageSpec struct {
	Key       string
	Name      string
	Type      StageType
	Gate      Gate
	Artifacts []ArtifactKind
	Subtypes  []string // test → aaa, e2e, integration
}

// Flow is the effective flow returned by the resolution chain
// platform ◁ account ◁ workspace ◁ project ◁ demand (ADR-0014).
type Flow struct {
	ID           string
	Name         string
	Version      int32
	ResolvedFrom string // the visible trail: "project ◂ workspace ◂ account"
	Stages       []StageSpec
}

// Snapshot is the flow FROZEN inside the demand.
//
// Keeping only (flow_id, version) would not be enough: a version is a label, and
// a flow edited in place — or promoted, or deleted — would rewrite the past of
// every demand in progress. The snapshot is the immutable copy of the mould at
// the instant the demand started; it is the snapshot, not the live flow, that
// drives the stage machine from here on.
type Snapshot struct {
	FlowID       string
	Name         string
	Version      int32
	ResolvedFrom string
	Stages       []StageSpec
	FrozenAt     time.Time
}

func freeze(f Flow, at time.Time) Snapshot {
	stages := make([]StageSpec, len(f.Stages))
	copy(stages, f.Stages)
	return Snapshot{
		FlowID: f.ID, Name: f.Name, Version: f.Version,
		ResolvedFrom: f.ResolvedFrom, Stages: stages, FrozenAt: at,
	}
}

// ── entidades ────────────────────────────────────────────────────────────────

type Artifact struct {
	ID        string
	Kind      ArtifactKind
	Name      string
	ObjectRef string // a pointer into the ObjectStore — the binary does not pass through here
	Version   int32
	CreatedAt time.Time
}

// Stage is the stage's INSTANCE in this demand: the mould comes from the
// snapshot, the progress comes from the log.
type Stage struct {
	Key        string
	Name       string
	Type       StageType
	Gate       Gate
	Status     StageStatus
	Position   int
	Artifacts  []Artifact
	StartedAt  *time.Time
	FinishedAt *time.Time
	// The human gate's decision, when there was one: nil = nobody decided yet.
	GateApproved *bool
	GateComment  string
}

type Demand struct {
	ID             string
	AccountID      string
	ProjectID      string
	ExternalKey    string // SUOPT-1315
	Title          string
	CardType       string // dynamic, from the provider
	ProviderStatus string
	Status         DopStatus
	Flow           Snapshot
	Stages         []Stage
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// AgentCard is the subagent's brief: purpose, granted tools, model and its slice
// of the demand's budget (ADR-0010).
type AgentCard struct {
	Purpose      string
	Tools        []string
	Model        string
	Effort       string // low | medium | high | xhigh | max
	BudgetMicros int64
}

type Thread struct {
	ID        string
	AccountID string
	DemandID  string
	Key       string // main, db-forensics, logs
	Card      AgentCard
	State     ThreadState
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Blocked is what the attention box reads: a pending question becomes a queue item.
func (t Thread) Blocked() bool { return t.State == ThreadBlocked }

type Message struct {
	ID         string
	AccountID  string
	DemandID   string
	ThreadID   string
	AuthorID   string
	AuthorKind string
	AuthorName string
	Text       string
	At         time.Time
}

// Finding is the conclusion an agent published. It becomes context for its
// siblings, for the dossier and for the project's memory (ADR-0010 §4).
type Finding struct {
	ID        string
	AccountID string
	DemandID  string
	ThreadID  string
	Title     string
	Payload   map[string]any
	CreatedBy string
	CreatedAt time.Time
}

// ── stage machine rules (ADR-0014) ───────────────────────────────────────────

// stageTransitions is the machine, written as data.
//
// `done` is terminal on purpose: undoing a finished stage would rewrite the
// demand's past, and the past is the log. Rework is a new stage or a new demand,
// not a `done` that walks back.
var stageTransitions = map[StageStatus][]StageStatus{
	StagePending: {StageRunning},
	StageRunning: {StageBlocked, StageDone},
	StageBlocked: {StageRunning}, // unblock before concluding
	StageDone:    {},
}

func ValidStageStatus(s StageStatus) bool {
	_, ok := stageTransitions[s]
	return ok
}

// StageByKey finds the stage by the frozen flow's key.
func (d *Demand) StageByKey(key string) (*Stage, error) {
	for i := range d.Stages {
		if d.Stages[i].Key == key {
			return &d.Stages[i], nil
		}
	}
	return nil, errs.Invalid(
		"stage %q does not exist in this demand's frozen flow (%s v%d)",
		key, d.Flow.Name, d.Flow.Version)
}

// CheckAdvance decides whether the requested transition is legitimate, and
// REFUSES saying which stage and why — a generic message here becomes a support
// ticket later.
//
// It returns noop=true when the stage is already in the requested status: event
// delivery is at-least-once (ADR-0019), so repeating an advance is routine and
// must not become an error.
func (d *Demand) CheckAdvance(key string, to StageStatus) (st *Stage, noop bool, err error) {
	if !ValidStageStatus(to) {
		return nil, false, errs.Invalid("unknown stage status: %q", to)
	}
	s, err := d.StageByKey(key)
	if err != nil {
		return nil, false, err
	}
	if s.Status == to {
		return s, true, nil
	}

	// The stage's TYPE drives the machine: a human gate does not close on its
	// own. Letting AdvanceStage finish a human-validation stage would be the
	// platform approving on the human's behalf — exactly what the gate exists to
	// prevent.
	if to == StageDone && s.RequiresHumanGate() {
		return nil, false, errs.Invalid(
			"stage %q (%s) has a human gate: finish it through DecideGate, not AdvanceStage",
			s.Key, s.Type)
	}

	allowed := stageTransitions[s.Status]
	if !contains(allowed, to) {
		return nil, false, errs.Invalid(
			"invalid transition on stage %q: from %s to %s (allowed from %s: %s)",
			s.Key, s.Status, to, s.Status, join(allowed))
	}

	// The flow's order: starting stage 4 with stage 2 pending would hide skipped
	// work behind progress that looks legitimate on screen.
	if to == StageRunning {
		if prev := d.firstUnfinishedBefore(s.Position); prev != nil {
			return nil, false, errs.Invalid(
				"stage %q cannot start: the previous stage %q is still %s",
				s.Key, prev.Key, prev.Status)
		}
	}
	return s, false, nil
}

// RequiresHumanGate: the gate declared in the flow, plus the `human_validation`
// type, which is a human gate by definition — a flow that declares `gate: none`
// on a human-validation stage is contradicting itself, and the type wins.
func (s Stage) RequiresHumanGate() bool {
	return s.Gate == GateHuman || s.Type == TypeHumanValidation
}

// CheckDecideGate validates the gate's decision.
func (d *Demand) CheckDecideGate(key string) (*Stage, error) {
	s, err := d.StageByKey(key)
	if err != nil {
		return nil, err
	}
	if !s.RequiresHumanGate() {
		return nil, errs.Invalid(
			"stage %q has no human gate: nothing to decide", s.Key)
	}
	switch s.Status {
	case StagePending:
		return nil, errs.Invalid(
			"stage %q has not started: there is nothing to approve", s.Key)
	case StageDone:
		return nil, errs.Invalid(
			"stage %q is already finished: a demand's past is not rewritten", s.Key)
	}
	return s, nil
}

func (d *Demand) firstUnfinishedBefore(pos int) *Stage {
	for i := range d.Stages {
		if d.Stages[i].Position < pos && d.Stages[i].Status != StageDone {
			return &d.Stages[i]
		}
	}
	return nil
}

// ProjectStatus recomputes the demand's status from its stages — it is a
// projection, never a field somebody writes by hand.
//
// `delivered` is not derivable from here: delivery is the delivery domain's
// business, and what it has already marked does not regress because of a
// stage.
func (d *Demand) ProjectStatus() DopStatus {
	if d.Status == StatusDelivered {
		return StatusDelivered
	}
	if len(d.Stages) == 0 {
		return StatusNew
	}
	done, touched := 0, false
	for _, s := range d.Stages {
		if s.Status == StageDone {
			done++
		}
		if s.Status != StagePending {
			touched = true
		}
	}
	switch {
	case done == len(d.Stages):
		return StatusDone
	case touched:
		return StatusDoing
	default:
		return StatusNew
	}
}

// ── thread rules (conversation-and-attention spec §1) ────────────────────────

// CheckConclude is the rule that names this section: the thread does not die in
// silence.
//
// hasFinding comes from the repository because a finding is STATE — published in
// the event's own transaction — and cannot be read from the asynchronous
// projection, which may not have seen a publication from a second ago.
func (t Thread) CheckConclude(hasFinding bool) error {
	if t.State == ThreadConcluded {
		return nil // already concluded: repeating is harmless
	}
	if !hasFinding {
		return errs.Precondition(
			"thread %q cannot be concluded without a published finding: "+
				"publish the investigation's conclusion before closing it", t.Key)
	}
	return nil
}

// CheckPost refuses a message in a closed thread — after the finding, the
// durable record is the finding; reopening the conversation is a new thread.
func (t Thread) CheckPost() error {
	if t.State == ThreadConcluded {
		return errs.Precondition(
			"thread %q is concluded and takes no new messages", t.Key)
	}
	return nil
}

// ValidateThreadKey: the key is the thread's address within the demand
// (#main, #db-forensics) — it has to be stable and typeable.
func ValidateThreadKey(k string) error {
	k = strings.TrimSpace(k)
	if k == "" {
		return errs.Invalid("a thread needs a key (e.g. main, db-forensics)")
	}
	if len(k) > 64 {
		return errs.Invalid("a thread key may have at most 64 characters")
	}
	for _, r := range k {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return errs.Invalid(
				"a thread key accepts only lowercase letters, digits, hyphen and underscore: %q", k)
		}
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func contains(list []StageStatus, v StageStatus) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func join(list []StageStatus) string {
	if len(list) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(list))
	for _, s := range list {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ", ")
}
