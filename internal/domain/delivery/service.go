package delivery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service concentra as regras de entrega. Recebe apenas PORTAS.
type Service struct {
	repo      Repository
	demands   Demands
	clock     ports.Clock
	providers GitProviders
}

// NewService requires the three ports.
//
// The clock is required for the same reason as in identity: accepting nil would
// let the service fall back to time.Now() internally, and no evidence test
// ("is this run from this commit, and when did it end?") would be
// deterministic. The panic here is deliberate — a wiring error is caught at
// boot, not in production.
// NewService takes `providers` OPTIONALLY: without it, the PR is recorded only
// in our own database and is not opened at the provider. That is DECLARED
// degradation — there are environments (tests, and local with no credential)
// where no provider exists — and the service says so in the returned PR instead
func NewService(repo Repository, demands Demands, clock ports.Clock, providers GitProviders) *Service {
	if repo == nil {
		panic("delivery.NewService: repository is required")
	}
	if demands == nil {
		panic("delivery.NewService: the demands port is required")
	}
	if clock == nil {
		panic("delivery.NewService: clock is required — use clock.NewSystem()")
	}
	return &Service{repo: repo, demands: demands, clock: clock, providers: providers}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// ─────────────────────────── evidence ─────────────────────────────

// RecordVerification records ONE verification run.
//
// This is how evidence enters the system, and it is why there is no RPC saying
// "this PR is green": green is derived from these rows. Whoever wants to cheat
// has to forge a run with a commit, a suite, an outcome and a trace — which is
// exactly the record we want auditable.
func (s *Service) RecordVerification(ctx context.Context, run VerificationRun, idemKey string) (*VerificationRun, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if err := run.Validate(); err != nil {
		return nil, err
	}
	// The demand has to exist IN THE ACCOUNT: without that, evidence from one
	// provaria o verde de outra.
	if _, err := s.demands.Demand(ctx, accountID, run.DemandID); err != nil {
		return nil, err
	}
	run.AccountID = accountID
	if run.EndedAt.IsZero() {
		run.EndedAt = s.now()
	}
	if run.Attempts <= 0 {
		run.Attempts = 1
	}
	return s.repo.RecordVerification(ctx, &run, idemKey)
}

// Evidence responde "o que se sabe sobre o verde deste commit" — a consulta que
// the critic, the cockpit and the queue's refusal all share.
func (s *Service) Evidence(ctx context.Context, demandID, repoID, commit string) (Evidence, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return Evidence{}, err
	}
	if demandID == "" || repoID == "" || commit == "" {
		return Evidence{}, errs.Invalid("evidence is always about one demand, one repository and one commit")
	}
	return s.repo.EvidenceFor(ctx, accountID, demandID, repoID, commit)
}

// ─────────────────────────── pull requests ───────────────────────────

// OpenSpec is the request to open a PR.
type OpenSpec struct {
	DemandID     string
	RepoID       string
	Repo         string
	SourceBranch string
	TargetBranch string
	HeadCommit   string
	URL          string
	ExternalID   string
	Reviewers    []Reviewer
	// The PR's title and body at the provider. Empty, the adapter uses the
	// branch name — a PR with no title exists and is bad, a PR not opened is worse.
	Title string
	Body  string
}

// OpenPullRequest is ADR-0007 at the exact point where it bites: there is no PR
// without evidence of green for the commit it carries.
//
// The refusal says what is missing, item by item. "Precondition failed" without
// saying which precondition makes the agent retry blindly — and retrying blindly
// is how a broken PR ends up reaching the human by another route.
func (s *Service) OpenPullRequest(ctx context.Context, spec OpenSpec, idemKey string) (*PullRequest, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.DemandID) == "" || strings.TrimSpace(spec.RepoID) == "" {
		return nil, errs.Invalid("a PR needs a demand and a repository")
	}
	if strings.TrimSpace(spec.HeadCommit) == "" {
		return nil, errs.Invalid("a PR with no head commit: there is nothing to verify")
	}
	if strings.TrimSpace(spec.SourceBranch) == "" {
		return nil, errs.Invalid("PR sem branch de origem")
	}
	if _, err := s.demands.Demand(ctx, accountID, spec.DemandID); err != nil {
		return nil, err
	}

	ev, err := s.repo.EvidenceFor(ctx, accountID, spec.DemandID, spec.RepoID, spec.HeadCommit)
	if err != nil {
		return nil, err
	}
	if falta := ev.Missing(); len(falta) > 0 {
		return nil, errs.Precondition(
			"sem verde, sem PR (ADR-0007): %s", strings.Join(falta, "; "))
	}

	target := spec.TargetBranch
	if target == "" {
		target = "main"
	}

	// The PR is opened AT THE PROVIDER before being recorded here.
	//
	// A ordem importa: registrar primeiro deixaria a nossa base afirmando um PR
	// that does not exist, and it is our database the merge queue consults.
	// Failing to open beats recording a lie.
	if s.providers != nil {
		p, err := s.providers.For(ctx, accountID, spec.RepoID)
		if err != nil {
			return nil, err
		}
		aberto, err := p.OpenPullRequest(ctx, OpenPRSpec{
			RepoExternalID: spec.Repo,
			SourceBranch:   spec.SourceBranch,
			TargetBranch:   target,
			Title:          spec.Title,
			Body:           spec.Body,
		})
		if err != nil {
			return nil, err
		}
		// What the provider returns WINS over what the caller said: id and URL
		// dele, e aceitar os do chamador deixaria a nossa base apontando para
		// a PR that may not be that one.
		spec.ExternalID, spec.URL = aberto.ExternalID, aberto.URL
	}

	pr := &PullRequest{
		AccountID:    accountID,
		DemandID:     spec.DemandID,
		RepoID:       spec.RepoID,
		Repo:         spec.Repo,
		SourceBranch: spec.SourceBranch,
		TargetBranch: target,
		HeadCommit:   spec.HeadCommit,
		URL:          spec.URL,
		ExternalID:   spec.ExternalID,
		Reviewers:    spec.Reviewers,
		CreatedAt:    s.now(),
		UpdatedAt:    s.now(),
	}
	return s.repo.OpenPullRequest(ctx, pr, idemKey)
}

func (s *Service) ListPullRequests(ctx context.Context, f PRFilter) ([]PullRequest, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListPullRequests(ctx, accountID, f)
}

// ─────────────────────────── fila de merge ───────────────────────────

// GetMergeQueue returns ONE repository's queue, in a deterministic order and
// with numbered positions (ADR-0008 §1).
//
// The queue is per repository because the repository is what serializes: two
// PRs in different repositories do not invalidate each other.
func (s *Service) GetMergeQueue(ctx context.Context, repoID string) ([]MergeQueueEntry, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(repoID) == "" {
		return nil, errs.Invalid("a merge queue is always about one repository (ADR-0008)")
	}
	entries, err := s.repo.QueueOfRepo(ctx, accountID, repoID, false)
	if err != nil {
		return nil, err
	}
	return SortQueue(entries), nil
}

// EnqueueMerge is the queue's door — and it is where ADR-0007 is enforced a
// vez, agora contra o commit ATUAL do PR.
//
// Enforcing it again is not redundancy: between opening the PR and entering the
// queue the branch may have moved, and the old commit's green is not the new
// commit's green. It is the same reasoning that makes the queue re-verify at
// each position (ADR-0008 §1) — green is always about a state of the code, never
// about an intention.
func (s *Service) EnqueueMerge(ctx context.Context, repoID, demandID, idemKey string) (*MergeQueueEntry, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(repoID) == "" || strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("entering the queue requires a repository and a demand")
	}
	if _, err := s.demands.Demand(ctx, accountID, demandID); err != nil {
		return nil, err
	}

	pr, err := s.repo.PullRequestOf(ctx, accountID, demandID, repoID)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, errs.Precondition(
			"demand %s has no open PR on repository %s yet — and there is no PR without evidence of green (ADR-0007)",
			demandID, repoID)
	}
	if pr.Merged {
		return nil, errs.Precondition("demand %s's PR has already been merged", demandID)
	}

	ev, err := s.repo.EvidenceFor(ctx, accountID, demandID, repoID, pr.HeadCommit)
	if err != nil {
		return nil, err
	}
	if falta := ev.Missing(); len(falta) > 0 {
		// FailedPrecondition, and not Invalid: the request is well formed; what
		// is missing is a state of the world the caller can provide (run the
		// acceptance, call the critic) and try again.
		return nil, errs.Precondition(
			"the merge queue refuses an entry with no evidence of green for commit %s (ADR-0007): %s",
			short(pr.HeadCommit), strings.Join(falta, "; "))
	}

	entry := &MergeQueueEntry{
		AccountID:     accountID,
		RepoID:        repoID,
		DemandID:      demandID,
		PullRequestID: pr.ID,
		Priority:      DefaultPriority,
		State:         StateQueued,
		EnqueuedAt:    s.now(),
		UpdatedAt:     s.now(),
	}
	return s.repo.Enqueue(ctx, entry, idemKey)
}

// AdvanceQueue moves the entry through the flow `queued → rebase →
// re-verification → merge`. A transition outside the state machine is refused —
// it is what prevents "merged" without going through re-verification.
func (s *Service) AdvanceQueue(ctx context.Context, entryID string, to QueueState, idemKey string) (*MergeQueueEntry, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidQueueState(to) {
		return nil, errs.Invalid("estado de fila desconhecido: %q", to)
	}
	if to == StateConflict {
		// A conflict carries a report; it has its own path, with its own event.
		return nil, errs.Invalid("a conflict enters through ReportConflict, with the report the attention box needs")
	}
	entry, err := s.queueEntry(ctx, accountID, entryID)
	if err != nil {
		return nil, err
	}
	if entry.State == to {
		return entry, nil // a repeat is harmless
	}
	if !entry.State.CanTransitionTo(to) {
		return nil, errs.Precondition(
			"the queue does not go from %s to %s (ADR-0008: queued → rebase → re-verification → merge)",
			entry.State, to)
	}
	return s.repo.SetQueueState(ctx, accountID, entryID, to, nil, idemKey)
}

// ReportConflict transforma o conflito em ITEM DE DECISÃO HUMANA.
//
// ADR-0008 §2 is explicit: rebasing and resolving are the demand agent's task;
// a failure ESCALATES to the human through the attention box, with the
// conflict's context. Escalating means writing state and emitting the event in
// the same transaction — the event is what feeds the box. Returning an error
// here would be the silent version of the same fact: the agent would see a
func (s *Service) ReportConflict(ctx context.Context, entryID string, c ConflictReport, idemKey string) (*MergeQueueEntry, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	entry, err := s.queueEntry(ctx, accountID, entryID)
	if err != nil {
		return nil, err
	}
	if entry.State.IsTerminal() {
		return nil, errs.Precondition("an already merged entry does not enter a conflict")
	}
	if c.ReportedAt.IsZero() {
		c.ReportedAt = s.now()
	}
	return s.repo.SetQueueState(ctx, accountID, entryID, StateConflict, &c, idemKey)
}

func (s *Service) queueEntry(ctx context.Context, accountID, entryID string) (*MergeQueueEntry, error) {
	if strings.TrimSpace(entryID) == "" {
		return nil, errs.Invalid("queue entry not provided")
	}
	entry, err := s.repo.QueueEntryByID(ctx, accountID, entryID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, errs.NotFound("entrada da fila de merge")
	}
	return entry, nil
}

// ─────────────────────────── diretrizes ───────────────────────────

// ProposeDirective is the techlead invoking the attention box with a request
// for a decision (ADR-0015 §3): ready-made options and a recommendation.
//
// There is no path to propose "pause demand X": Validate walks each option's
// instructions and only lets the coordination vocabulary through.
func (s *Service) ProposeDirective(ctx context.Context, d Directive, idemKey string) (*Directive, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	// Every instructed demand has to exist in the account and belong to the
	// directive's project — coordination across different projects is not
	// engano.
	for _, id := range instructedDemands(d) {
		info, err := s.demands.Demand(ctx, accountID, id)
		if err != nil {
			return nil, err
		}
		if info.ProjectID != d.ProjectID {
			return nil, errs.Invalid(
				"demand %s does not belong to the directive's project", id)
		}
	}
	d.AccountID = accountID
	d.Status = DirectiveProposed
	d.CreatedAt = s.now()
	d.UpdatedAt = d.CreatedAt
	return s.repo.CreateDirective(ctx, &d, idemKey)
}

func (s *Service) ListDirectives(ctx context.Context, projectID string) ([]Directive, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, errs.Invalid("directives always belong to a project (ADR-0015)")
	}
	return s.repo.ListDirectives(ctx, accountID, projectID)
}

// Keys accepted in the contract's decision Struct. There are two because
// pede as duas: a escolha e o motivo dela.
const (
	DecisionKeyOption    = "option"
	DecisionKeyRationale = "rationale"
)

// DecideDirective records the dev's choice: WHO decided, WHICH option and WHY.
//
// And here is the golden rule, said again because it is the one that most tries
// to escape: deciding a directive INTERRUPTS no demand. There is no way to: this
// domain's only port into the demand domain is read-only, and what the decision
// produces are instructions from the coordination vocabulary — work to do,
// conditioned on what is already happening. Demand 1 goes as far as it can; when
// the condition is met, it applies the coordination and carries on.
func (s *Service) DecideDirective(ctx context.Context, directiveID string, decision map[string]any, idemKey string) (*Directive, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "deciding a directive requires an identified actor")
	}
	// The dev decides (ADR-0015 §4): the techlead detects, plans and proposes;
	// the choice is human. Letting an agent decide its own proposal would close
	// the loop without the one participant the directive exists to consult.
	if call.ActorKind == ctxutil.ActorAgent || call.ActorKind == ctxutil.ActorSubagent {
		return nil, errs.Permission("agents propose directives; the dev decides (ADR-0015 §4)")
	}
	if strings.TrimSpace(directiveID) == "" {
		return nil, errs.Invalid("directive not provided")
	}

	option := texto(decision[DecisionKeyOption])
	rationale := texto(decision[DecisionKeyRationale])
	if option == "" {
		return nil, errs.Invalid("the decision has to say which option (%q)", DecisionKeyOption)
	}
	// A reason is required. Coordination between parallel demands is an
	// engineering decision: with no recorded why, nobody understands three weeks
	// later why demand 2 waited on 1 — and the directive becomes invisible magic.
	if rationale == "" {
		return nil, errs.Invalid("the decision has to record the reason (%q)", DecisionKeyRationale)
	}

	d, err := s.repo.DirectiveByID(ctx, accountID, directiveID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, errs.NotFound("diretriz")
	}
	if d.Status == DirectiveDecided && d.Decision != nil {
		if d.Decision.Option == option {
			return d, nil // repeating the same decision is harmless
		}
		return nil, errs.Conflict(
			"the directive was already decided (%s) by %s — propose a new one instead of rewriting the decision",
			d.Decision.Option, d.Decision.DecidedBy)
	}
	if d.Status == DirectiveSuperseded {
		return nil, errs.Precondition("diretriz superada por outra")
	}

	opt, ok := d.Option(option)
	if !ok {
		return nil, errs.Invalid("option %q is not among those offered: %s",
			option, strings.Join(chaves(d.Options), ", "))
	}
	for _, ins := range opt.Instructions {
		if err := ins.Validate(); err != nil {
			return nil, err
		}
	}

	dec := Decision{
		Option:    option,
		Rationale: rationale,
		DecidedBy: call.ActorID,
		ActorKind: string(call.ActorKind),
		DecidedAt: s.now(),
	}
	return s.repo.DecideDirective(ctx, accountID, directiveID, dec, opt.Instructions, idemKey)
}

// ─────────────────────────── auxiliares ───────────────────────────

// instructedDemands junta, sem repetir, toda demanda citada pela diretriz.
func instructedDemands(d Directive) []string {
	visto := make(map[string]bool)
	var out []string
	add := func(id string) {
		if id != "" && !visto[id] {
			visto[id] = true
			out = append(out, id)
		}
	}
	for _, id := range d.AffectedDemands {
		add(id)
	}
	for _, o := range d.Options {
		for _, ins := range o.Instructions {
			add(ins.DemandID)
		}
	}
	return out
}

func chaves(opts []DirectiveOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Key)
	}
	return out
}

// texto extrai string do Struct do contrato sem explodir com tipo inesperado —
// the body comes from outside, and a wrong-typed value is a client error, not a panic.
func texto(v any) string {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return strings.TrimSpace(s)
}
