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
// arquivo porque o teste de arquitetura reprova qualquer import de adaptador
// sob internal/domain — inclusive em _test.go.

// ── o congelamento do fluxo ──────────────────────────────────────────────────

// It is the most expensive guarantee to lose: a flow changed later must NOT
// passado de uma demanda em andamento (ADR-0014 §4).
func TestStartFreezesTheFlow(t *testing.T) {
	repo, flows, _, svc := setup(t)
	ctx := comConta("acc-1")

	d, err := svc.Start(ctx, projeto, "SUOPT-1315", "idem-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d.Flow.Version != 1 || len(d.Stages) != 3 {
		t.Fatalf("congelou errado: v%d com %d etapas", d.Flow.Version, len(d.Stages))
	}

	// The catalogue's flow changes — a new version, different stages.
	flows.flow = demand.Flow{
		ID: "flow-1", Name: "fluxo revisado", Version: 2, ResolvedFrom: "projeto",
		Stages: []demand.StageSpec{{Key: "novo", Name: "Etapa nova", Type: demand.TypeGeneric}},
	}

	recarregada, err := svc.Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if recarregada.Flow.Version != 1 {
		t.Errorf("a demanda seguiu o fluxo vivo: v%d (esperado v1 congelada)", recarregada.Flow.Version)
	}
	if len(recarregada.Stages) != 3 || recarregada.Stages[0].Key != "context" {
		t.Errorf("etapas reescritas pelo fluxo novo: %+v", chaves(recarregada.Stages))
	}
	if recarregada.Flow.FrozenAt.IsZero() {
		t.Error("snapshot sem instante de congelamento")
	}

	// Restarting the SAME external key returns the demand as it stands:
	// re-resolving would freeze again, which is exactly what freezing prevents.
	outra, err := svc.Start(ctx, projeto, "SUOPT-1315", "idem-2")
	if err != nil {
		t.Fatalf("Start repetido: %v", err)
	}
	if outra.ID != d.ID {
		t.Errorf("Start repetido criou demanda nova: %s != %s", outra.ID, d.ID)
	}
	if flows.chamadas != 1 {
		t.Errorf("o fluxo foi resolvido %d vezes; deveria ser 1", flows.chamadas)
	}
	if got := repo.tipos(); len(got) != 1 || got[0] != demand.EventStarted {
		t.Errorf("events emitted = %v, expected only %s", got, demand.EventStarted)
	}
}

func TestStartRefusesABrokenFlow(t *testing.T) {
	_, flows, _, svc := setup(t)
	ctx := comConta("acc-1")

	flows.flow = demand.Flow{ID: "flow-x", Name: "vazio", Version: 3}
	if _, err := svc.Start(ctx, projeto, "SUOPT-1", ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("fluxo sem etapas deveria ser recusado, veio %v", err)
	}

	flows.flow = demand.Flow{ID: "flow-y", Name: "duplicado", Version: 1, Stages: []demand.StageSpec{
		{Key: "spec", Type: demand.TypeSpec}, {Key: "spec", Type: demand.TypePlan},
	}}
	if _, err := svc.Start(ctx, projeto, "SUOPT-2", ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("chave de stage repetida deveria ser recusada, veio %v", err)
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

	// Status desconhecido (o UNSPECIFIED do contrato chega assim).
	_, err = svc.AdvanceStage(ctx, d.ID, "context", demand.StageStatus(""), "")
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("status vazio deveria ser recusado, veio %v", err)
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
		t.Errorf("a recusa deveria apontar o caminho certo: %v", err)
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
		t.Errorf("status projetado = %s, esperado %s", final.Status, demand.StatusDone)
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

// ── threads e achados ────────────────────────────────────────────────────────

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
		t.Errorf("thread deveria nascer aberta, veio %s", th.State)
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
		t.Errorf("achado gravado errado: %+v", f)
	}

	concluida, err := svc.ConcludeThread(ctx, th.ID, "")
	if err != nil {
		t.Fatalf("ConcludeThread depois do achado: %v", err)
	}
	if concluida.State != demand.ThreadConcluded {
		t.Errorf("estado da thread = %s, esperado %s", concluida.State, demand.ThreadConcluded)
	}

	// After the finding, the durable record is the finding: the conversation does not come back.
	if _, err := svc.PostMessage(ctx, th.ID, "mais uma coisa", ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("a concluded thread takes no message, got %v", err)
	}

	// Every action became an event (ADR-0006) — an action with no event is a bug.
	esperados := []string{
		demand.EventStarted, demand.EventThreadCreated, demand.EventMessagePosted,
		demand.EventFindingPublished, demand.EventThreadConcluded,
	}
	if got := repo.tipos(); !mesmaLista(got, esperados) {
		t.Errorf("events = %v, esperado %v", got, esperados)
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
	if _, err := svc.PublishFinding(ctx, b.ID, th.ID, "achado", nil, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("achado cruzando demanda deveria ser recusado, veio %v", err)
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
		t.Error("thread deveria estar bloqueada")
	}
	solta, err := svc.SetThreadBlocked(ctx, th.ID, false, "", "")
	if err != nil {
		t.Fatalf("SetThreadBlocked desbloqueando: %v", err)
	}
	if solta.Blocked() {
		t.Error("thread deveria ter voltado a ativa")
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

	var vistos []string
	err := watcher.deliver(ctx, svc, d.ID, []ports.Event{
		{ID: "e1", Aggregate: "demand", AggregateID: d.ID, Type: demand.EventStarted},
		{ID: "e2", Aggregate: "demand", AggregateID: "outra", Type: demand.EventMessagePosted},
		{ID: "e3", Aggregate: "demand", AggregateID: d.ID, Type: demand.EventMessagePosted},
	}, func(e ports.Event) error {
		vistos = append(vistos, e.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !mesmaLista(vistos, []string{"e1", "e3"}) {
		t.Errorf("events entregues = %v, esperado [e1 e3]", vistos)
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

const projeto = "proj-1"

// fixedClock is the Clock double: without it, StartedAt and FinishedAt would
// depend on the wall clock and nothing would be verifiable by equality.
type fixedClock struct{ t time.Time }

func (r fixedClock) Now() time.Time { return r.t }

type fakeFlows struct {
	flow     demand.Flow
	chamadas int
}

func (f *fakeFlows) Resolve(_ context.Context, _, _, _ string) (demand.Flow, error) {
	f.chamadas++
	return f.flow, nil
}

// fakeWatcher guarda o filtro pedido e entrega os events que lhe derem.
type fakeWatcher struct {
	aggregates []string
	fila       []ports.Event
}

func (w *fakeWatcher) Watch(_ context.Context, _ string, aggregates, _ []string, emit func(ports.Event) error) error {
	w.aggregates = aggregates
	for _, e := range w.fila {
		if err := emit(e); err != nil {
			return err
		}
	}
	return nil
}

// deliver runs the service's Watch with a prepared queue.
func (w *fakeWatcher) deliver(ctx context.Context, svc *demand.Service, demandID string, fila []ports.Event, emit func(ports.Event) error) error {
	w.fila = fila
	return svc.Watch(ctx, demandID, emit)
}

type fakeRepo struct {
	demands  map[string]*demand.Demand
	threads  map[string]*demand.Thread
	findings []demand.Finding
	// events is what proves ADR-0006's rule: no write passes through
	// aqui sem trazer o evento junto.
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
		return nil, errs.NotFound("demanda")
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
	var achados []demand.Finding
	for _, fd := range f.findings {
		if fd.AccountID == accountID && fd.DemandID == demandID {
			achados = append(achados, fd)
		}
	}
	return achados, nil
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
	d, err := svc.Start(ctx, projeto, key, "")
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

func chaves(stages []demand.Stage) []string {
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
