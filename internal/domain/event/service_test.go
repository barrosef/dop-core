package event_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM Postgres e SEM NATS: repositório e barramento são
// portas, e aqui entram duplos em memória. É o retorno prático da arquitetura
// hexagonal — inclusive para a parte difícil, que é a emenda replay→ao vivo.

var t0 = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

// ── duplo do barramento ──────────────────────────────────────────────────────

type fakeBus struct {
	mu sync.Mutex
	h  ports.Handler
}

func (b *fakeBus) Publish(context.Context, ports.Event) error { return nil }
func (b *fakeBus) Close() error                               { return nil }

func (b *fakeBus) Subscribe(_ context.Context, _, _ string, _ []string, h ports.Handler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.h = h
	return nil
}

// aoVivo entrega um evento como o broker entregaria.
func (b *fakeBus) aoVivo(e ports.Event) {
	b.mu.Lock()
	h := b.h
	b.mu.Unlock()
	if h != nil {
		_ = h(context.Background(), e)
	}
}

// ── duplo do repositório ─────────────────────────────────────────────────────

type fakeRepo struct {
	log []ports.Event // em ordem de acontecimento
	// duranteLeitura roda DENTRO da leitura do banco: é como o teste coloca um
	// evento no ar exatamente na janela entre assinar e ler.
	duranteLeitura func()
}

func (r *fakeRepo) Locate(_ context.Context, accountID, eventID string) (event.Cursor, error) {
	for _, e := range r.log {
		if e.ID == eventID && e.AccountID == accountID {
			return event.Cursor{OccurredAt: e.OccurredAt, ID: e.ID}, nil
		}
	}
	return event.Cursor{}, errs.NotFound("evento do cursor")
}

func (r *fakeRepo) EventsAfter(_ context.Context, accountID string, after event.Cursor, f event.Filter, limit int) ([]ports.Event, error) {
	if fn := r.duranteLeitura; fn != nil {
		r.duranteLeitura = nil
		fn()
	}
	out := make([]ports.Event, 0, limit)
	for _, e := range r.log {
		if e.AccountID != accountID || !e.OccurredAt.After(after.OccurredAt) || !f.Matches(e) {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ── arreio ───────────────────────────────────────────────────────────────────

func ev(id, conta, agregado, tipo string, minuto int) ports.Event {
	return ports.Event{
		ID: id, AccountID: conta, Aggregate: agregado, AggregateID: "ag-" + id,
		Type: tipo, Payload: []byte(`{}`), OccurredAt: t0.Add(time.Duration(minuto) * time.Minute),
	}
}

func novoServico(t *testing.T, repo *fakeRepo) (*event.Service, *fakeBus) {
	t.Helper()
	bus := &fakeBus{}
	svc := event.NewService(repo, bus, fakeClock{t0})
	if err := svc.Start(context.Background(), "teste", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return svc, bus
}

func ctxDaConta(conta string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		RequestID: "teste", AccountID: conta, ActorID: "ator", ActorKind: ctxutil.ActorUser,
	})
}

// coletor entrega um Emitter que empilha o que chegou e avisa em um canal.
type coletor struct {
	mu       sync.Mutex
	recebido []ports.Event
	sinal    chan struct{}
}

func novoColetor() *coletor { return &coletor{sinal: make(chan struct{}, 1024)} }

func (c *coletor) emit(e ports.Event) error {
	c.mu.Lock()
	c.recebido = append(c.recebido, e)
	c.mu.Unlock()
	select {
	case c.sinal <- struct{}{}:
	default:
	}
	return nil
}

// ids devolve o que chegou, MENOS as sondas (ver aguardarAssinante).
func (c *coletor) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.recebido))
	for _, e := range c.recebido {
		if e.Aggregate != agregadoSonda {
			out = append(out, e.ID)
		}
	}
	return out
}

func (c *coletor) recebeuSonda() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.recebido {
		if e.Aggregate == agregadoSonda {
			return true
		}
	}
	return false
}

// agregadoSonda marca eventos que existem só para sincronizar o teste.
const agregadoSonda = "__sonda"

// aguardarAssinante espera o Watch entrar no fan-out.
//
// Watch registra o assinante e SÓ ENTÃO entra no laço de entrega; evento
// publicado antes disso cai no vazio. Dormir um tempo arbitrário aqui seria uma
// corrida disfarçada — então o teste bate na porta com sondas até uma voltar.
// As asserções descartam sondas, por isso republicar é inofensivo.
func aguardarAssinante(t *testing.T, bus *fakeBus, conta string, chegou func() bool) {
	t.Helper()
	prazo := time.After(5 * time.Second)
	for i := 0; !chegou(); i++ {
		bus.aoVivo(ev(fmt.Sprintf("sonda-%d", i), conta, agregadoSonda, "dop.teste.sonda", 1))
		select {
		case <-prazo:
			t.Fatal("o assinante nunca entrou no fan-out")
		case <-time.After(time.Millisecond):
		}
	}
}

// esperar bloqueia até o coletor ter n eventos, ou falha por prazo.
func (c *coletor) esperar(t *testing.T, n int) {
	t.Helper()
	prazo := time.After(3 * time.Second)
	for {
		total := len(c.ids())
		if total >= n {
			return
		}
		select {
		case <-c.sinal:
		case <-prazo:
			t.Fatalf("esperava %d eventos, chegaram %d: %v", n, total, c.ids())
		}
	}
}

func iguais(a, b []string) bool {
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

// ── A EMENDA ─────────────────────────────────────────────────────────────────

// A garantia central: entre o replay e o fluxo ao vivo não há buraco NEM
// duplicata.
//
// O cenário é exatamente o perigoso: E2 já estava commitado quando o cliente
// pediu o replay, mas só foi publicado no barramento DEPOIS de assinarmos e
// DURANTE a leitura do banco. Ele aparece nas duas fontes. Tem que sair uma vez.
func TestEmendaReplayAoVivoSemPerdaNemDuplicata(t *testing.T) {
	e0 := ev("e0", "conta-a", "project", "dop.hierarchy.project.created", 1)
	e1 := ev("e1", "conta-a", "project", "dop.hierarchy.project.created", 2)
	e2 := ev("e2", "conta-a", "demand", "dop.demand.created", 3)
	e3 := ev("e3", "conta-a", "demand", "dop.demand.stage.advanced", 4)

	repo := &fakeRepo{log: []ports.Event{e0, e1, e2}}
	svc, bus := novoServico(t, repo)

	// A janela: publica E2 enquanto o replay lê o banco.
	repo.duranteLeitura = func() { bus.aoVivo(e2) }

	col := novoColetor()
	ctx, cancel := context.WithCancel(ctxDaConta("conta-a"))
	defer cancel()

	fim := make(chan error, 1)
	go func() { fim <- svc.Watch(ctx, "e0", event.Filter{}, col.emit) }()

	col.esperar(t, 2) // replay: e1, e2

	// Agora o ao vivo de verdade, depois da emenda.
	bus.aoVivo(e3)
	col.esperar(t, 3)

	cancel()
	if err := <-fim; err != nil {
		t.Fatalf("Watch devolveu erro: %v", err)
	}

	esperado := []string{"e1", "e2", "e3"}
	if got := col.ids(); !iguais(got, esperado) {
		t.Errorf("emenda quebrada: recebido %v, esperado %v", got, esperado)
	}
}

// Sem cursor, nada de histórico: só o que vier daqui pra frente.
func TestSemCursorNaoHaReplay(t *testing.T) {
	e1 := ev("e1", "conta-a", "project", "dop.hierarchy.project.created", 1)
	e2 := ev("e2", "conta-a", "project", "dop.hierarchy.project.updated", 2)

	repo := &fakeRepo{log: []ports.Event{e1}}
	svc, bus := novoServico(t, repo)

	col := novoColetor()
	ctx, cancel := context.WithCancel(ctxDaConta("conta-a"))
	defer cancel()
	fim := make(chan error, 1)
	go func() { fim <- svc.Watch(ctx, "", event.Filter{}, col.emit) }()
	aguardarAssinante(t, bus, "conta-a", col.recebeuSonda)

	bus.aoVivo(e2)
	col.esperar(t, 1)
	cancel()
	<-fim

	if got := col.ids(); !iguais(got, []string{"e2"}) {
		t.Errorf("sem cursor o histórico vazou: %v", got)
	}
}

// ── ISOLAMENTO POR CONTA ─────────────────────────────────────────────────────

// Assinante da conta A jamais recebe evento da conta B — nem no replay, nem ao
// vivo. E evento SEM conta (migração 0003: `identity.user.ensured` acontece
// antes de a conta pessoal existir) não pertence a ninguém: não vaza para
// assinante nenhum.
func TestIsolamentoPorConta(t *testing.T) {
	a0 := ev("a0", "conta-a", "account", "dop.identity.account.created", 1)
	a1 := ev("a1", "conta-a", "project", "dop.hierarchy.project.created", 2)
	b1 := ev("b1", "conta-b", "project", "dop.hierarchy.project.created", 3)
	orfao := ev("orfao", "", "user", "dop.identity.user.ensured", 4)
	a2 := ev("a2", "conta-a", "demand", "dop.demand.created", 5)

	// O log tem eventos das DUAS contas e o órfão: o replay precisa recortar.
	repo := &fakeRepo{log: []ports.Event{a0, a1, b1, orfao, a2}}
	svc, bus := novoServico(t, repo)

	col := novoColetor()
	ctx, cancel := context.WithCancel(ctxDaConta("conta-a"))
	defer cancel()
	fim := make(chan error, 1)
	go func() { fim <- svc.Watch(ctx, "a0", event.Filter{}, col.emit) }()

	col.esperar(t, 2) // replay da conta A: a1, a2

	// Ao vivo: só o de A pode passar.
	bus.aoVivo(b1)
	bus.aoVivo(orfao)
	a3 := ev("a3", "conta-a", "demand", "dop.demand.stage.advanced", 6)
	bus.aoVivo(a3)
	col.esperar(t, 3)

	cancel()
	<-fim

	got := col.ids()
	if !iguais(got, []string{"a1", "a2", "a3"}) {
		t.Fatalf("ISOLAMENTO VIOLADO: assinante da conta A recebeu %v", got)
	}
}

// O cursor também é superfície de ataque: id de evento de outra conta não pode
// virar posição válida — nem confirmar que o id existe.
func TestCursorDeOutraContaNaoResolve(t *testing.T) {
	b1 := ev("b1", "conta-b", "project", "dop.hierarchy.project.created", 1)
	repo := &fakeRepo{log: []ports.Event{b1}}
	svc, _ := novoServico(t, repo)

	err := svc.Watch(ctxDaConta("conta-a"), "b1", event.Filter{}, func(ports.Event) error { return nil })
	if errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("esperava not_found para cursor de outra conta, veio %v", err)
	}
}

// Requisição sem conta ativa é inválida por definição — igual ao resto do
// domínio (ctxutil.MustAccount).
func TestWatchSemContaAtivaERecusado(t *testing.T) {
	svc, _ := novoServico(t, &fakeRepo{})
	err := svc.Watch(context.Background(), "", event.Filter{}, func(ports.Event) error { return nil })
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("esperava invalid_argument sem conta ativa, veio %v", err)
	}
}

// ── FILTRO ───────────────────────────────────────────────────────────────────

// O filtro tem que valer igual nos dois caminhos: se o replay recortasse de um
// jeito e o ao vivo de outro, o cliente veria um evento e não veria o irmão.
func TestFiltroValeNoReplayENoAoVivo(t *testing.T) {
	e0 := ev("e0", "conta-a", "project", "dop.hierarchy.project.created", 1)
	proj := ev("proj", "conta-a", "project", "dop.hierarchy.project.updated", 2)
	dem := ev("dem", "conta-a", "demand", "dop.demand.created", 3)

	repo := &fakeRepo{log: []ports.Event{e0, proj, dem}}
	svc, bus := novoServico(t, repo)

	col := novoColetor()
	ctx, cancel := context.WithCancel(ctxDaConta("conta-a"))
	defer cancel()
	f := event.Filter{Aggregates: []string{"project"}}
	fim := make(chan error, 1)
	go func() { fim <- svc.Watch(ctx, "e0", f, col.emit) }()

	col.esperar(t, 1) // replay: só o de project

	bus.aoVivo(ev("dem2", "conta-a", "demand", "dop.demand.created", 4))
	bus.aoVivo(ev("proj2", "conta-a", "project", "dop.hierarchy.project.updated", 5))
	col.esperar(t, 2)

	cancel()
	<-fim

	if got := col.ids(); !iguais(got, []string{"proj", "proj2"}) {
		t.Errorf("filtro divergiu entre replay e ao vivo: %v", got)
	}
}

func TestFiltroPorTipo(t *testing.T) {
	f := event.Filter{Types: []string{"dop.demand.created"}}
	if !f.Matches(ev("x", "c", "demand", "dop.demand.created", 1)) {
		t.Error("tipo listado deveria casar")
	}
	if f.Matches(ev("x", "c", "demand", "dop.demand.stage.advanced", 1)) {
		t.Error("tipo fora da lista não deveria casar")
	}
	if !(event.Filter{}).Matches(ev("x", "c", "qualquer", "dop.qualquer", 1)) {
		t.Error("filtro vazio deveria aceitar tudo")
	}
}

// ── CONSUMIDOR LENTO E CANCELAMENTO ──────────────────────────────────────────

// Consumidor lento não trava o servidor: a fila dele estoura e ELE cai, com
// erro claro para reconectar. O barramento nunca fica esperando.
func TestConsumidorLentoCaiEmVezDeTravar(t *testing.T) {
	svc, bus := novoServico(t, &fakeRepo{})

	solta := make(chan struct{})
	var travou atomic.Bool
	lento := func(ports.Event) error {
		travou.Store(true)
		<-solta // trava na primeira entrega e não sai mais
		return nil
	}

	fim := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctxDaConta("conta-a"))
	defer cancel()
	go func() { fim <- svc.Watch(ctx, "", event.Filter{}, lento) }()
	// A primeira sonda entregue é a que trava o assinante: `lento` só volta
	// quando o teste soltar.
	aguardarAssinante(t, bus, "conta-a", travou.Load)

	// Empurra muito mais do que cabe na fila. Se o fan-out bloqueasse, este
	// laço nunca terminaria — o teste estouraria por timeout.
	pronto := make(chan struct{})
	go func() {
		for i := 0; i < 4096; i++ {
			bus.aoVivo(ev("x", "conta-a", "demand", "dop.demand.created", i+1))
		}
		close(pronto)
	}()

	select {
	case <-pronto:
	case <-time.After(5 * time.Second):
		t.Fatal("o fan-out travou por causa de um assinante lento")
	}

	close(solta)
	select {
	case err := <-fim:
		if errs.KindOf(err) != errs.KindUnavailable {
			t.Errorf("esperava unavailable para assinante lento, veio %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("o assinante lento não foi desconectado")
	}
}

// Cliente desconecta ⇒ a assinatura morre junto. Watch retorna sem erro: ir
// embora é comportamento normal, não falha.
func TestCancelamentoEncerraAAssinatura(t *testing.T) {
	svc, bus := novoServico(t, &fakeRepo{})

	ctx, cancel := context.WithCancel(ctxDaConta("conta-a"))
	fim := make(chan error, 1)
	var entregou atomic.Bool
	go func() {
		fim <- svc.Watch(ctx, "", event.Filter{}, func(ports.Event) error {
			entregou.Store(true)
			return nil
		})
	}()
	aguardarAssinante(t, bus, "conta-a", entregou.Load)
	cancel()

	select {
	case err := <-fim:
		if err != nil {
			t.Errorf("desconexão do cliente não é erro, veio %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch não respeitou o cancelamento — goroutine vazando")
	}
}

// Erro do Emitter (Send falhou: cliente sumiu) encerra o fluxo.
func TestErroDeEntregaEncerraOFluxo(t *testing.T) {
	svc, bus := novoServico(t, &fakeRepo{})
	ctx, cancel := context.WithCancel(ctxDaConta("conta-a"))
	defer cancel()

	quebrado := errs.Internal("cliente sumiu")
	var entregou atomic.Bool
	fim := make(chan error, 1)
	go func() {
		fim <- svc.Watch(ctx, "", event.Filter{}, func(ports.Event) error {
			entregou.Store(true)
			return quebrado
		})
	}()
	aguardarAssinante(t, bus, "conta-a", entregou.Load)

	select {
	case err := <-fim:
		if err != quebrado {
			t.Errorf("esperava o erro do Emitter de volta, veio %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("erro de entrega não encerrou o fluxo")
	}
}

// Sem Start não há assinatura: falhar explicitamente é melhor do que entregar
// um fluxo mudo que o operador levaria horas para diagnosticar.
func TestWatchAntesDeStartERecusado(t *testing.T) {
	svc := event.NewService(&fakeRepo{}, &fakeBus{}, fakeClock{t0})
	err := svc.Watch(ctxDaConta("conta-a"), "", event.Filter{}, func(ports.Event) error { return nil })
	if errs.KindOf(err) != errs.KindUnavailable {
		t.Errorf("esperava unavailable antes de Start, veio %v", err)
	}
}

// O tail ao vivo é tail: o que aconteceu ANTES de ele existir é assunto do
// replay pelo Postgres. A porta entrega ao durável recém-criado tudo o que está
// retido (garantia 6); sem este corte, todo start do processo despejaria esse
// histórico em cima de quem estivesse ouvindo.
func TestEventoAnteriorAoStartNaoEntraNoAoVivo(t *testing.T) {
	svc, bus := novoServico(t, &fakeRepo{})
	col := novoColetor()
	ctx, cancel := context.WithCancel(ctxDaConta("conta-a"))
	defer cancel()
	fim := make(chan error, 1)
	go func() { fim <- svc.Watch(ctx, "", event.Filter{}, col.emit) }()
	aguardarAssinante(t, bus, "conta-a", col.recebeuSonda)

	antigo := ev("antigo", "conta-a", "demand", "dop.demand.created", 0)
	antigo.OccurredAt = t0.Add(-time.Hour)
	bus.aoVivo(antigo)
	bus.aoVivo(ev("novo", "conta-a", "demand", "dop.demand.created", 1))
	col.esperar(t, 1)

	cancel()
	<-fim

	if got := col.ids(); !iguais(got, []string{"novo"}) {
		t.Errorf("o tail entregou o passado do broker: %v", got)
	}
}
