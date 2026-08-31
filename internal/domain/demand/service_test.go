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

// O domínio é testável SEM banco e SEM broker: repositório, fluxo e assinatura
// de eventos são portas, e aqui entram duplos em memória. Os duplos moram neste
// arquivo porque o teste de arquitetura reprova qualquer import de adaptador
// sob internal/domain — inclusive em _test.go.

// ── o congelamento do fluxo ──────────────────────────────────────────────────

// É a garantia mais cara de perder: fluxo alterado depois NÃO pode reescrever o
// passado de uma demanda em andamento (ADR-0014 §4).
func TestStartCongelaOFluxo(t *testing.T) {
	repo, flows, _, svc := setup(t)
	ctx := comConta("acc-1")

	d, err := svc.Start(ctx, projeto, "SUOPT-1315", "idem-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d.Flow.Version != 1 || len(d.Stages) != 3 {
		t.Fatalf("congelou errado: v%d com %d etapas", d.Flow.Version, len(d.Stages))
	}

	// O fluxo do catálogo muda — versão nova, etapas diferentes.
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
	if len(recarregada.Stages) != 3 || recarregada.Stages[0].Key != "contexto" {
		t.Errorf("etapas reescritas pelo fluxo novo: %+v", chaves(recarregada.Stages))
	}
	if recarregada.Flow.FrozenAt.IsZero() {
		t.Error("snapshot sem instante de congelamento")
	}

	// Reiniciar a MESMA chave externa devolve a demanda como está: re-resolver
	// seria congelar de novo, que é exatamente o que o congelamento impede.
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
		t.Errorf("eventos emitidos = %v, esperado só %s", got, demand.EventStarted)
	}
}

func TestStartRecusaFluxoQuebrado(t *testing.T) {
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
		t.Errorf("chave de etapa repetida deveria ser recusada, veio %v", err)
	}
}

// ── a máquina de etapas ──────────────────────────────────────────────────────

func TestTransicaoDeEtapaInvalidaERecusada(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	// pending → done pula o início: a etapa nunca teria começado.
	_, err := svc.AdvanceStage(ctx, d.ID, "contexto", demand.StageDone, "")
	exigeInvalido(t, err, "contexto")

	// Etapa fora de ordem: começar a 2ª com a 1ª pendente esconderia trabalho
	// pulado atrás de um progresso que parece legítimo.
	_, err = svc.AdvanceStage(ctx, d.ID, "spec", demand.StageRunning, "")
	exigeInvalido(t, err, "contexto")

	// Etapa que não existe no fluxo congelado.
	_, err = svc.AdvanceStage(ctx, d.ID, "inexistente", demand.StageRunning, "")
	exigeInvalido(t, err, "inexistente")

	// Status desconhecido (o UNSPECIFIED do contrato chega assim).
	_, err = svc.AdvanceStage(ctx, d.ID, "contexto", demand.StageStatus(""), "")
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("status vazio deveria ser recusado, veio %v", err)
	}

	// O caminho legítimo continua funcionando, e concluído é terminal.
	avancar(t, svc, ctx, d.ID, "contexto", demand.StageRunning)
	avancar(t, svc, ctx, d.ID, "contexto", demand.StageDone)
	_, err = svc.AdvanceStage(ctx, d.ID, "contexto", demand.StageRunning, "")
	exigeInvalido(t, err, "contexto")
}

// O TIPO da etapa dirige a máquina: portão humano não fecha sozinho.
func TestPortaoHumanoNaoFechaPorAdvance(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	avancar(t, svc, ctx, d.ID, "contexto", demand.StageRunning)
	avancar(t, svc, ctx, d.ID, "contexto", demand.StageDone)
	avancar(t, svc, ctx, d.ID, "spec", demand.StageRunning)
	avancar(t, svc, ctx, d.ID, "spec", demand.StageDone)
	avancar(t, svc, ctx, d.ID, "validacao", demand.StageRunning)

	_, err := svc.AdvanceStage(ctx, d.ID, "validacao", demand.StageDone, "")
	exigeInvalido(t, err, "validacao")
	if !strings.Contains(err.Error(), "DecideGate") {
		t.Errorf("a recusa deveria apontar o caminho certo: %v", err)
	}

	// Reprovar não zera a etapa: bloqueia com o comentário, para que a
	// reprovação continue visível.
	st, err := svc.DecideGate(ctx, d.ID, "validacao", false, "faltou critério de aceite", "")
	if err != nil {
		t.Fatalf("DecideGate reprovando: %v", err)
	}
	if st.Status != demand.StageBlocked || st.GateComment == "" {
		t.Errorf("reprovação deveria bloquear com comentário, veio %s/%q", st.Status, st.GateComment)
	}

	st, err = svc.DecideGate(ctx, d.ID, "validacao", true, "ok", "")
	if err != nil {
		t.Fatalf("DecideGate aprovando: %v", err)
	}
	if st.Status != demand.StageDone || st.FinishedAt == nil {
		t.Errorf("aprovação deveria concluir a etapa, veio %s", st.Status)
	}

	// Com todas as etapas concluídas, o status projetado da demanda fecha.
	final, err := svc.Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != demand.StatusDone {
		t.Errorf("status projetado = %s, esperado %s", final.Status, demand.StatusDone)
	}
}

func TestDecideGateSoOndeHaPortao(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	// Etapa sem portão não tem o que decidir.
	_, err := svc.DecideGate(ctx, d.ID, "contexto", true, "", "")
	exigeInvalido(t, err, "contexto")

	// Etapa com portão que ainda nem começou.
	_, err = svc.DecideGate(ctx, d.ID, "validacao", true, "", "")
	exigeInvalido(t, err, "validacao")

	// Agente aprovando o próprio trabalho: é o que o portão existe para impedir.
	comoAgente := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: "acc-1", ActorID: "thread-9", ActorKind: ctxutil.ActorAgent})
	if _, err := svc.DecideGate(comoAgente, d.ID, "validacao", true, "", ""); errs.KindOf(err) != errs.KindPermission {
		t.Errorf("agente não decide portão humano, veio %v", err)
	}
}

// ── threads e achados ────────────────────────────────────────────────────────

// A regra que a spec põe em uma frase: a thread não morre em silêncio.
func TestThreadNaoConcluiSemAchadoPublicado(t *testing.T) {
	repo, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	th, err := svc.CreateThread(ctx, d.ID, "forense-db", demand.AgentCard{
		Purpose: "análise forense do banco", Model: "opus", Effort: "high",
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

	// Sem achado publicado, concluir é recusado — e a recusa diz por quê.
	err = nil
	if _, err = svc.ConcludeThread(ctx, th.ID, ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("concluir sem achado deveria falhar com precondição, veio %v", err)
	}
	if !strings.Contains(err.Error(), "achado") {
		t.Errorf("a recusa deveria falar do achado: %v", err)
	}

	f, err := svc.PublishFinding(ctx, d.ID, th.ID, "deadlock na tabela X",
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

	// Depois do achado, o registro durável é o achado: a conversa não volta.
	if _, err := svc.PostMessage(ctx, th.ID, "mais uma coisa", ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("thread concluída não recebe mensagem, veio %v", err)
	}

	// Toda ação virou evento (ADR-0006) — ação sem evento é bug.
	esperados := []string{
		demand.EventStarted, demand.EventThreadCreated, demand.EventMessagePosted,
		demand.EventFindingPublished, demand.EventThreadConcluded,
	}
	if got := repo.tipos(); !mesmaLista(got, esperados) {
		t.Errorf("eventos = %v, esperado %v", got, esperados)
	}
}

func TestAchadoDeOutraDemandaERecusado(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	a := iniciar(t, svc, ctx, "SUOPT-1")
	b := iniciar(t, svc, ctx, "SUOPT-2")

	th, err := svc.CreateThread(ctx, a.ID, "principal", demand.AgentCard{Purpose: "implementar"}, "")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// A demanda é a fronteira de segurança (ADR-0010 §6).
	if _, err := svc.PublishFinding(ctx, b.ID, th.ID, "achado", nil, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("achado cruzando demanda deveria ser recusado, veio %v", err)
	}
}

func TestFichaDoSubagenteEObrigatoria(t *testing.T) {
	_, _, _, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	// Subagente sem propósito é caixa-preta — o que a ADR-0010 recusa.
	if _, err := svc.CreateThread(ctx, d.ID, "logs", demand.AgentCard{}, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("ficha sem propósito deveria ser recusada, veio %v", err)
	}
	if _, err := svc.CreateThread(ctx, d.ID, "Logs Do Servidor",
		demand.AgentCard{Purpose: "ler logs"}, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("chave de thread inválida deveria ser recusada, veio %v", err)
	}
	if _, err := svc.CreateThread(ctx, d.ID, "logs",
		demand.AgentCard{Purpose: "ler logs", Effort: "turbo"}, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("esforço fora do cardápio deveria ser recusado, veio %v", err)
	}
}

// Bloquear é o que alimenta a caixa de atenção.
func TestThreadBloqueadaEntraNaCaixaDeAtencao(t *testing.T) {
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

func TestConsultaSemContaEInvalida(t *testing.T) {
	_, _, _, svc := setup(t)
	if _, err := svc.Get(context.Background(), "qualquer"); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("requisição sem conta ativa deveria ser inválida, veio %v", err)
	}
}

func TestDemandaDeOutraContaNaoAparece(t *testing.T) {
	_, _, _, svc := setup(t)
	d := iniciar(t, svc, comConta("acc-1"), "SUOPT-1315")

	if _, err := svc.Get(comConta("acc-2"), d.ID); errs.KindOf(err) != errs.KindNotFound {
		t.Error("demanda de outra conta deveria ser invisível")
	}
}

// WatchDemand entrega os eventos DESTA demanda — o recorte por id é daqui,
// porque o filtro do barramento é por agregado e tipo.
func TestWatchEntregaSoOsEventosDaDemanda(t *testing.T) {
	_, _, watcher, svc := setup(t)
	ctx := comConta("acc-1")
	d := iniciar(t, svc, ctx, "SUOPT-1315")

	var vistos []string
	err := watcher.entregar(ctx, svc, d.ID, []ports.Event{
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
		t.Errorf("eventos entregues = %v, esperado [e1 e3]", vistos)
	}
}

func TestConstrutorRecusaDependenciaNula(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("relógio nulo deveria explodir na montagem, não em produção")
		}
	}()
	demand.NewService(newFakeRepo(), &fakeFlows{}, &fakeWatcher{}, nil)
}

// ── duplos em memória ────────────────────────────────────────────────────────

const projeto = "proj-1"

// relogioFixo é o duplo do Clock: sem ele, StartedAt e FinishedAt dependeriam
// do relógio de parede e nada seria verificável por igualdade.
type relogioFixo struct{ t time.Time }

func (r relogioFixo) Now() time.Time { return r.t }

type fakeFlows struct {
	flow     demand.Flow
	chamadas int
}

func (f *fakeFlows) Resolve(_ context.Context, _, _, _ string) (demand.Flow, error) {
	f.chamadas++
	return f.flow, nil
}

// fakeWatcher guarda o filtro pedido e entrega os eventos que lhe derem.
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

// entregar roda o Watch do serviço com uma fila preparada.
func (w *fakeWatcher) entregar(ctx context.Context, svc *demand.Service, demandID string, fila []ports.Event, emit func(ports.Event) error) error {
	w.fila = fila
	return svc.Watch(ctx, demandID, emit)
}

type fakeRepo struct {
	demands  map[string]*demand.Demand
	threads  map[string]*demand.Thread
	findings []demand.Finding
	// eventos é o que prova a regra da ADR-0006: nenhuma escrita passa por
	// aqui sem trazer o evento junto.
	eventos []demand.Emission
	seq     int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{demands: map[string]*demand.Demand{}, threads: map[string]*demand.Thread{}}
}

func (f *fakeRepo) id(prefixo string) string {
	f.seq++
	return prefixo + "-" + string(rune('a'+f.seq))
}

func (f *fakeRepo) tipos() []string {
	out := make([]string, 0, len(f.eventos))
	for _, e := range f.eventos {
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
	f.eventos = append(f.eventos, ev)
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
			f.eventos = append(f.eventos, ev)
			saved := st
			return &saved, nil
		}
	}
	return nil, errs.NotFound("etapa")
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
	f.eventos = append(f.eventos, ev)
	return &saved, nil
}

func (f *fakeRepo) SaveThreadState(_ context.Context, accountID, threadID string, state demand.ThreadState, ev demand.Emission, _ string) (*demand.Thread, error) {
	t, ok := f.threads[threadID]
	if !ok || t.AccountID != accountID {
		return nil, errs.NotFound("thread")
	}
	t.State = state
	f.eventos = append(f.eventos, ev)
	copia := *t
	return &copia, nil
}

func (f *fakeRepo) AppendMessage(_ context.Context, m *demand.Message, ev demand.Emission, _ string) (*demand.Message, error) {
	saved := *m
	saved.ID = f.id("msg")
	if t, ok := f.threads[m.ThreadID]; ok && t.State == demand.ThreadOpen {
		t.State = demand.ThreadActive
	}
	f.eventos = append(f.eventos, ev)
	return &saved, nil
}

func (f *fakeRepo) CreateFinding(_ context.Context, fd *demand.Finding, ev demand.Emission, _ string) (*demand.Finding, error) {
	saved := *fd
	saved.ID = f.id("fnd")
	f.findings = append(f.findings, saved)
	f.eventos = append(f.eventos, ev)
	return &saved, nil
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
// contexto → spec → validação humana.
func fluxoPadrao() demand.Flow {
	return demand.Flow{
		ID: "flow-1", Name: "fluxo padrão", Version: 1, ResolvedFrom: "projeto ◂ conta",
		Stages: []demand.StageSpec{
			{Key: "contexto", Name: "Contexto", Type: demand.TypeContext},
			{Key: "spec", Name: "Spec", Type: demand.TypeSpec, Artifacts: []demand.ArtifactKind{demand.ArtifactSpec}},
			{Key: "validacao", Name: "Validação", Type: demand.TypeHumanValidation, Gate: demand.GateHuman},
		},
	}
}

func setup(t *testing.T) (*fakeRepo, *fakeFlows, *fakeWatcher, *demand.Service) {
	t.Helper()
	repo := newFakeRepo()
	flows := &fakeFlows{flow: fluxoPadrao()}
	watcher := &fakeWatcher{}
	svc := demand.NewService(repo, flows, watcher,
		relogioFixo{time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)})
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

// exigeInvalido cobra o essencial da recusa: erro de argumento inválido que
// DIZ qual etapa. Mensagem genérica aqui vira ticket de suporte depois.
func exigeInvalido(t *testing.T, err error, etapa string) {
	t.Helper()
	if errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("esperava recusa por argumento inválido, veio %v", err)
	}
	if !strings.Contains(err.Error(), etapa) {
		t.Errorf("a recusa não diz qual etapa (%q): %v", etapa, err)
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
