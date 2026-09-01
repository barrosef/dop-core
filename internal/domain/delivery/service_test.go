package delivery_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT a database: repository and demands are ports,
// and in-memory doubles go in here. They live in THIS file because the
// arquitetura reprova qualquer import de adaptador sob internal/domain,
// inclusive em arquivo _test.go.

// fixedClock is the ports.Clock double — a fixed instant so that "is this
// evidence from this commit, and when did it end?" is verifiable by equality.
type fixedClock struct{ t time.Time }

func (r fixedClock) Now() time.Time { return r.t }

var agora = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

const (
	conta   = "acct-1"
	projeto = "proj-1"
	repo1   = "repo-1"
	repo2   = "repo-2"
	commit1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commit2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// ── duplo da porta de demandas ──────────────────────────────────────────────

// fakeDemands is the NARROW and READ-ONLY port into the demand domain.
// Note there is nothing to spy on beyond reads: delivery has no way to touch a
// demand's state, and that is what the golden rule's test
// verifica estruturalmente.
type fakeDemands struct {
	demandas map[string]delivery.DemandInfo
	leituras int
}

func (f *fakeDemands) Demand(_ context.Context, accountID, id string) (*delivery.DemandInfo, error) {
	f.leituras++
	if accountID != conta {
		return nil, errs.Permission("demanda de outra conta")
	}
	d, ok := f.demandas[id]
	if !ok {
		return nil, errs.NotFound("demanda")
	}
	return &d, nil
}

var _ delivery.Demands = (*fakeDemands)(nil)

// ── repository double ───────────────────────────────────────────────────────

type fakeRepo struct {
	runs       []delivery.VerificationRun
	prs        []delivery.PullRequest
	entries    []delivery.MergeQueueEntry
	directives map[string]*delivery.Directive
	// delivered keeps the instructions a decision applied — it is where the test
	// checks that coordination is work, never a pause.
	delivered []delivery.Instruction
	seq       int
}

func novoRepo() *fakeRepo {
	return &fakeRepo{directives: map[string]*delivery.Directive{}}
}

func (f *fakeRepo) id(prefixo string) string {
	f.seq++
	return fmt.Sprintf("%s-%d", prefixo, f.seq)
}

func (f *fakeRepo) RecordVerification(_ context.Context, r *delivery.VerificationRun, _ string) (*delivery.VerificationRun, error) {
	for i := range f.runs {
		x := f.runs[i]
		if x.AccountID == r.AccountID && x.DemandID == r.DemandID && x.RepoID == r.RepoID &&
			x.Commit == r.Commit && x.Kind == r.Kind && x.Suite == r.Suite {
			r.ID = x.ID
			r.Attempts = x.Attempts + 1
			f.runs[i] = *r
			return r, nil
		}
	}
	r.ID = f.id("run")
	f.runs = append(f.runs, *r)
	return r, nil
}

func (f *fakeRepo) EvidenceFor(_ context.Context, accountID, demandID, repoID, commit string) (delivery.Evidence, error) {
	ev := delivery.Evidence{DemandID: demandID, RepoID: repoID, Commit: commit}
	for _, r := range f.runs {
		if r.AccountID == accountID && r.DemandID == demandID && r.RepoID == repoID {
			ev.Runs = append(ev.Runs, r)
		}
	}
	return ev, nil
}

func (f *fakeRepo) ListPullRequests(_ context.Context, accountID string, filtro delivery.PRFilter) ([]delivery.PullRequest, error) {
	var out []delivery.PullRequest
	for _, pr := range f.prs {
		if pr.AccountID != accountID {
			continue
		}
		if filtro.DemandID != "" && pr.DemandID != filtro.DemandID {
			continue
		}
		if filtro.RepoID != "" && pr.RepoID != filtro.RepoID {
			continue
		}
		if filtro.OnlyOpen && pr.Merged {
			continue
		}
		out = append(out, pr)
	}
	return out, nil
}

func (f *fakeRepo) PullRequestByID(_ context.Context, accountID, id string) (*delivery.PullRequest, error) {
	for i := range f.prs {
		if f.prs[i].ID == id && f.prs[i].AccountID == accountID {
			return &f.prs[i], nil
		}
	}
	return nil, nil
}

func (f *fakeRepo) PullRequestOf(_ context.Context, accountID, demandID, repoID string) (*delivery.PullRequest, error) {
	for i := range f.prs {
		if f.prs[i].AccountID == accountID && f.prs[i].DemandID == demandID && f.prs[i].RepoID == repoID {
			return &f.prs[i], nil
		}
	}
	return nil, nil
}

// OpenPullRequest replicates the database trigger: no green, no write. The
// double has to be as strict as Postgres, otherwise the test passes and production does not.
func (f *fakeRepo) OpenPullRequest(ctx context.Context, pr *delivery.PullRequest, _ string) (*delivery.PullRequest, error) {
	ev, _ := f.EvidenceFor(ctx, pr.AccountID, pr.DemandID, pr.RepoID, pr.HeadCommit)
	if !ev.Green() {
		return nil, errs.Precondition("sem green, sem PR: %s", ev.Reason())
	}
	pr.ID = f.id("pr")
	f.prs = append(f.prs, *pr)
	return pr, nil
}

func (f *fakeRepo) QueueOfRepo(_ context.Context, accountID, repoID string, incluirMergeados bool) ([]delivery.MergeQueueEntry, error) {
	var out []delivery.MergeQueueEntry
	for _, e := range f.entries {
		if e.AccountID != accountID || e.RepoID != repoID {
			continue
		}
		if !incluirMergeados && e.State.IsTerminal() {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeRepo) QueueEntryByID(_ context.Context, accountID, id string) (*delivery.MergeQueueEntry, error) {
	for i := range f.entries {
		if f.entries[i].ID == id && f.entries[i].AccountID == accountID {
			return &f.entries[i], nil
		}
	}
	return nil, nil
}

// Enqueue mimics the database's two constraints: a PR does not enter the same
// repository's queue twice, and the sequence is assigned by the repository.
func (f *fakeRepo) Enqueue(_ context.Context, e *delivery.MergeQueueEntry, _ string) (*delivery.MergeQueueEntry, error) {
	var maior int64
	for _, x := range f.entries {
		if x.RepoID == e.RepoID {
			if x.PullRequestID == e.PullRequestID {
				return nil, errs.New(errs.KindAlreadyExists, "the PR is already in this repository's queue")
			}
			if x.Seq > maior {
				maior = x.Seq
			}
		}
	}
	e.ID = f.id("mq")
	e.Seq = maior + 1
	f.entries = append(f.entries, *e)
	return e, nil
}

func (f *fakeRepo) SetQueueState(_ context.Context, accountID, entryID string, to delivery.QueueState, c *delivery.ConflictReport, _ string) (*delivery.MergeQueueEntry, error) {
	for i := range f.entries {
		if f.entries[i].ID == entryID && f.entries[i].AccountID == accountID {
			f.entries[i].State = to
			f.entries[i].Conflict = c
			return &f.entries[i], nil
		}
	}
	return nil, errs.NotFound("entrada da queue")
}

func (f *fakeRepo) ListDirectives(_ context.Context, accountID, projectID string) ([]delivery.Directive, error) {
	var out []delivery.Directive
	for _, d := range f.directives {
		if d.AccountID == accountID && d.ProjectID == projectID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (f *fakeRepo) DirectiveByID(_ context.Context, accountID, id string) (*delivery.Directive, error) {
	d, ok := f.directives[id]
	if !ok || d.AccountID != accountID {
		return nil, nil
	}
	cp := *d
	return &cp, nil
}

func (f *fakeRepo) CreateDirective(_ context.Context, d *delivery.Directive, _ string) (*delivery.Directive, error) {
	d.ID = f.id("dir")
	cp := *d
	f.directives[d.ID] = &cp
	return d, nil
}

func (f *fakeRepo) DecideDirective(_ context.Context, accountID, id string, dec delivery.Decision, ins []delivery.Instruction, _ string) (*delivery.Directive, error) {
	d, ok := f.directives[id]
	if !ok || d.AccountID != accountID {
		return nil, errs.NotFound("diretriz")
	}
	d.Status = delivery.DirectiveDecided
	d.Decision = &dec
	f.delivered = append(f.delivered, ins...)
	// The preferred order is the only coordination delivery applies on its own:
	// mexe na PRIORIDADE da queue, nunca no estado da demanda.
	for _, i := range ins {
		if i.Action != delivery.DirectiveMergeOrder {
			continue
		}
		p, ok := i.Payload["priority"].(int)
		if !ok {
			continue
		}
		for k := range f.entries {
			if f.entries[k].DemandID == i.DemandID && !f.entries[k].State.IsTerminal() {
				f.entries[k].Priority = p
			}
		}
	}
	cp := *d
	return &cp, nil
}

var _ delivery.Repository = (*fakeRepo)(nil)

// ── scenario ────────────────────────────────────────────────────────────────

func cenario() (*delivery.Service, *fakeRepo, *fakeDemands) {
	repo := novoRepo()
	dem := &fakeDemands{demandas: map[string]delivery.DemandInfo{
		"dem-0": {ID: "dem-0", ProjectID: projeto, Active: true},
		"dem-1": {ID: "dem-1", ProjectID: projeto, Active: true},
		"dem-2": {ID: "dem-2", ProjectID: projeto, Active: true},
	}}
	return delivery.NewService(repo, dem, fixedClock{agora}, nil), repo, dem
}

func comoAtor(id string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: conta, ActorID: id, ActorKind: ctxutil.ActorUser,
	})
}

func execucao(demanda, repoID, commit string, kind delivery.CheckKind, out delivery.Outcome) delivery.VerificationRun {
	return delivery.VerificationRun{
		DemandID: demanda, RepoID: repoID, Commit: commit,
		Kind: kind, Suite: string(kind) + "-suite", Outcome: out,
		Total: 10, Passed: 10, SandboxID: "sbx-" + demanda,
		LogRef: "logs/" + demanda + "/" + commit, EndedAt: agora,
	}
}

// green records the minimum package ADR-0007 requires: a passed acceptance plus
// the critic's opinion, both over the SAME commit.
func green(t *testing.T, svc *delivery.Service, ctx context.Context, demanda, repoID, commit string) {
	t.Helper()
	for _, k := range []delivery.CheckKind{delivery.CheckAcceptance, delivery.CheckCritic} {
		if _, err := svc.RecordVerification(ctx, execucao(demanda, repoID, commit, k, delivery.OutcomePassed), ""); err != nil {
			t.Fatalf("registrar %s: %v", k, err)
		}
	}
}

func abrePR(t *testing.T, svc *delivery.Service, ctx context.Context, demanda, repoID, commit string) *delivery.PullRequest {
	t.Helper()
	pr, err := svc.OpenPullRequest(ctx, delivery.OpenSpec{
		DemandID: demanda, RepoID: repoID, Repo: "acme/portal",
		SourceBranch: "feat/" + demanda, TargetBranch: "main", HeadCommit: commit,
	}, "idem-pr-"+demanda+"-"+repoID)
	if err != nil {
		t.Fatalf("abrir PR: %v", err)
	}
	return pr
}

// ── evidence of green (ADR-0007) ────────────────────────────────────────────

func TestEvidenceWithNoRunIsNotGreen(t *testing.T) {
	ev := delivery.Evidence{DemandID: "dem-1", RepoID: repo1, Commit: commit1}
	if ev.Green() {
		t.Fatal("a commit with no runs at all cannot be considered green")
	}
	// The refusal has to SAY what is missing — a precondition with no which
	// precondition makes the agent retry blindly.
	if len(ev.Missing()) < 2 {
		t.Errorf("acceptance and critic were missing, got: %v", ev.Missing())
	}
}

func TestEvidenceFromAnotherCommitDoesNotCount(t *testing.T) {
	ev := delivery.Evidence{
		DemandID: "dem-1", RepoID: repo1, Commit: commit2,
		Runs: []delivery.VerificationRun{
			execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed),
			execucao("dem-1", repo1, commit1, delivery.CheckCritic, delivery.OutcomePassed),
		},
	}
	// It is ADR-0008's heart: yesterday's green is not today's green.
	if ev.Green() {
		t.Fatal("evidence from another commit must not approve the commit under review")
	}
	if !strings.Contains(ev.Reason(), "not to commit") {
		t.Errorf("a recusa deveria apontar o commit divergente: %s", ev.Reason())
	}
}

func TestEvidenceWithAFailureIsNotGreen(t *testing.T) {
	ev := delivery.Evidence{
		DemandID: "dem-1", RepoID: repo1, Commit: commit1,
		Runs: []delivery.VerificationRun{
			execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed),
			execucao("dem-1", repo1, commit1, delivery.CheckCritic, delivery.OutcomePassed),
			execucao("dem-1", repo1, commit1, delivery.CheckE2E, delivery.OutcomeFailed),
		},
	}
	if ev.Green() {
		t.Fatal("one failed run brings down the whole green")
	}
}

func TestEvidenceRequiresTheCriticsOpinion(t *testing.T) {
	ev := delivery.Evidence{
		DemandID: "dem-1", RepoID: repo1, Commit: commit1,
		Runs: []delivery.VerificationRun{
			execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed),
		},
	}
	if ev.Green() {
		t.Fatal("an acceptance with no critic opinion does not close ADR-0007 §3's gate")
	}
}

func TestARunWithNoTraceIsNotEvidence(t *testing.T) {
	r := execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed)
	r.SandboxID, r.LogRef = "", ""
	if err := r.Validate(); err == nil {
		t.Error("'it passed' with no sandbox and no log is a word, not evidence")
	}
	r = execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed)
	r.Commit = ""
	if err := r.Validate(); err == nil {
		t.Error("a run with no commit proves nothing and should be refused")
	}
}

func TestAPRDoesNotOpenWithoutGreen(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	_, err := svc.OpenPullRequest(ctx, delivery.OpenSpec{
		DemandID: "dem-1", RepoID: repo1, SourceBranch: "feat/x", HeadCommit: commit1,
	}, "idem-1")
	if err == nil || errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("sem green, sem PR (ADR-0007); erro veio: %v", err)
	}
	if !strings.Contains(err.Error(), "acceptance") {
		t.Errorf("a recusa deveria dizer o que falta: %v", err)
	}
}

// ── the queue refuses without evidence (ADR-0007 + ADR-0008) ────────────────

func TestEnqueueWithoutAPRIsRefused(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	_, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if err == nil || errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("a demand with no PR does not enter the queue; error was: %v", err)
	}
}

func TestEnqueueWithoutEvidenceOfGreenIsRefused(t *testing.T) {
	svc, repo, _ := cenario()
	ctx := comoAtor("ed")

	// PR aberto com o commit green...
	green(t, svc, ctx, "dem-1", repo1, commit1)
	pr := abrePR(t, svc, ctx, "dem-1", repo1, commit1)

	// ...and then the branch moved: the PR now points at a commit nobody
	// verified. It is exactly the hole ADR-0008's re-verification closes.
	for i := range repo.prs {
		if repo.prs[i].ID == pr.ID {
			repo.prs[i].HeadCommit = commit2
		}
	}

	_, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if err == nil {
		t.Fatal("the queue accepted an entry with no evidence for the current commit")
	}
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("recusa deveria ser failed_precondition, veio %s: %v", errs.KindOf(err), err)
	}
	// The message has to say WHAT is missing, not only that something was.
	if !strings.Contains(err.Error(), "acceptance") || !strings.Contains(err.Error(), "critic") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

func TestEnqueueWithAFailedAcceptanceIsRefused(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	green(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)

	// A new suite fails on the SAME commit: the green ceases to exist.
	if _, err := svc.RecordVerification(ctx,
		execucao("dem-1", repo1, commit1, delivery.CheckE2E, delivery.OutcomeFailed), ""); err != nil {
		t.Fatalf("recording the failure: %v", err)
	}

	if _, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1"); err == nil {
		t.Fatal("the queue accepted a PR with a failed run on the commit")
	}
}

func TestEnqueueWithGreenEntersTheQueue(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	green(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)

	e, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if err != nil {
		t.Fatalf("with evidence of green the entry should be accepted: %v", err)
	}
	if e.State != delivery.StateQueued || e.Seq == 0 {
		t.Errorf("entrada malformada: estado=%s seq=%d", e.State, e.Seq)
	}
}

func TestOnePRDoesNotEnterTheSameRepoQueueTwice(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	green(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	if _, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1"); err != nil {
		t.Fatalf("primeira entrada: %v", err)
	}
	if _, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q2"); err == nil {
		t.Fatal("the same PR entered the same repository's queue twice")
	}
}

func TestTheQueueIsAlwaysPerRepository(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	for _, d := range []string{"dem-1", "dem-2"} {
		green(t, svc, ctx, d, repo1, commit1)
		abrePR(t, svc, ctx, d, repo1, commit1)
		if _, err := svc.EnqueueMerge(ctx, repo1, d, "idem-"+d); err != nil {
			t.Fatalf("enfileirar %s: %v", d, err)
		}
	}
	green(t, svc, ctx, "dem-0", repo2, commit2)
	abrePR(t, svc, ctx, "dem-0", repo2, commit2)
	if _, err := svc.EnqueueMerge(ctx, repo2, "dem-0", "idem-dem-0"); err != nil {
		t.Fatalf("enfileirar no repo 2: %v", err)
	}

	queue1, _ := svc.GetMergeQueue(ctx, repo1)
	queue2, _ := svc.GetMergeQueue(ctx, repo2)
	if len(queue1) != 2 || len(queue2) != 1 {
		t.Fatalf("the queues are not independent per repository: %d and %d", len(queue1), len(queue2))
	}
	// Each queue numbers from 1: the position is within the repository, not global.
	if queue1[0].Position != 1 || queue2[0].Position != 1 {
		t.Errorf("positions outside the repository: %d and %d", queue1[0].Position, queue2[0].Position)
	}
}

// ── deterministic order (ADR-0008) ──────────────────────────────────────────

// Two entries with the SAME priority must not tie: the per-repository sequence
// always breaks it. A queue with ties is a queue whose order changes between
// dois refreshes do cockpit.
func TestQueueOrderIsDeterministicUnderATie(t *testing.T) {
	base := []delivery.MergeQueueEntry{
		{ID: "c", RepoID: repo1, Priority: delivery.DefaultPriority, Seq: 3, State: delivery.StateQueued},
		{ID: "a", RepoID: repo1, Priority: delivery.DefaultPriority, Seq: 1, State: delivery.StateQueued},
		{ID: "b", RepoID: repo1, Priority: delivery.DefaultPriority, Seq: 2, State: delivery.StateQueued},
	}
	esperado := []string{"a", "b", "c"}

	// Input shuffled in various ways ALWAYS produces the same output.
	for _, entrada := range [][]delivery.MergeQueueEntry{
		{base[0], base[1], base[2]},
		{base[2], base[0], base[1]},
		{base[1], base[2], base[0]},
	} {
		queue := delivery.SortQueue(entrada)
		for i := range queue {
			if queue[i].ID != esperado[i] || queue[i].Position != int32(i+1) {
				t.Fatalf("unstable order: %s at position %d, expected %s",
					queue[i].ID, queue[i].Position, esperado[i])
			}
		}
	}

	// Ordem TOTAL: para nenhum par distinto os dois lados podem ser falsos —
	// that is what "there is no ambiguous tie" means.
	for i := range base {
		for j := range base {
			if i == j {
				continue
			}
			if !base[i].Before(base[j]) && !base[j].Before(base[i]) {
				t.Errorf("ambiguous tie between %s and %s", base[i].ID, base[j].ID)
			}
		}
	}
}

func TestPriorityComesBeforeArrival(t *testing.T) {
	// Diretriz de ordem preferencial (ADR-0015 §6) mexe na prioridade: quem
	// arrived later may merge earlier. Note that nobody STOPPED: the demand that
	// lost its turn keeps running, it just merges later.
	queue := delivery.SortQueue([]delivery.MergeQueueEntry{
		{ID: "primeiro", Priority: delivery.DefaultPriority, Seq: 1},
		{ID: "urgente", Priority: 10, Seq: 2},
	})
	if queue[0].ID != "urgente" || queue[1].ID != "primeiro" {
		t.Fatalf("priority was not honoured: %s, %s", queue[0].ID, queue[1].ID)
	}
}

func TestTheQueueDoesNotShowWhatAlreadyMerged(t *testing.T) {
	svc, repo, _ := cenario()
	ctx := comoAtor("ed")
	green(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	e, _ := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	repo.entries[0].State = delivery.StateMerged

	queue, err := svc.GetMergeQueue(ctx, repo1)
	if err != nil {
		t.Fatalf("consultar queue: %v", err)
	}
	if len(queue) != 0 {
		t.Errorf("entrada %s mergeada continua ocupando a queue", e.ID)
	}
}

// ── a conflict becomes a human decision item (ADR-0008 §2) ──────────────────

func TestAConflictEscalatesWithItsReport(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")
	green(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	e, _ := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")

	if _, err := svc.AdvanceQueue(ctx, e.ID, delivery.StateRebasing, "idem-r1"); err != nil {
		t.Fatalf("ir para rebase: %v", err)
	}
	// An empty report gives nobody anything to decide — the attention box gets
	// context, not an alarm.
	if _, err := svc.ReportConflict(ctx, e.ID, delivery.ConflictReport{}, "idem-c0"); err == nil {
		t.Error("a conflict with neither files nor a description should be refused")
	}

	got, err := svc.ReportConflict(ctx, e.ID, delivery.ConflictReport{
		Files: []string{"internal/app/register.go"}, BaseCommit: commit2, Attempts: 3,
		Detail: "the rebase failed three times on the same hunk",
	}, "idem-c1")
	if err != nil {
		t.Fatalf("escalar conflito: %v", err)
	}
	if !got.State.NeedsHuman() {
		t.Error("a conflict has to set the state that feeds the attention box")
	}
	if got.Conflict == nil || got.Conflict.ReportedAt.IsZero() {
		t.Error("o relato do conflito precisa ficar gravado, com quando")
	}
	// A conflict is not the end: once resolved, it goes back to the rebase.
	if !delivery.StateConflict.CanTransitionTo(delivery.StateRebasing) {
		t.Error("conflito resolvido deveria voltar para o rebase")
	}
}

func TestTheQueueDoesNotSkipReverification(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")
	green(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	e, _ := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")

	// "queued → merged" would skip the rebase and the re-verification, which is
	// where the semantic break between parallel demands shows up (ADR-0008).
	if _, err := svc.AdvanceQueue(ctx, e.ID, delivery.StateMerged, "idem-x"); err == nil {
		t.Fatal("the queue allowed a merge without going through rebase and re-verification")
	}
}

// ── diretrizes (ADR-0015) ───────────────────────────────────────────────────

func diretrizExemplo() delivery.Directive {
	return delivery.Directive{
		ProjectID: projeto,
		Kind:      delivery.DirectiveCherryPick,
		Summary:   "demand 1 depends on the contract demand 0 is writing",
		Payload: map[string]any{
			"overlapping_files": []any{"internal/app/register.go"},
		},
		AffectedDemands: []string{"dem-0", "dem-1"},
		Recommended:     "cherry-pick",
		Options: []delivery.DirectiveOption{
			{
				Key:     "cherry-pick",
				Summary: "quando a 0 commitar o contrato, a 1 faz cherry-pick e segue",
				Instructions: []delivery.Instruction{{
					DemandID: "dem-1", Action: delivery.DirectiveCherryPick,
					When:    "demanda dem-0 commitou o contrato",
					Payload: map[string]any{"from_demand": "dem-0"},
				}},
			},
			{
				Key:     "ordem-de-merge",
				Summary: "as duas seguem; a 0 mergeia antes da 1",
				Instructions: []delivery.Instruction{{
					DemandID: "dem-1", Action: delivery.DirectiveMergeOrder,
					Payload: map[string]any{"priority": 200, "repo_id": repo1},
				}},
			},
		},
	}
}

func TestADirectiveNeedsOptionsAndARecommendation(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	d := diretrizExemplo()
	d.Options = d.Options[:1]
	if _, err := svc.ProposeDirective(ctx, d, "idem-d0"); err == nil {
		t.Error("a single option is an alarm with an OK button, not a request for a decision")
	}

	d = diretrizExemplo()
	d.Recommended = "inexistente"
	if _, err := svc.ProposeDirective(ctx, d, "idem-d0"); err == nil {
		t.Error("a recommendation outside the options should be refused")
	}
}

// ADR-0015 §5's golden rule written as a TYPE: there is no value in the
// vocabulary that expresses stopping a demand.
func TestTheCoordinationVocabularyHasNoPause(t *testing.T) {
	for _, attempt := range []delivery.DirectiveKind{"pause", "block", "suspend", "wait", ""} {
		if delivery.ValidDirectiveKind(attempt) {
			t.Errorf("%q must not exist in the coordination vocabulary", attempt)
		}
		ins := delivery.Instruction{DemandID: "dem-1", Action: attempt}
		if err := ins.Validate(); err == nil {
			t.Errorf("instruction %q should be refused", attempt)
		}
	}

	svc, _, _ := cenario()
	ctx := comoAtor("ed")
	d := diretrizExemplo()
	d.Options[0].Instructions[0].Action = "pause"
	if _, err := svc.ProposeDirective(ctx, d, "idem-d0"); err == nil {
		t.Fatal("the domain accepted a directive that would pause the demand")
	}
}

func TestDecidingRequiresWhoAndWhy(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")
	d, err := svc.ProposeDirective(ctx, diretrizExemplo(), "idem-d1")
	if err != nil {
		t.Fatalf("propor: %v", err)
	}

	// With no reason: refused. A decision with no why becomes invisible magic.
	if _, err := svc.DecideDirective(ctx, d.ID,
		map[string]any{"option": "cherry-pick"}, "idem-x1"); err == nil {
		t.Error("decidir sem motivo deveria ser recusado")
	}
	// An option that was not offered: refused, saying which ones exist.
	_, err = svc.DecideDirective(ctx, d.ID,
		map[string]any{"option": "cancelar-tudo", "rationale": "porque sim"}, "idem-x2")
	if err == nil || !strings.Contains(err.Error(), "cherry-pick") {
		t.Errorf("the refusal should list the options offered: %v", err)
	}
	// With no identified actor: nobody to record as the decision's author.
	semAtor := ctxutil.Into(context.Background(), ctxutil.Call{AccountID: conta})
	if _, err := svc.DecideDirective(semAtor, d.ID,
		map[string]any{"option": "cherry-pick", "rationale": "ok"}, "idem-x3"); err == nil {
		t.Error("a decision with no identified actor should be refused")
	}

	// An agent does not decide its own proposal: the techlead detects and
	// escolhe (ADR-0015 §4).
	comoAgente := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: conta, ActorID: "thread-7", ActorKind: ctxutil.ActorAgent})
	if _, err := svc.DecideDirective(comoAgente, d.ID, map[string]any{
		"option": "cherry-pick", "rationale": "eu mesmo resolvo",
	}, "idem-x0"); err == nil || errs.KindOf(err) != errs.KindPermission {
		t.Errorf("an agent should not decide a directive; error was: %v", err)
	}

	decided, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "cherry-pick", "rationale": "1 only needs the contract, not the rest",
	}, "idem-x4")
	if err != nil {
		t.Fatalf("decidir: %v", err)
	}
	if decided.Decision == nil || decided.Decision.DecidedBy != "ed" ||
		decided.Decision.Rationale == "" || !decided.Decision.DecidedAt.Equal(agora) {
		t.Fatalf("the decision has to record who, what and why: %+v", decided.Decision)
	}
	// Repeating the SAME decision is harmless; changing your mind needs a new directive.
	if _, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "cherry-pick", "rationale": "de novo",
	}, "idem-x4"); err != nil {
		t.Errorf("repeating the same decision should be harmless: %v", err)
	}
	if _, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "ordem-de-merge", "rationale": "mudei de ideia",
	}, "idem-x5"); err == nil {
		t.Error("rewriting a decision already made should be a conflict")
	}
}

// O teste que a regra de ouro pede: decidir uma diretriz NÃO interrompe a
// a demand that is already moving.
//
// Two assertions, and both matter. The first is about state: demand 1 stays
// active, and its queue entry stays where it was. The second is structural:
// everything the decision produced was WORK TO DO — no instruction outside the
// coordination vocabulary, because none could exist.
func TestDecidingADirectiveDoesNotInterruptARunningDemand(t *testing.T) {
	svc, repo, dem := cenario()
	ctx := comoAtor("ed")

	// Demand 1 is moving: a green PR open and an entry in the queue.
	green(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	entrada, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if err != nil {
		t.Fatalf("enfileirar: %v", err)
	}
	if _, err := svc.AdvanceQueue(ctx, entrada.ID, delivery.StateRebasing, "idem-r1"); err != nil {
		t.Fatalf("advancing: %v", err)
	}

	// O techlead detecta a transversal com a demanda 0 e o dev decide.
	d, err := svc.ProposeDirective(ctx, diretrizExemplo(), "idem-d1")
	if err != nil {
		t.Fatalf("propor diretriz: %v", err)
	}
	if _, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "cherry-pick", "rationale": "a 1 segue e faz cherry-pick quando a 0 commitar",
	}, "idem-x1"); err != nil {
		t.Fatalf("decidir: %v", err)
	}

	// 1. The demand stays active — delivery only READS demands, there is no way to stop one.
	if info, _ := dem.Demand(ctx, conta, "dem-1"); !info.Active {
		t.Error("a demanda parou por causa de uma transversal identificada (ADR-0015 §5)")
	}
	// 2. A entrada dela na queue continua exatamente onde estava.
	alive, _ := repo.QueueEntryByID(ctx, conta, entrada.ID)
	if alive.State != delivery.StateRebasing {
		t.Errorf("the decision touched the demand's progress: state became %s", alive.State)
	}
	// 3. And what the decision delivered was work, not an order to stop.
	if len(repo.delivered) == 0 {
		t.Fatal("the decision delivered no coordination at all")
	}
	for _, ins := range repo.delivered {
		if err := ins.Validate(); err != nil {
			t.Errorf("instruction delivered outside the coordination vocabulary: %v", err)
		}
		if ins.Action == delivery.DirectiveCherryPick && ins.When == "" {
			t.Error("a cherry-pick with no condition becomes a wait: the demand has to know WHEN to apply it")
		}
	}
}

// A preferred order is the case where the directive TOUCHES the queue — and even
// so it stops nobody: it changes the priority, the demand's state does not move.
func TestAnOrderingDirectiveReordersWithoutStopping(t *testing.T) {
	svc, repo, _ := cenario()
	ctx := comoAtor("ed")

	green(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	entrada, _ := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if _, err := svc.AdvanceQueue(ctx, entrada.ID, delivery.StateRebasing, "idem-r1"); err != nil {
		t.Fatalf("advancing: %v", err)
	}

	d, _ := svc.ProposeDirective(ctx, diretrizExemplo(), "idem-d1")
	if _, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "ordem-de-merge", "rationale": "a 0 precisa entrar antes",
	}, "idem-x1"); err != nil {
		t.Fatalf("decidir: %v", err)
	}

	after, _ := repo.QueueEntryByID(ctx, conta, entrada.ID)
	if after.Priority != 200 {
		t.Errorf("the preferred order was not applied: priority %d", after.Priority)
	}
	if after.State != delivery.StateRebasing {
		t.Errorf("reordenar mudou o andamento da demanda: %s", after.State)
	}
}

// ── isolamento por conta ────────────────────────────────────────────────────

func TestAnOperationWithNoActiveAccountIsRefused(t *testing.T) {
	svc, _, _ := cenario()
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "ed"})

	if _, err := svc.GetMergeQueue(ctx, repo1); err == nil {
		t.Error("consultar queue sem conta ativa deveria ser recusado")
	}
	if _, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "k"); err == nil {
		t.Error("enfileirar sem conta ativa deveria ser recusado")
	}
	if _, err := svc.ListDirectives(ctx, projeto); err == nil {
		t.Error("listar diretrizes sem conta ativa deveria ser recusado")
	}
}

func TestTheServiceRefusesANilClock(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a nil clock has to fail at boot, not in production")
		}
	}()
	delivery.NewService(novoRepo(), &fakeDemands{}, nil, nil)
}
