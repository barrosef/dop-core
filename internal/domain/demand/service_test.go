package demand_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT a database and WITHOUT a broker: repository,
// flow and event subscription are ports, and in-memory doubles go in here. The doubles live in this
// file because the architecture test fails any adapter import
// sob internal/domain — inclusive em _test.go.

// ── o congelamento do fluxo ──────────────────────────────────────────────────

// It is the most expensive guarantee to lose: a flow changed later must NOT
// past of a demand already under way (ADR-0014 §4).
func TestStartFreezesTheFlow(t *testing.T) {
	repo, flows, _, svc := setup(t)
	ctx := comConta("acc-1")

	d, err := svc.Start(ctx, project, "SUOPT-1315", "idem-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d.Flow.Version != 1 || len(d.Stages) != 3 {
		t.Fatalf("it froze wrong: v%d with %d stages", d.Flow.Version, len(d.Stages))
	}

	// The catalogue's flow changes — a new version, different stages.
	flows.flow = demand.Flow{
		ID: "flow-1", Name: "revised flow", Version: 2, ResolvedFrom: "project",
		Stages: []demand.StageSpec{{Key: "new", Name: "New stage", Type: demand.TypeGeneric}},
	}

	reloaded, err := svc.Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Flow.Version != 1 {
		t.Errorf("the demand followed the live flow: v%d (expected the frozen v1)", reloaded.Flow.Version)
	}
	if len(reloaded.Stages) != 3 || reloaded.Stages[0].Key != "context" {
		t.Errorf("stages rewritten by the new flow: %+v", keys(reloaded.Stages))
	}
	if reloaded.Flow.FrozenAt.IsZero() {
		t.Error("a snapshot with no freezing instant")
	}

	// Restarting the SAME external key returns the demand as it stands:
	// re-resolving would freeze again, which is exactly what freezing prevents.
	other, err := svc.Start(ctx, project, "SUOPT-1315", "idem-2")
	if err != nil {
		t.Fatalf("Start repetido: %v", err)
	}
	if other.ID != d.ID {
		t.Errorf("a repeated Start created a new demand: %s != %s", other.ID, d.ID)
	}
	if flows.calls != 1 {
		t.Errorf("the flow was resolved %d times; it should be 1", flows.calls)
	}
	if got := repo.tipos(); len(got) != 1 || got[0] != demand.EventStarted {
		t.Errorf("events emitted = %v, expected only %s", got, demand.EventStarted)
	}
}

func TestStartRefusesABrokenFlow(t *testing.T) {
	_, flows, _, svc := setup(t)
	ctx := comConta("acc-1")

	flows.flow = demand.Flow{ID: "flow-x", Name: "vazio", Version: 3}
	if _, err := svc.Start(ctx, project, "SUOPT-1", ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("a flow with no stages should be refused, got %v", err)
	}

	flows.flow = demand.Flow{ID: "flow-y", Name: "duplicado", Version: 1, Stages: []demand.StageSpec{
		{Key: "spec", Type: demand.TypeSpec}, {Key: "spec", Type: demand.TypePlan},
	}}
	if _, err := svc.Start(ctx, project, "SUOPT-2", ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("a repeated stage key should have been refused, got %v", err)
	}
}

// ── the stage machine ────────────────────────────────────────────────────────

func TestAnInvalidStageTransitionIsRefused(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	// pending → done skips the start: the stage would never have begun.
	_, err := svc.AdvanceStage(ctx, d.ID, "context", demand.StageDone, "")
	requireInvalid(t, err, "context")

	// A stage out of order: starting the 2nd with the 1st pending would hide
	// skipped work behind progress that looks legitimate.
	_, err = svc.AdvanceStage(ctx, d.ID, "spec", demand.StageRunning, "")
	requireInvalid(t, err, "context")

	// A stage that does not exist in the frozen flow.
	_, err = svc.AdvanceStage(ctx, d.ID, "inexistente", demand.StageRunning, "")
	requireInvalid(t, err, "inexistente")

	// An unknown status (the contract's UNSPECIFIED arrives like this).
	_, err = svc.AdvanceStage(ctx, d.ID, "context", demand.StageStatus(""), "")
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("an empty status should be refused, got %v", err)
	}

	// The legitimate path keeps working, and finished is terminal.
	avancar(t, svc, ctx, d.ID, "context", demand.StageRunning)
	avancar(t, svc, ctx, d.ID, "context", demand.StageDone)
	_, err = svc.AdvanceStage(ctx, d.ID, "context", demand.StageRunning, "")
	requireInvalid(t, err, "context")
}

// The stage's TYPE drives the machine: a human gate does not close on its own.
func TestAHumanGateDoesNotCloseThroughAdvance(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	avancar(t, svc, ctx, d.ID, "context", demand.StageRunning)
	avancar(t, svc, ctx, d.ID, "context", demand.StageDone)
	avancar(t, svc, ctx, d.ID, "spec", demand.StageRunning)
	avancar(t, svc, ctx, d.ID, "spec", demand.StageDone)
	avancar(t, svc, ctx, d.ID, "validation", demand.StageRunning)

	_, err := svc.AdvanceStage(ctx, d.ID, "validation", demand.StageDone, "")
	requireInvalid(t, err, "validation")
	if !strings.Contains(err.Error(), "DecideGate") {
		t.Errorf("the refusal should point at the right path: %v", err)
	}

	// Rejecting does not clear the stage: it blocks with the comment, so the
	// rejection stays visible.
	st, err := svc.DecideGate(ctx, d.ID, "validation", false, "acceptance criteria missing", "")
	if err != nil {
		t.Fatalf("DecideGate reprovando: %v", err)
	}
	if st.Status != demand.StageBlocked || st.GateComment == "" {
		t.Errorf("a rejection should block with a comment, got %s/%q", st.Status, st.GateComment)
	}

	st, err = svc.DecideGate(ctx, d.ID, "validation", true, "ok", "")
	if err != nil {
		t.Fatalf("DecideGate aprovando: %v", err)
	}
	if st.Status != demand.StageDone || st.FinishedAt == nil {
		t.Errorf("an approval should finish the stage, got %s", st.Status)
	}

	// With every stage finished, the demand's projected status closes.
	final, err := svc.Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != demand.StatusDone {
		t.Errorf("projected status = %s, expected %s", final.Status, demand.StatusDone)
	}
}

func TestDecideGateOnlyWhereThereIsAGate(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	// A stage with no gate has nothing to decide.
	_, err := svc.DecideGate(ctx, d.ID, "context", true, "", "")
	requireInvalid(t, err, "context")

	// A stage with a gate that has not even started.
	_, err = svc.DecideGate(ctx, d.ID, "validation", true, "", "")
	requireInvalid(t, err, "validation")

	// An agent approving its own work: what the gate exists to prevent.
	comoAgente := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: "acc-1", ActorID: "thread-9", ActorKind: ctxutil.ActorAgent})
	if _, err := svc.DecideGate(comoAgente, d.ID, "validation", true, "", ""); errs.KindOf(err) != errs.KindPermission {
		t.Errorf("an agent does not decide a human gate, got %v", err)
	}
}

// ── threads and findings ─────────────────────────────────────────────────────

// The rule the spec puts in one sentence: the thread does not die in silence.
func TestAThreadDoesNotConcludeWithoutAPublishedFinding(t *testing.T) {
	repo, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	th, err := svc.CreateThread(ctx, d.ID, "db-forensics", demand.AgentCard{
		Purpose: "database forensics", Model: "opus", Effort: "high",
		Tools: []string{"mcp:mysql"}, BudgetMicros: 500_000,
	}, "")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if th.State != demand.ThreadOpen {
		t.Errorf("a thread should be born open, got %s", th.State)
	}

	if _, err := svc.PostMessage(ctx, th.ID, "olhando os deadlocks", ""); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}

	// With no published finding, concluding is refused — and the refusal says why.
	err = nil
	if _, err = svc.ConcludeThread(ctx, th.ID, ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("concluding with no finding should fail with a precondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "finding") {
		t.Errorf("the refusal should mention the finding: %v", err)
	}

	f, err := svc.PublishFinding(ctx, d.ID, th.ID, "deadlock on table X",
		map[string]any{"janela": "14:02-14:07", "causa": "migration Y"}, "")
	if err != nil {
		t.Fatalf("PublishFinding: %v", err)
	}
	if f.ID == "" || f.ThreadID != th.ID {
		t.Errorf("the finding was stored wrong: %+v", f)
	}

	concluded, err := svc.ConcludeThread(ctx, th.ID, "")
	if err != nil {
		t.Fatalf("ConcludeThread after the finding: %v", err)
	}
	if concluded.State != demand.ThreadConcluded {
		t.Errorf("thread state = %s, expected %s", concluded.State, demand.ThreadConcluded)
	}

	// After the finding, the durable record is the finding: the conversation does not come back.
	if _, err := svc.PostMessage(ctx, th.ID, "one more thing", ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("a concluded thread takes no message, got %v", err)
	}

	// Every action became an event (ADR-0006) — an action with no event is a bug.
	want := []string{
		demand.EventStarted, demand.EventThreadCreated, demand.EventMessagePosted,
		demand.EventFindingPublished, demand.EventThreadConcluded,
	}
	if got := repo.tipos(); !mesmaLista(got, want) {
		t.Errorf("events = %v, expected %v", got, want)
	}
}

func TestAFindingFromAnotherDemandIsRefused(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	a := iniciar(t, svc, ctx, "SUOPT-1")
	b := iniciar(t, svc, ctx, "SUOPT-2")

	th, err := svc.CreateThread(ctx, a.ID, "principal", demand.AgentCard{Purpose: "implementar"}, "")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// The demand is the security boundary (ADR-0010 §6).
	if _, err := svc.PublishFinding(ctx, b.ID, th.ID, "finding", nil, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("a finding crossing demands should have been refused, got %v", err)
	}
}

func TestTheSubagentBriefIsRequired(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	// A subagent with no purpose is a black box — what ADR-0010 refuses.
	if _, err := svc.CreateThread(ctx, d.ID, "logs", demand.AgentCard{}, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("a brief with no purpose should be refused, got %v", err)
	}
	if _, err := svc.CreateThread(ctx, d.ID, "Logs Do Servidor",
		demand.AgentCard{Purpose: "ler logs"}, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("an invalid thread key should be refused, got %v", err)
	}
	if _, err := svc.CreateThread(ctx, d.ID, "logs",
		demand.AgentCard{Purpose: "ler logs", Effort: "turbo"}, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("an effort outside the menu should be refused, got %v", err)
	}
}

// Blocking is what feeds the attention box.
func TestABlockedThreadEntersTheAttentionBox(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")
	th, err := svc.CreateThread(ctx, d.ID, "principal", demand.AgentCard{Purpose: "implementar"}, "")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	bloqueada, err := svc.SetThreadBlocked(ctx, th.ID, true, "posso dropar a coluna?", "")
	if err != nil {
		t.Fatalf("SetThreadBlocked: %v", err)
	}
	if !bloqueada.Blocked() {
		t.Error("the thread should be blocked")
	}
	solta, err := svc.SetThreadBlocked(ctx, th.ID, false, "", "")
	if err != nil {
		t.Fatalf("SetThreadBlocked desbloqueando: %v", err)
	}
	if solta.Blocked() {
		t.Error("the thread should have gone back to active")
	}
}

// ── isolamento e streaming ───────────────────────────────────────────────────

func TestAQueryWithNoAccountIsInvalid(t *testing.T) {
	_, _, _, svc := setup(t)
	if _, err := svc.Get(context.Background(), "qualquer"); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("a request with no active account should be invalid, got %v", err)
	}
}

func TestAnotherAccountsDemandDoesNotAppear(t *testing.T) {
	_, _, _, svc := setup(t)
	d := iniciar(t, svc, comConta("acc-1"), "SUOPT-1315")

	if _, err := svc.Get(comConta("acc-2"), d.ID); errs.KindOf(err) != errs.KindNotFound {
		t.Error("another account's demand should be invisible")
	}
}

// WatchDemand delivers THIS demand's events — the slice by id belongs here,
// because the bus filter is by aggregate and type.
func TestWatchDeliversOnlyTheDemandsEvents(t *testing.T) {
	_, _, watcher, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	var seen []string
	err := watcher.deliver(ctx, svc, d.ID, []ports.Event{
		{ID: "e1", Aggregate: "demand", AggregateID: d.ID, Type: demand.EventStarted},
		{ID: "e2", Aggregate: "demand", AggregateID: "other", Type: demand.EventMessagePosted},
		{ID: "e3", Aggregate: "demand", AggregateID: d.ID, Type: demand.EventMessagePosted},
	}, func(e ports.Event) error {
		seen = append(seen, e.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !mesmaLista(seen, []string{"e1", "e3"}) {
		t.Errorf("events delivered = %v, expected [e1 e3]", seen)
	}
}

func TestTheConstructorRefusesANilDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a nil clock should blow up at assembly, not in production")
		}
	}()
	demand.NewService(newFakeRepo(), &fakeFlows{}, &fakeWatcher{}, nil)
}

// ── in-memory doubles ────────────────────────────────────────────────────────

const project = "proj-1"

// fixedClock is the Clock double: without it, StartedAt and FinishedAt would
// depend on the wall clock and nothing would be verifiable by equality.
type fixedClock struct{ t time.Time }

func (r fixedClock) Now() time.Time { return r.t }

type fakeFlows struct {
	flow  demand.Flow
	calls int
}

func (f *fakeFlows) Resolve(_ context.Context, _, _, _ string) (demand.Flow, error) {
	f.calls++
	return f.flow, nil
}

// fakeWatcher keeps the filter it was asked for and delivers the events it is given.
type fakeWatcher struct {
	aggregates []string
	queue      []ports.Event
}

func (w *fakeWatcher) Watch(_ context.Context, _ string, aggregates, _ []string, emit func(ports.Event) error) error {
	w.aggregates = aggregates
	for _, e := range w.queue {
		if err := emit(e); err != nil {
			return err
		}
	}
	return nil
}

// deliver runs the service's Watch with a prepared queue.
func (w *fakeWatcher) deliver(ctx context.Context, svc *demand.Service, demandID string, queue []ports.Event, emit func(ports.Event) error) error {
	w.queue = queue
	return svc.Watch(ctx, demandID, emit)
}

type fakeRepo struct {
	demands  map[string]*demand.Demand
	threads  map[string]*demand.Thread
	findings []demand.Finding
	// events is what proves ADR-0006's rule: no write passes through here without
	// bringing the event along.
	events []demand.Emission
	seq    int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{demands: map[string]*demand.Demand{}, threads: map[string]*demand.Thread{}}
}

func (f *fakeRepo) id(prefixo string) string {
	f.seq++
	return prefixo + "-" + string(rune('a'+f.seq))
}

func (f *fakeRepo) tipos() []string {
	out := make([]string, 0, len(f.events))
	for _, e := range f.events {
		out = append(out, e.Type)
	}
	return out
}

func (f *fakeRepo) List(_ context.Context, accountID, projectID string, limit int, _ string) ([]demand.Demand, error) {
	out := []demand.Demand{}
	for _, d := range f.demands {
		if d.AccountID == accountID && (projectID == "" || d.ProjectID == projectID) {
			out = append(out, *d)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) ByID(_ context.Context, accountID, id string) (*demand.Demand, error) {
	d, ok := f.demands[id]
	if !ok || d.AccountID != accountID {
		return nil, nil
	}
	copia := *d
	copia.Stages = append([]demand.Stage(nil), d.Stages...)
	return &copia, nil
}

func (f *fakeRepo) ByExternalKey(_ context.Context, accountID, projectID, key string) (*demand.Demand, error) {
	for _, d := range f.demands {
		if d.AccountID == accountID && d.ProjectID == projectID && d.ExternalKey == key {
			copia := *d
			return &copia, nil
		}
	}
	return nil, nil
}

func (f *fakeRepo) Create(_ context.Context, d *demand.Demand, ev demand.Emission, _ string) (*demand.Demand, error) {
	saved := *d
	saved.ID = f.id("dem")
	f.demands[saved.ID] = &saved
	f.events = append(f.events, ev)
	return &saved, nil
}

func (f *fakeRepo) SaveStage(_ context.Context, accountID, demandID string, st demand.Stage, status demand.DopStatus, ev demand.Emission, _ string) (*demand.Stage, error) {
	d, ok := f.demands[demandID]
	if !ok || d.AccountID != accountID {
		return nil, errs.NotFound("demand")
	}
	for i := range d.Stages {
		if d.Stages[i].Key == st.Key {
			d.Stages[i] = st
			d.Status = status
			f.events = append(f.events, ev)
			saved := st
			return &saved, nil
		}
	}
	return nil, errs.NotFound("stage")
}

func (f *fakeRepo) ThreadsOf(_ context.Context, accountID, demandID string) ([]demand.Thread, error) {
	out := []demand.Thread{}
	for _, t := range f.threads {
		if t.AccountID == accountID && t.DemandID == demandID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (f *fakeRepo) ThreadByID(_ context.Context, accountID, id string) (*demand.Thread, error) {
	t, ok := f.threads[id]
	if !ok || t.AccountID != accountID {
		return nil, nil
	}
	copia := *t
	return &copia, nil
}

func (f *fakeRepo) CreateThread(_ context.Context, t *demand.Thread, ev demand.Emission, _ string) (*demand.Thread, error) {
	saved := *t
	saved.ID = f.id("thr")
	f.threads[saved.ID] = &saved
	f.events = append(f.events, ev)
	return &saved, nil
}

func (f *fakeRepo) SaveThreadState(_ context.Context, accountID, threadID string, state demand.ThreadState, ev demand.Emission, _ string) (*demand.Thread, error) {
	t, ok := f.threads[threadID]
	if !ok || t.AccountID != accountID {
		return nil, errs.NotFound("thread")
	}
	t.State = state
	f.events = append(f.events, ev)
	copia := *t
	return &copia, nil
}

func (f *fakeRepo) AppendMessage(_ context.Context, m *demand.Message, ev demand.Emission, _ string) (*demand.Message, error) {
	saved := *m
	saved.ID = f.id("msg")
	if t, ok := f.threads[m.ThreadID]; ok && t.State == demand.ThreadOpen {
		t.State = demand.ThreadActive
	}
	f.events = append(f.events, ev)
	return &saved, nil
}

func (f *fakeRepo) CreateFinding(_ context.Context, fd *demand.Finding, ev demand.Emission, _ string) (*demand.Finding, error) {
	saved := *fd
	saved.ID = f.id("fnd")
	f.findings = append(f.findings, saved)
	f.events = append(f.events, ev)
	return &saved, nil
}

func (f *fakeRepo) ListFindings(_ context.Context, accountID, demandID string) ([]demand.Finding, error) {
	var findings []demand.Finding
	for _, fd := range f.findings {
		if fd.AccountID == accountID && fd.DemandID == demandID {
			findings = append(findings, fd)
		}
	}
	return findings, nil
}

func (f *fakeRepo) HasFinding(_ context.Context, accountID, threadID string) (bool, error) {
	for _, fd := range f.findings {
		if fd.AccountID == accountID && fd.ThreadID == threadID {
			return true, nil
		}
	}
	return false, nil
}

// ── andaimes ─────────────────────────────────────────────────────────────────

// fluxoPadrao imita o fluxo default da plataforma (ADR-0014 §8), encurtado:
// context → spec → human validation.
func fluxoPadrao() demand.Flow {
	return demand.Flow{
		ID: "flow-1", Name: "default flow", Version: 1, ResolvedFrom: "project ◂ account",
		Stages: []demand.StageSpec{
			{Key: "context", Name: "Contexto", Type: demand.TypeContext},
			{Key: "spec", Name: "Spec", Type: demand.TypeSpec, Artifacts: []demand.ArtifactKind{demand.ArtifactSpec}},
			{Key: "validation", Name: "Validation", Type: demand.TypeHumanValidation, Gate: demand.GateHuman},
		},
	}
}

func setup(t *testing.T) (*fakeRepo, *fakeFlows, *fakeWatcher, *demand.Service) {
	t.Helper()
	repo := newFakeRepo()
	flows := &fakeFlows{flow: fluxoPadrao()}
	watcher := &fakeWatcher{}
	svc := demand.NewService(repo, flows, watcher,
		fixedClock{time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)})
	return repo, flows, watcher, svc
}

func comConta(id string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: id, ActorID: "user-1", ActorKind: ctxutil.ActorUser, ActorName: "Ed"})
}

func iniciar(t *testing.T, svc *demand.Service, ctx context.Context, key string) *demand.Demand {
	t.Helper()
	d, err := svc.Start(ctx, project, key, "")
	if err != nil {
		t.Fatalf("Start(%s): %v", key, err)
	}
	return d
}

func avancar(t *testing.T, svc *demand.Service, ctx context.Context, demandID, stage string, to demand.StageStatus) {
	t.Helper()
	if _, err := svc.AdvanceStage(ctx, demandID, stage, to, ""); err != nil {
		t.Fatalf("AdvanceStage(%s → %s): %v", stage, to, err)
	}
}

// requireInvalid demands the essentials of the refusal: an invalid-argument
// error that SAYS which stage. A generic message here becomes a support ticket later.
func requireInvalid(t *testing.T, err error, stage string) {
	t.Helper()
	if errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("expected a refusal for an invalid argument, got %v", err)
	}
	if !strings.Contains(err.Error(), stage) {
		t.Errorf("the refusal does not say which stage (%q): %v", stage, err)
	}
}

func keys(stages []demand.Stage) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		out = append(out, s.Key)
	}
	return out
}

func mesmaLista(a, b []string) bool {
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
