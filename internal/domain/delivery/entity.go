// Package delivery is the delivery domain: the path from verified code to
// `main` — evidence of green, pull request, per-repository merge queue and the
// techlead's coordination directives.
//
// House rule: this package knows nothing of Postgres, gRPC or a git provider's
// SDK. It declares what it needs as a PORT (repository.go) and the composition
// root wires it.
//
// Two decisions organize everything here:
//
//   - **Evidence is data, not trust** (ADR-0007). There is no
//     `verified bool` field coming from the caller. What exists is a list of runs
//     — which suite, over which commit, with which outcome, with what trace — and
//     green is a FUNCTION of that list. A "no green, no PR" that accepts a
//     boolean from the caller proves nothing.
//   - **A directive coordinates, it never pauses** (ADR-0015 §5). The vocabulary
//     of a directive's actions is closed and contains no "pause", "block" or
//     "suspend" — and the port into the demand domain is read-only. There is no
//     path, not even by mistake, through which a coordination decision stops a
//     demand that is already moving.
package delivery

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ─────────────────────────── evidence of green ────────────────────────────

// CheckKind is the nature of one verification run.
//
// `critic` is a run like any other on purpose: the critic's opinion (ADR-0007
// §3) is evidence with an outcome, a trace and a commit — not an adjective
// pendurado no PR.
type CheckKind string

const (
	CheckAcceptance CheckKind = "acceptance" // the spec's executable criteria
	CheckUnit       CheckKind = "unit"
	CheckE2E        CheckKind = "e2e"
	CheckCritic     CheckKind = "critic" // the independent instance's opinion
)

func ValidCheckKind(k CheckKind) bool {
	switch k {
	case CheckAcceptance, CheckUnit, CheckE2E, CheckCritic:
		return true
	}
	return false
}

// Outcome is ONE run's verdict. There is no "unknown": a run with no outcome is
// not evidence, it is noise.
type Outcome string

const (
	OutcomePassed  Outcome = "passed"
	OutcomeFailed  Outcome = "failed"
	OutcomeErrored Outcome = "errored" // nem passou nem falhou: quebrou no meio
)

func ValidOutcome(o Outcome) bool {
	switch o {
	case OutcomePassed, OutcomeFailed, OutcomeErrored:
		return true
	}
	return false
}

// VerificationRun is ONE verifiable run — the unit of evidence.
//
// Commit is the field that gives everything else its value: evidence that does
// not say WHICH code ran can prove anything, and therefore proves nothing. That
// is why green is always asked for a specific commit, and not for
// para "a demanda".
type VerificationRun struct {
	ID        string
	AccountID string
	DemandID  string
	RepoID    string
	Commit    string // the exact SHA the run executed over
	Kind      CheckKind
	Suite     string // the suite's or criterion's name — WHAT ran
	Outcome   Outcome
	Total     int
	Passed    int
	Failed    int
	// The trace: where it ran (sandbox) and where the logs are (ObjectStore).
	// One of the two is required — "it passed" with no trace is a word, not evidence.
	SandboxID string
	LogRef    string
	Detail    map[string]any // the critic's opinion, the failure's reasons
	Attempts  int            // how many times this suite ran on this commit
	StartedAt time.Time
	EndedAt   time.Time
}

// Validate refuses a run that would not serve as proof.
func (r VerificationRun) Validate() error {
	if strings.TrimSpace(r.DemandID) == "" || strings.TrimSpace(r.RepoID) == "" {
		return errs.Invalid("verification run with no demand or repository")
	}
	if strings.TrimSpace(r.Commit) == "" {
		return errs.Invalid("verification run with no commit: evidence that does not say which code ran is not evidence")
	}
	if !ValidCheckKind(r.Kind) {
		return errs.Invalid("unknown verification kind: %q", r.Kind)
	}
	if !ValidOutcome(r.Outcome) {
		return errs.Invalid("unknown verification outcome: %q", r.Outcome)
	}
	if strings.TrimSpace(r.Suite) == "" {
		return errs.Invalid("verification run with no identification of what ran")
	}
	if r.SandboxID == "" && r.LogRef == "" {
		return errs.Invalid("run with no trace (sandbox or log): an outcome with nowhere to check does not prove green")
	}
	if r.Outcome == OutcomePassed && r.Failed > 0 {
		return errs.Invalid("a passed run with %d failure(s) is incoherent", r.Failed)
	}
	return nil
}

// Evidence is ONE commit's evidence package: the runs that proved — or failed
// to prove — that this exact code is green.
type Evidence struct {
	DemandID string
	RepoID   string
	Commit   string
	Runs     []VerificationRun
}

// Missing returns EVERYTHING that is missing for the commit to be green.
//
// A list, not a boolean, because whoever is refused needs to know what to
// provide. "Precondition failed" without saying which precondition is the same
// as not answering.
func (e Evidence) Missing() []string {
	var falta []string
	if strings.TrimSpace(e.Commit) == "" {
		return []string{"there is no commit to verify: the PR has to point at a commit"}
	}

	var aceitacao, critico int
	for _, r := range e.Runs {
		// A run from another commit does not count — neither for nor against. It
		// is what caught the semantic break ADR-0008 describes: yesterday's green
		// is not today's green.
		if r.Commit != e.Commit {
			falta = append(falta, fmt.Sprintf(
				"run %q belongs to commit %s, not to commit %s under review",
				r.Suite, short(r.Commit), short(e.Commit)))
			continue
		}
		if r.Outcome != OutcomePassed {
			falta = append(falta, fmt.Sprintf(
				"run %q (%s) ended in %s", r.Suite, r.Kind, r.Outcome))
			continue
		}
		switch r.Kind {
		case CheckAcceptance:
			aceitacao++
		case CheckCritic:
			critico++
		}
	}
	if aceitacao == 0 {
		falta = append(falta, fmt.Sprintf(
			"no passed acceptance run for commit %s (ADR-0007 §1)", short(e.Commit)))
	}
	if critico == 0 {
		falta = append(falta, fmt.Sprintf(
			"the critic's opinion for commit %s is missing (ADR-0007 §3)", short(e.Commit)))
	}
	return falta
}

// Green is the question ADR-0007 asks. The answer comes from the list of runs,
// never from a field somebody filled in.
func (e Evidence) Green() bool { return len(e.Missing()) == 0 }

// Reason joins what is missing into a single sentence, for the refusal message.
func (e Evidence) Reason() string { return strings.Join(e.Missing(), "; ") }

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// ─────────────────────────── pull request ───────────────────────────

type Reviewer struct {
	Name     string
	Initials string
	Status   string // approved | rejected | pending
}

// PullRequest is the PR once opened — and, by construction, a PR only exists if
// its commit was green at the moment of opening (ADR-0007). The rule is checked
// here, in the service, and again by a database TRIGGER: an invariant that
// cannot be violated by any path does not live only in application code.
type PullRequest struct {
	ID           string
	AccountID    string
	DemandID     string
	RepoID       string
	Repo         string // the readable name, "org/repo"
	SourceBranch string
	TargetBranch string
	HeadCommit   string // the commit the evidence covers
	URL          string
	ExternalID   string // the provider's number/id
	Merged       bool
	HasConflict  bool
	Reviewers    []Reviewer
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ─────────────────────────── fila de merge ───────────────────────────

// QueueState are ADR-0008's states: queued → rebase → re-verification → merge,
// one at a time, per repository.
type QueueState string

const (
	StateQueued    QueueState = "queued"
	StateRebasing  QueueState = "rebasing"
	StateVerifying QueueState = "verifying"
	StateMerged    QueueState = "merged"
	StateConflict  QueueState = "conflict"
)

func ValidQueueState(s QueueState) bool {
	switch s {
	case StateQueued, StateRebasing, StateVerifying, StateMerged, StateConflict:
		return true
	}
	return false
}

// IsTerminal: a merge is the end of the line. A conflict is NOT terminal — it is
// waiting on a person, and it goes back to rebase once somebody resolves it.
func (s QueueState) IsTerminal() bool { return s == StateMerged }

// NeedsHuman marks the state that feeds the attention box.
func (s QueueState) NeedsHuman() bool { return s == StateConflict }

// CanTransitionTo writes ADR-0008's flow as a state machine. Without it,
// "verifying → queued" happens by accident of code and nobody notices.
func (s QueueState) CanTransitionTo(n QueueState) bool {
	switch s {
	case StateQueued:
		return n == StateRebasing || n == StateConflict
	case StateRebasing:
		return n == StateVerifying || n == StateConflict
	case StateVerifying:
		// A failed re-verification sends the PR back to the agent: the conflict
		// here is semantic, not textual — it is exactly the case the queue exists
		// pegar.
		return n == StateMerged || n == StateConflict
	case StateConflict:
		return n == StateRebasing // resolvido, tenta de novo
	}
	return false
}

// DefaultPriority is the priority of whoever enters with no ordering directive.
// It is worth keeping far from zero: an ordering directive has to be able to put
// somebody AHEAD of what is already queued without renumbering the world.
const DefaultPriority = 100

// ConflictReport is the conflict turned into DATA — what the attention box needs
// for the human to decide without archaeology (ADR-0008 §2).
type ConflictReport struct {
	Files      []string
	BaseCommit string // contra qual `main` o rebase foi tentado
	Attempts   int    // quantas vezes o agente tentou antes de escalar
	Detail     string
	ReportedAt time.Time
}

func (c ConflictReport) Validate() error {
	if len(c.Files) == 0 && strings.TrimSpace(c.Detail) == "" {
		return errs.Invalid("a conflict report with neither files nor a description gives nobody anything to decide")
	}
	return nil
}

// MergeQueueEntry is a PR's position in ONE repository's queue (ADR-0008).
type MergeQueueEntry struct {
	ID            string
	AccountID     string
	RepoID        string
	DemandID      string
	PullRequestID string
	// Seq is the arrival sequence WITHIN the repository, unique per repository
	// (a database constraint). It is the tie-break that keeps two entries from
	// entradas de ficarem ambiguamente lado a lado.
	Seq int64
	// Priority is where the preferred-ordering directive (ADR-0015) acts. Lower
	// goes first. Note that reordering pauses nobody: the demand that lost its
	// turn keeps running, it just merges later.
	Priority         int
	Position         int32 // DERIVED from the order; it is not persisted state
	State            QueueState
	OverlappingFiles []string // the techlead's detection (ADR-0008 §3)
	Conflict         *ConflictReport
	EnqueuedAt       time.Time
	UpdatedAt        time.Time
}

// Before is the queue's TOTAL order: (priority, sequence).
//
// Total, and not partial, is the whole point: because Seq is unique per
// repository, there is no pair of entries for which both `a.Before(b)` and
// `b.Before(a)` are false. A queue with ties is a queue whose order depends on
// who ran the ORDER BY — and then the position shown in the cockpit changes on
// refreshes.
func (e MergeQueueEntry) Before(o MergeQueueEntry) bool {
	if e.Priority != o.Priority {
		return e.Priority < o.Priority
	}
	return e.Seq < o.Seq
}

// SortQueue orders and NUMBERS the positions (1-based).
//
// The order belongs to the domain, not to the ORDER BY: the adapter already
// returns the rows sorted, and we still reorder here. A queue serializes merges
// — the rule of who goes first is a business rule, and a business rule that lives only in SQL is
// not testable without a database.
func SortQueue(entries []MergeQueueEntry) []MergeQueueEntry {
	out := make([]MergeQueueEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Before(out[j]) })
	for i := range out {
		out[i].Position = int32(i + 1)
	}
	return out
}

// ─────────────────────────── diretrizes ───────────────────────────

// DirectiveKind is ADR-0015 §6's INITIAL vocabulary — and also the vocabulary of
// the actions a directive instructs.
//
// Note what is not here and will not be: pause, block, suspend,
// esperar. A regra de ouro da ADR-0015 ("transversal identificada NUNCA pausa
// demand") is not a care taken by whoever implements: it is the absence of a
// value in the type. Demand 1 goes as far as it can; when the directive's
// condition is met, it applies the coordination and carries on.
type DirectiveKind string

const (
	DirectiveCherryPick    DirectiveKind = "cherry_pick"
	DirectiveMergeOrder    DirectiveKind = "merge_order"
	DirectiveFilePartition DirectiveKind = "file_partition"
	DirectiveCrossVerify   DirectiveKind = "cross_verify"
)

func ValidDirectiveKind(k DirectiveKind) bool {
	switch k {
	case DirectiveCherryPick, DirectiveMergeOrder, DirectiveFilePartition, DirectiveCrossVerify:
		return true
	}
	return false
}

type DirectiveStatus string

const (
	DirectiveProposed   DirectiveStatus = "proposed"
	DirectiveDecided    DirectiveStatus = "decided"
	DirectiveSuperseded DirectiveStatus = "superseded"
)

// Instruction is what a decided directive hands to ONE demand.
//
// It is always WORK TO DO — "cherry-pick when 0 commits", "run 1's acceptance
// over 0's result". When is the condition that fires the application, and
// having a condition is the opposite of blocking: the demand carries on and
// applies the coordination once the condition is met.
type Instruction struct {
	DemandID string
	Action   DirectiveKind
	When     string // the condition in domain language: "demand X committed"
	Payload  map[string]any
}

func (i Instruction) Validate() error {
	if strings.TrimSpace(i.DemandID) == "" {
		return errs.Invalid("coordination instruction with no target demand")
	}
	// The narrow gate: only the coordination vocabulary passes. A pause
	// "instruction" simply cannot be expressed.
	if !ValidDirectiveKind(i.Action) {
		return errs.Invalid(
			"coordination action outside the vocabulary: %q — a directive coordinates, it never pauses a demand (ADR-0015 §5)",
			i.Action)
	}
	return nil
}

// DirectiveOption is one of the ready-made ways out the techlead brings along
// with the problem. The attention box receives a decision item, not a raw alarm.
type DirectiveOption struct {
	Key          string
	Summary      string
	Instructions []Instruction
}

// Decision records WHO decided and WHY. Both are required: coordination between
// parallel demands is an engineering decision, and a decision with no recorded
// reason becomes invisible magic three weeks later (ADR-0015).
type Decision struct {
	Option    string
	Rationale string
	DecidedBy string
	ActorKind string
	DecidedAt time.Time
}

// Directive is the techlead's request for a decision about a cross-cutting concern.
type Directive struct {
	ID        string
	AccountID string
	ProjectID string
	Kind      DirectiveKind
	Summary   string
	Payload   map[string]any // sinais: arquivos sobrepostos, diffs, specs lidas
	// AffectedDemands are the demands the concern touches. They are here as a
	// reference, and ONLY as a reference: nothing in this domain writes into the
	// delas.
	AffectedDemands []string
	Options         []DirectiveOption
	Recommended     string // the recommended option's key
	Status          DirectiveStatus
	Decision        *Decision
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Option returns the option by key.
func (d Directive) Option(key string) (DirectiveOption, bool) {
	for _, o := range d.Options {
		if o.Key == key {
			return o, true
		}
	}
	return DirectiveOption{}, false
}

// Validate demands what ADR-0015 §3 demands of a request for a decision:
// ready-made options (at least two — with only one there is nothing to decide,
// it is an alarm with an OK button) and a recommendation among them.
func (d Directive) Validate() error {
	if strings.TrimSpace(d.ProjectID) == "" {
		return errs.Invalid("diretriz sem projeto")
	}
	if !ValidDirectiveKind(d.Kind) {
		return errs.Invalid("tipo de diretriz desconhecido: %q", d.Kind)
	}
	if strings.TrimSpace(d.Summary) == "" {
		return errs.Invalid("diretriz sem enunciado da transversal detectada")
	}
	if len(d.Options) < 2 {
		return errs.Invalid("a directive needs at least two options: one option is an alarm, not a decision (ADR-0015 §3)")
	}
	vistas := make(map[string]bool, len(d.Options))
	for _, o := range d.Options {
		if strings.TrimSpace(o.Key) == "" {
			return errs.Invalid("directive option with no key")
		}
		if vistas[o.Key] {
			return errs.Invalid("option %q duplicated in the directive", o.Key)
		}
		vistas[o.Key] = true
		if strings.TrimSpace(o.Summary) == "" {
			return errs.Invalid("option %q with no description of what it does", o.Key)
		}
		if len(o.Instructions) == 0 {
			return errs.Invalid("option %q instructs no demand: a decision that does not become coordination is useless", o.Key)
		}
		for _, ins := range o.Instructions {
			if err := ins.Validate(); err != nil {
				return err
			}
		}
	}
	if _, ok := d.Option(d.Recommended); !ok {
		return errs.Invalid("the recommendation %q is not among the offered options", d.Recommended)
	}
	return nil
}
