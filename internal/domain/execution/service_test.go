package execution_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/execution"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM cluster, SEM Docker e SEM banco: repositório,
// launcher, acesso e relógio são portas, e aqui entram duplos em memória. É o
// retorno prático da arquitetura hexagonal.
//
// Os duplos moram NESTE arquivo, e não em internal/adapter: o teste de
// arquitetura varre todo .go sob internal/domain, inclusive os _test.go, e
// importar adaptador daqui quebraria a fronteira que ele protege. Eles
// satisfazem as MESMAS portas — o mesmo contrato que o k8s e o Docker cumprem.

// ── o modelo: suspender ≠ destruir ───────────────────────────────────────────

// TestSuspenderNaoEDestruir é o teste central deste domínio. As duas operações
// param a execução; só uma delas leva o trabalho junto. Se um dia alguém
// "simplificar" as duas em uma, é aqui que o build quebra.
func TestSuspenderNaoEDestruir(t *testing.T) {
	sus, des := execution.SuspendTransition, execution.DestroyTransition

	if !sus.StopsRuntime || !des.StopsRuntime {
		t.Error("as duas param a execução — é o que as faz parecerem iguais de fora")
	}
	if !sus.PreservesWork() {
		t.Error("suspender é ECONOMIA: o workspace sobrevive")
	}
	if des.PreservesWork() {
		t.Error("destruir leva o workspace junto — é o que o torna irreversível")
	}
	if !sus.Reversible || des.Reversible {
		t.Error("suspender volta; destruir não")
	}
	if !execution.StateDestroyed.IsTerminal() {
		t.Error("destruído é absorvente")
	}
	// De destruído não sai NADA — nem para ativo, nem para suspenso.
	for _, tr := range []execution.Transition{sus, des, execution.ResumeTransition} {
		if execution.CanApply(execution.StateDestroyed, tr) {
			t.Errorf("destruído aceitou transição para %q", tr.To)
		}
	}
	if !execution.CanApply(execution.StateActive, sus) {
		t.Error("ativo deveria suspender")
	}
	if !execution.CanApply(execution.StateSuspended, execution.ResumeTransition) {
		t.Error("suspenso deveria retomar")
	}
	if execution.CanApply(execution.StateSuspended, sus) {
		t.Error("suspender o já suspenso não é transição — é repetição")
	}
}

// ── isolationTier: declarado, nunca presumido ────────────────────────────────

func TestTierNaoDeclaradoERecusado(t *testing.T) {
	f := novoCenario(t)

	_, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierUnspecified, "")
	if errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("tier ausente precisa ser recusado, veio: %v", err)
	}
	if !strings.Contains(err.Error(), "não escolhe por você") {
		t.Errorf("a mensagem precisa dizer que o substrato não escolhe: %q", err)
	}
	// E, sobretudo: nada foi gravado e nada foi lançado.
	if n := f.repo.total(); n != 0 {
		t.Errorf("recusa gravou %d sandbox(es)", n)
	}
	if f.launcher.launches != 0 {
		t.Errorf("recusa chamou o substrato %d vez(es)", f.launcher.launches)
	}
}

func TestTierDesconhecidoERecusado(t *testing.T) {
	f := novoCenario(t)
	if _, err := f.svc.Provision(f.ctx, "demanda-1", "microvm-turbinada", ""); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("tier fora do vocabulário precisa ser recusado, veio: %v", err)
	}
}

// TestTierNaoOferecidoRecusaSemGravar é o R-4 da spec: RuntimeClass de Kata
// falta na maioria das distribuições, e a resposta é recusa com mensagem.
func TestTierNaoOferecidoRecusaSemGravar(t *testing.T) {
	f := novoCenario(t)
	f.launcher.tiers = []ports.IsolationTier{ports.TierNamespace}

	_, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierHardware, "")
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("esperava recusa por precondição, veio: %v", err)
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("a mensagem precisa dizer o que ESTÁ disponível: %q", err)
	}
	if n := f.repo.total(); n != 0 {
		t.Errorf("recusa gravou %d sandbox(es) — recusa que provisiona metade é pior que recusa nenhuma", n)
	}
}

// TestSubstratoQueDegradaEDescartado: se o launcher entregar um nível diferente
// do declarado, o sandbox é DESTRUÍDO. Aceitá-lo seria transformar a garantia
// da porta em recomendação.
func TestSubstratoQueDegradaEDescartado(t *testing.T) {
	f := novoCenario(t)
	f.launcher.tiers = []ports.IsolationTier{ports.TierHardware, ports.TierNamespace}
	f.launcher.entrega = ports.TierNamespace // pedimos hardware, ele dá namespace

	_, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierHardware, "")
	if err == nil {
		t.Fatal("degradação silenciosa foi aceita")
	}
	if f.launcher.destroys == 0 {
		t.Error("o sandbox degradado ficou de pé, faturando")
	}
	sb := f.repo.only(t)
	if sb.State != execution.StateDestroyed {
		t.Errorf("a linha ficou em %q; deveria constar destruída", sb.State)
	}
}

func TestTierEntregueEGravado(t *testing.T) {
	f := novoCenario(t)
	sb, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if sb.Tier != ports.TierNamespace {
		t.Errorf("o cliente precisa ver o que RECEBEU: %q", sb.Tier)
	}
	if sb.State != execution.StateActive {
		t.Errorf("estado após provisionar: %q", sb.State)
	}
	if sb.Namespace != execution.NamespaceFor("demanda-1") {
		t.Errorf("namespace: %q", sb.Namespace)
	}
}

// ── idempotência e unicidade ─────────────────────────────────────────────────

func TestChaveDeIdempotenciaNaoDuplicaSandbox(t *testing.T) {
	f := novoCenario(t)
	a, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, "chave-1")
	if err != nil {
		t.Fatalf("1º Provision: %v", err)
	}
	b, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, "chave-1")
	if err != nil {
		t.Fatalf("2º Provision: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("a repetição criou outro sandbox: %s != %s", a.ID, b.ID)
	}
	if f.launcher.launches != 1 {
		t.Errorf("o substrato foi acionado %d vezes para uma chave só", f.launcher.launches)
	}
}

// TestProvisionamentoInterrompidoERetomado cobre a queda entre as DUAS
// transações do provisionamento: a linha ficou em provisioning e o substrato
// nunca subiu. A repetição precisa terminar o serviço, não devolver ao cliente
// um sandbox pela metade que ninguém mais conserta.
func TestProvisionamentoInterrompidoERetomado(t *testing.T) {
	f := novoCenario(t)
	f.launcher.falhasNoLaunch = 1

	if _, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, ""); err == nil {
		t.Fatal("a primeira tentativa deveria ter falhado")
	}
	meio := f.repo.only(t)
	if meio.State != execution.StateProvisioning {
		t.Fatalf("o rastro da tentativa sumiu: estado %q", meio.State)
	}

	sb, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("a repetição deveria terminar o provisionamento: %v", err)
	}
	if sb.ID != meio.ID {
		t.Errorf("a repetição criou outro sandbox: %s != %s", sb.ID, meio.ID)
	}
	if sb.State != execution.StateActive {
		t.Errorf("estado após retomar o provisionamento: %q", sb.State)
	}
}

func TestUmaDemandaUmSandbox(t *testing.T) {
	f := novoCenario(t)
	a, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	b, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("2º Provision: %v", err)
	}
	if a.ID != b.ID {
		t.Error("a mesma demanda ganhou dois sandboxes")
	}
	// Trocar o isolamento de um sandbox que já existe seria degradar (ou
	// promover) em silêncio o que alguém já declarou.
	if _, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierHardware, ""); errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("esperava recusa ao mudar o tier de um sandbox existente, veio: %v", err)
	}
}

// ── isolamento e permissão ───────────────────────────────────────────────────

func TestDemandaDeOutraContaNaoExiste(t *testing.T) {
	f := novoCenario(t)
	f.demands.dono["demanda-alheia"] = "outra-conta"

	if _, err := f.svc.Provision(f.ctx, "demanda-alheia", ports.TierNamespace, ""); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("demanda de outra conta precisa ser 'não encontrada' — a existência do id não pode vazar: %v", err)
	}
}

func TestViewerNaoProvisiona(t *testing.T) {
	f := novoCenario(t)
	f.access.papel = identity.RoleViewer
	if _, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, ""); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("viewer provisionou: %v", err)
	}
}

func TestRequisicaoSemContaAtiva(t *testing.T) {
	f := novoCenario(t)
	sem := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "u1"})
	if _, err := f.svc.Provision(sem, "demanda-1", ports.TierNamespace, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("requisição sem conta ativa é inválida por definição: %v", err)
	}
}

func TestSandboxDeOutraContaNaoEEncontrado(t *testing.T) {
	f := novoCenario(t)
	sb, err := f.svc.Provision(f.ctx, "demanda-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	outra := ctxutil.Into(context.Background(), ctxutil.Call{AccountID: "conta-b", ActorID: "u2"})
	if _, err := f.svc.Describe(outra, sb.ID); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("sandbox de outra conta vazou: %v", err)
	}
}

// ── ciclo de vida ────────────────────────────────────────────────────────────

func TestSuspenderRepetidoNaoEmiteTransicao(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)

	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("1º Suspend: %v", err)
	}
	antes := f.repo.transitions
	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("2º Suspend deveria ser inócuo: %v", err)
	}
	if f.repo.transitions != antes {
		t.Error("suspender o já suspenso emitiu transição — evento que não mudou nada envenena o dossiê")
	}
}

func TestDestruirEIrreversivel(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)

	ok, err := f.svc.Destroy(f.ctx, sb.ID)
	if err != nil || !ok {
		t.Fatalf("Destroy: %v", err)
	}
	// Repetir é inócuo: quem repete quer o mesmo resultado, e ele já está lá.
	if ok, err := f.svc.Destroy(f.ctx, sb.ID); err != nil || !ok {
		t.Fatalf("2º Destroy: %v (%v)", err, ok)
	}
	_, err = f.svc.Resume(f.ctx, sb.ID)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("destruído retomou: %v", err)
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("a mensagem precisa dizer que o trabalho foi junto: %q", err)
	}
}

func TestRetomarRecriaSobreOWorkspace(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	got, err := f.svc.Resume(f.ctx, sb.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.State != execution.StateActive {
		t.Fatalf("estado após retomar: %q", got.State)
	}
	if f.launcher.resumes != 1 {
		t.Errorf("o substrato foi retomado %d vezes", f.launcher.resumes)
	}
}

// TestRetomadaComTierDiferenteERecusada fecha a porta dos fundos: o sandbox já
// existia, então ninguém reconferiria o isolamento.
func TestRetomadaComTierDiferenteERecusada(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	f.launcher.entrega = ports.TierKernelEmulated
	if _, err := f.svc.Resume(f.ctx, sb.ID); err == nil {
		t.Fatal("retomada degradada foi aceita")
	}
}

func TestDescribeDenunciaDivergenciaComOSubstrato(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	f.launcher.sumiu = true // alguém apagou o namespace por fora

	_, err := f.svc.Describe(f.ctx, sb.ID)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("dizer 'ativo' para um sandbox que não existe é mentir para o cockpit: %v", err)
	}
}

// ── economia ─────────────────────────────────────────────────────────────────

func TestVarreduraSuspendeSomenteOOcioso(t *testing.T) {
	f := novoCenario(t)
	ocioso := f.provisionado(t)
	f.repo.get(ocioso.ID).LastActiveAt = f.clock.Now().Add(-2 * execution.IdleTimeout)

	f.demands.dono["demanda-2"] = "conta-a"
	recente, err := f.svc.Provision(f.ctx, "demanda-2", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	n, err := f.svc.SweepIdle(f.ctx)
	if err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}
	if n != 1 {
		t.Fatalf("suspendeu %d; esperava só o ocioso", n)
	}
	if f.repo.get(ocioso.ID).State != execution.StateSuspended {
		t.Error("o ocioso continuou de pé — é isso que afoga a máquina")
	}
	if f.repo.get(recente.ID).State != execution.StateActive {
		t.Error("o sandbox em uso foi derrubado")
	}
}

func TestShouldSuspendPuro(t *testing.T) {
	agora := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	ativo := execution.Sandbox{State: execution.StateActive, LastActiveAt: agora.Add(-execution.IdleTimeout)}
	if !ativo.ShouldSuspend(agora) {
		t.Error("no limite de ociosidade o sandbox suspende")
	}
	quase := execution.Sandbox{State: execution.StateActive, LastActiveAt: agora.Add(-execution.IdleTimeout + time.Second)}
	if quase.ShouldSuspend(agora) {
		t.Error("um segundo antes do limite ele fica")
	}
	suspenso := execution.Sandbox{State: execution.StateSuspended, LastActiveAt: agora.Add(-time.Hour)}
	if suspenso.ShouldSuspend(agora) {
		t.Error("suspender o já suspenso não economiza nada")
	}
}

// ── logs ─────────────────────────────────────────────────────────────────────

func TestClassificacaoDeLinha(t *testing.T) {
	casos := []struct {
		raw   string
		src   execution.Source
		tt    execution.TestType
		texto string
	}{
		{"[app] subiu na 3000", execution.SourceApp, "", "subiu na 3000"},
		{"[test:e2e] 3 passaram", execution.SourceTest, execution.TestE2E, "3 passaram"},
		{"[infra] docker pronto", execution.SourceInfra, "", "docker pronto"},
		// Sem prefixo é o que o próprio substrato imprimiu.
		{"npm ERR! algo", execution.SourceInfra, "", "npm ERR! algo"},
		// Colchete que não é tag nossa não pode ser comido: a linha vale como veio.
		{"[2026-08-31] backup ok", execution.SourceInfra, "", "[2026-08-31] backup ok"},
	}
	for _, c := range casos {
		src, tt, texto := execution.Classify(c.raw)
		if src != c.src || tt != c.tt || texto != c.texto {
			t.Errorf("Classify(%q) = (%q,%q,%q); esperava (%q,%q,%q)",
				c.raw, src, tt, texto, c.src, c.tt, c.texto)
		}
	}
}

func TestFiltroDeLog(t *testing.T) {
	linha := execution.LogLine{Source: execution.SourceTest, TestType: execution.TestE2E, Service: "backend"}
	if !(execution.LogFilter{}).Matches(linha) {
		t.Error("filtro vazio pede tudo")
	}
	if !(execution.LogFilter{Source: execution.SourceTest}).Matches(linha) {
		t.Error("origem igual deveria casar")
	}
	if (execution.LogFilter{Source: execution.SourceApp}).Matches(linha) {
		t.Error("origem diferente não casa")
	}
	if (execution.LogFilter{TestType: execution.TestAAA}).Matches(linha) {
		t.Error("tipo de teste diferente não casa")
	}
}

func TestStreamLogsFiltraEContaComoAtividade(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	f.launcher.linhas = []ports.LogLine{
		{Text: "[app] subiu"},
		{Text: "[test:e2e] verde"},
		{Text: "docker pronto"},
	}

	var vistas []string
	err := f.svc.StreamLogs(f.ctx, sb.ID, execution.LogFilter{Source: execution.SourceApp},
		func(l execution.LogLine) error {
			vistas = append(vistas, l.Text)
			return nil
		})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if len(vistas) != 1 || vistas[0] != "subiu" {
		t.Fatalf("o filtro não recortou: %v", vistas)
	}
	// Dev conectado é atividade: sem isso o varredor derrubaria o sandbox de
	// quem está justamente olhando para ele.
	if f.repo.touches == 0 {
		t.Error("seguir logs não contou como atividade")
	}
}

func TestStreamLogsRecusaSuspensoEDestruido(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	if _, err := f.svc.Suspend(f.ctx, sb.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	err := f.svc.StreamLogs(f.ctx, sb.ID, execution.LogFilter{}, func(execution.LogLine) error { return nil })
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("suspenso não tem execução; esperava recusa, veio: %v", err)
	}

	if _, err := f.svc.Destroy(f.ctx, sb.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	err = f.svc.StreamLogs(f.ctx, sb.ID, execution.LogFilter{}, func(execution.LogLine) error { return nil })
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("destruído não tem logs; veio: %v", err)
	}
}

func TestErroDoEmitInterrompeOFluxo(t *testing.T) {
	f := novoCenario(t)
	sb := f.provisionado(t)
	f.launcher.linhas = []ports.LogLine{{Text: "um"}, {Text: "dois"}, {Text: "três"}}

	boom := errs.Internal("cliente sumiu")
	n := 0
	err := f.svc.StreamLogs(f.ctx, sb.ID, execution.LogFilter{}, func(execution.LogLine) error {
		n++
		return boom
	})
	if err == nil {
		t.Fatal("erro do emit precisa subir")
	}
	if n != 1 {
		t.Errorf("continuou emitindo depois do erro: %d linhas", n)
	}
}

// ── montagem ─────────────────────────────────────────────────────────────────

// TestRelogioObrigatorio: aceitar nil manteria a porta de enfeite — o serviço
// cairia em time.Now() por dentro e nenhum teste de ociosidade seria
// determinístico.
func TestRelogioObrigatorio(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewService aceitou relógio nulo")
		}
	}()
	execution.NewService(novoRepo(), &launcherFalso{}, &acessoFalso{}, &demandasFalsas{}, nil, execution.Config{})
}

func TestURLDeEndpointEDoDominio(t *testing.T) {
	got := execution.EndpointURL("dev.dop.app", "5f3a9c21-0000-0000-0000-000000000000", "portal-frontend")
	want := "https://portal-frontend--5f3a9c21.dev.dop.app"
	if got != want {
		t.Errorf("EndpointURL = %q; esperava %q", got, want)
	}
	if execution.EndpointURL("", "d1", "app") != "" {
		t.Error("sem domínio de ingress não há URL para prometer")
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// Duplos
// ═════════════════════════════════════════════════════════════════════════════

type cenario struct {
	ctx      context.Context
	svc      *execution.Service
	repo     *repoFalso
	launcher *launcherFalso
	access   *acessoFalso
	demands  *demandasFalsas
	clock    *relogioFixo
}

func novoCenario(t *testing.T) *cenario {
	t.Helper()
	c := &cenario{
		ctx: ctxutil.Into(context.Background(), ctxutil.Call{
			RequestID: "req-1", AccountID: "conta-a", ActorID: "u1", ActorKind: ctxutil.ActorUser,
		}),
		repo:     novoRepo(),
		launcher: &launcherFalso{tiers: []ports.IsolationTier{ports.TierNamespace, ports.TierKernelEmulated}},
		access:   &acessoFalso{papel: identity.RoleDeveloper},
		demands:  &demandasFalsas{dono: map[string]string{"demanda-1": "conta-a"}},
		clock:    &relogioFixo{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)},
	}
	c.repo.agora = c.clock.Now
	c.svc = execution.NewService(c.repo, c.launcher, c.access, c.demands, c.clock,
		execution.Config{DevboxImage: "dop/devbox:1", IngressDomain: "dev.dop.app"})
	return c
}

func (c *cenario) provisionado(t *testing.T) *execution.Sandbox {
	t.Helper()
	sb, err := c.svc.Provision(c.ctx, "demanda-1", ports.TierNamespace, "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	return sb
}

// ── relógio ──────────────────────────────────────────────────────────────────

type relogioFixo struct{ t time.Time }

func (r *relogioFixo) Now() time.Time { return r.t.UTC() }

// ── repositório ──────────────────────────────────────────────────────────────

type repoFalso struct {
	mu sync.Mutex
	// agora vem do MESMO relógio do serviço. Duplo que consulta o relógio de
	// parede reintroduz, no teste, exatamente a dependência que a porta Clock
	// existe para remover — e o teste passa ou falha conforme a hora do dia.
	agora       func() time.Time
	linhas      map[string]*execution.Sandbox
	seq         int
	transitions int
	touches     int
}

func novoRepo() *repoFalso {
	return &repoFalso{
		linhas: map[string]*execution.Sandbox{},
		agora:  func() time.Time { return time.Now().UTC() },
	}
}

func (r *repoFalso) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.linhas)
}

func (r *repoFalso) get(id string) *execution.Sandbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.linhas[id]
}

func (r *repoFalso) only(t *testing.T) *execution.Sandbox {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.linhas) != 1 {
		t.Fatalf("esperava exatamente uma linha, há %d", len(r.linhas))
	}
	for _, s := range r.linhas {
		return s
	}
	return nil
}

func (r *repoFalso) ByID(_ context.Context, accountID, id string) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.linhas[id]
	if !ok || s.AccountID != accountID { // toda consulta filtra por conta
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (r *repoFalso) ByIdempotencyKey(_ context.Context, accountID, key string) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.linhas {
		if s.AccountID == accountID && s.IdempotencyKey == key && key != "" {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *repoFalso) LiveByDemand(_ context.Context, accountID, demandID string) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.linhas {
		if s.AccountID == accountID && s.DemandID == demandID && !s.State.IsTerminal() {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *repoFalso) Create(_ context.Context, s *execution.Sandbox) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	cp := *s
	cp.ID = fmt.Sprintf("sbx-%d", r.seq)
	cp.CreatedAt, cp.UpdatedAt = r.agora(), r.agora()
	r.linhas[cp.ID] = &cp
	out := cp
	return &out, nil
}

func (r *repoFalso) MarkProvisioned(_ context.Context, accountID, id string, tier ports.IsolationTier, eps []execution.Endpoint) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.linhas[id]
	if !ok || s.AccountID != accountID {
		return nil, errs.NotFound("sandbox")
	}
	s.State, s.Tier, s.Endpoints = execution.StateActive, tier, eps
	cp := *s
	return &cp, nil
}

func (r *repoFalso) Transition(_ context.Context, accountID, id string, t execution.Transition) (*execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.linhas[id]
	if !ok || s.AccountID != accountID {
		return nil, errs.NotFound("sandbox")
	}
	// O duplo carrega a MESMA invariante da trigger do banco: destruído é
	// absorvente. Duplo mais permissivo que o real deixa passar o bug que o
	// real barraria — em produção, longe daqui.
	if s.State.IsTerminal() && t.To != execution.StateDestroyed {
		return nil, errs.Precondition("sandbox destruído não retoma")
	}
	r.transitions++
	s.State = t.To
	cp := *s
	return &cp, nil
}

func (r *repoFalso) TouchActivity(_ context.Context, accountID, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.linhas[id]; ok && s.AccountID == accountID {
		r.touches++
	}
	return nil
}

func (r *repoFalso) AccountsWithIdle(_ context.Context, olderThanSeconds int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	corte := time.Duration(olderThanSeconds) * time.Second
	vistas := map[string]bool{}
	var out []string
	for _, s := range r.linhas {
		if s.State == execution.StateActive && r.agora().Sub(s.LastActiveAt) >= corte &&
			!vistas[s.AccountID] {
			vistas[s.AccountID] = true
			out = append(out, s.AccountID)
		}
	}
	return out, nil
}

func (r *repoFalso) ListIdle(_ context.Context, accountID string, olderThanSeconds int) ([]execution.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	corte := time.Duration(olderThanSeconds) * time.Second
	var out []execution.Sandbox
	for _, s := range r.linhas {
		if s.AccountID == accountID && s.State == execution.StateActive &&
			r.agora().Sub(s.LastActiveAt) >= corte {
			out = append(out, *s)
		}
	}
	return out, nil
}

// ── launcher ─────────────────────────────────────────────────────────────────

type launcherFalso struct {
	falhasNoLaunch int
	tiers          []ports.IsolationTier
	entrega        ports.IsolationTier // vazio = entrega o que foi pedido
	sumiu          bool
	linhas         []ports.LogLine
	launches       int
	resumes        int
	destroys       int
	fases          map[string]ports.SandboxPhase
	// execs guarda os comandos recebidos, e execSaida o que devolver. Comando
	// que falha é RESULTADO nesta porta (garantia 15), então o duplo precisa
	// saber devolver código != 0 sem devolver erro.
	execs     []ports.ExecRequest
	execSaida *ports.ExecResult
	execErro  error
}

func (l *launcherFalso) tierEntregue(pedido ports.IsolationTier) ports.IsolationTier {
	if l.entrega != "" {
		return l.entrega
	}
	return pedido
}

func (l *launcherFalso) SupportedTiers(context.Context) ([]ports.IsolationTier, error) {
	return l.tiers, nil
}

func (l *launcherFalso) Launch(_ context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	l.launches++
	if l.falhasNoLaunch > 0 {
		l.falhasNoLaunch--
		return nil, errs.New(errs.KindUnavailable, "o substrato não respondeu")
	}
	if l.fases == nil {
		l.fases = map[string]ports.SandboxPhase{}
	}
	l.fases[spec.ID] = ports.PhaseActive
	return &ports.SandboxStatus{Phase: ports.PhaseActive, Tier: l.tierEntregue(spec.Tier)}, nil
}

func (l *launcherFalso) Suspend(_ context.Context, h ports.SandboxHandle) error {
	if l.fases != nil {
		l.fases[h.ID] = ports.PhaseSuspended
	}
	return nil
}

func (l *launcherFalso) Resume(_ context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	l.resumes++
	if l.fases != nil {
		l.fases[spec.ID] = ports.PhaseActive
	}
	return &ports.SandboxStatus{Phase: ports.PhaseActive, Tier: l.tierEntregue(spec.Tier)}, nil
}

func (l *launcherFalso) Destroy(_ context.Context, h ports.SandboxHandle) error {
	l.destroys++
	delete(l.fases, h.ID)
	return nil
}

func (l *launcherFalso) Describe(_ context.Context, h ports.SandboxHandle) (*ports.SandboxStatus, error) {
	if l.sumiu {
		return nil, errs.NotFound("sandbox %s", h.ID)
	}
	fase, ok := l.fases[h.ID]
	if !ok {
		return nil, errs.NotFound("sandbox %s", h.ID)
	}
	return &ports.SandboxStatus{Phase: fase, Tier: ports.TierNamespace}, nil
}

func (l *launcherFalso) Exec(_ context.Context, h ports.SandboxHandle, req ports.ExecRequest) (*ports.ExecResult, error) {
	l.execs = append(l.execs, req)
	if l.execErro != nil {
		return nil, l.execErro
	}
	if fase, ok := l.fases[h.ID]; !ok || fase != ports.PhaseActive {
		// O adaptador real recusa por Describe antes de tentar; o duplo faz o
		// mesmo para que o teste do domínio não passe por um caminho que a
		// porta não permite.
		return nil, errs.Precondition("sandbox %s não está ativo", h.ID)
	}
	if l.execSaida != nil {
		return l.execSaida, nil
	}
	return &ports.ExecResult{ExitCode: 0, Stdout: "ok\n"}, nil
}

func (l *launcherFalso) Tail(_ context.Context, _ ports.SandboxHandle, _ ports.LogQuery, emit func(ports.LogLine) error) error {
	for _, ln := range l.linhas {
		if err := emit(ln); err != nil {
			return err
		}
	}
	return nil
}

// ── portas estreitas para outros domínios ────────────────────────────────────

type acessoFalso struct{ papel identity.Role }

func (a *acessoFalso) Authorize(_ context.Context, userID, accountID string) (*identity.Membership, error) {
	if userID == "" || accountID == "" {
		return nil, errs.Permission("sem vínculo")
	}
	return &identity.Membership{UserID: userID, AccountID: accountID, Role: a.papel}, nil
}

type demandasFalsas struct{ dono map[string]string }

func (d *demandasFalsas) DemandAccount(_ context.Context, demandID string) (string, error) {
	acc, ok := d.dono[demandID]
	if !ok {
		return "", errs.NotFound("demanda")
	}
	return acc, nil
}

// O scheduler é ator de SISTEMA e não tem conta ativa. A varredura visita conta
// por conta, e cada visita continua acontecendo DENTRO de uma conta — o
// isolamento não é afrouxado, só a ordem de visita é decidida por fora.
func TestVarreduraDeSistemaAtravessaContasSemAfrouxarIsolamento(t *testing.T) {
	repo := novoRepo()
	launcher := &launcherFalso{tiers: []ports.IsolationTier{ports.TierNamespace}}
	relogio := &relogioFixo{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}

	// Duas contas, cada uma com um sandbox parado além do limite.
	for _, conta := range []string{"acc-1", "acc-2"} {
		id := "sb-" + conta
		repo.linhas[id] = &execution.Sandbox{
			ID: id, AccountID: conta, State: execution.StateActive,
			LastActiveAt: relogio.t.Add(-2 * execution.IdleTimeout),
		}
	}

	varredor := execution.NewSweeper(repo, launcher, relogio)
	contas, suspensos, err := varredor.SweepAllAccounts(context.Background())
	if err != nil {
		t.Fatalf("varredura: %v", err)
	}
	if contas != 2 || suspensos != 2 {
		t.Fatalf("esperava 2 contas e 2 suspensos, veio %d e %d", contas, suspensos)
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// RunCommand — a ponte por onde o agente AGE (spec do substrato §4).
//
// A regra que estes testes protegem é a mesma da garantia 15 da porta, um andar
// acima: erro é do SUBSTRATO; o que o comando fez, inclusive falhar, é resultado.
// ═════════════════════════════════════════════════════════════════════════════

func TestRunCommandRodaNoSandboxDaDemanda(t *testing.T) {
	c := novoCenario(t)
	sb := c.provisionado(t)

	res, err := c.svc.RunCommand(c.ctx, "demanda-1", ports.ExecRequest{
		Command: []string{"sh", "-c", "echo oi"},
	})
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("código de saída %d", res.ExitCode)
	}
	if len(c.launcher.execs) != 1 {
		t.Fatalf("o substrato recebeu %d comando(s)", len(c.launcher.execs))
	}
	// O runtime de agente pergunta pela DEMANDA; quem resolve demanda → sandbox
	// é este domínio. O agente nunca vê um id de sandbox.
	if sb.DemandID != "demanda-1" {
		t.Fatalf("sandbox da demanda errada: %+v", sb)
	}
}

// Comando que falha é RESULTADO. Se isto virar erro, o laço de ferramenta do
// agente perde a única informação que o modelo consegue usar para corrigir.
func TestRunCommandCodigoDeSaidaNaoEhErro(t *testing.T) {
	c := novoCenario(t)
	c.provisionado(t)
	c.launcher.execSaida = &ports.ExecResult{ExitCode: 3, Stderr: "reprovou"}

	res, err := c.svc.RunCommand(c.ctx, "demanda-1", ports.ExecRequest{Command: []string{"x"}})
	if err != nil {
		t.Fatalf("código != 0 virou erro do domínio: %v", err)
	}
	if res.ExitCode != 3 || res.Stderr != "reprovou" {
		t.Fatalf("o resultado do comando não chegou inteiro: %+v", res)
	}
}

// Trabalho de agente é ATIVIDADE: sem o toque, o varredor de economia derruba o
// sandbox debaixo do agente que está justamente trabalhando nele (spec §3).
func TestRunCommandAdiaASuspensaoPorOciosidade(t *testing.T) {
	c := novoCenario(t)
	sb := c.provisionado(t)
	c.repo.touches = 0

	if _, err := c.svc.RunCommand(c.ctx, "demanda-1", ports.ExecRequest{Command: []string{"x"}}); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if c.repo.touches == 0 {
		t.Fatalf("rodar comando não contou como atividade no sandbox %s: o varredor "+
			"suspenderia o sandbox no meio do trabalho do agente", sb.ID)
	}
}

func TestRunCommandRecusaOQueNaoPodeExecutar(t *testing.T) {
	t.Run("demanda_sem_sandbox", func(t *testing.T) {
		c := novoCenario(t)
		_, err := c.svc.RunCommand(c.ctx, "demanda-1", ports.ExecRequest{Command: []string{"x"}})
		if errs.KindOf(err) != errs.KindNotFound {
			t.Fatalf("esperava KindNotFound, veio %v", err)
		}
	})

	t.Run("sandbox_suspenso", func(t *testing.T) {
		c := novoCenario(t)
		sb := c.provisionado(t)
		if _, err := c.svc.Suspend(c.ctx, sb.ID); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		// Precondição, e nunca um código de saída inventado: substrato sem
		// execução não roda comando, e dizer isso é diferente de dizer que o
		// comando falhou.
		_, err := c.svc.RunCommand(c.ctx, "demanda-1", ports.ExecRequest{Command: []string{"x"}})
		if errs.KindOf(err) != errs.KindPrecondition {
			t.Fatalf("esperava KindPrecondition, veio %v", err)
		}
		if len(c.launcher.execs) != 0 {
			t.Fatal("o comando foi para o substrato mesmo com o sandbox suspenso")
		}
	})

	t.Run("viewer_nao_executa", func(t *testing.T) {
		c := novoCenario(t)
		c.provisionado(t)
		c.access.papel = identity.RoleViewer
		_, err := c.svc.RunCommand(c.ctx, "demanda-1", ports.ExecRequest{Command: []string{"x"}})
		if errs.KindOf(err) != errs.KindPermission {
			t.Fatalf("esperava KindPermission, veio %v", err)
		}
	})

	t.Run("comando_vazio", func(t *testing.T) {
		c := novoCenario(t)
		c.provisionado(t)
		_, err := c.svc.RunCommand(c.ctx, "demanda-1", ports.ExecRequest{})
		if errs.KindOf(err) != errs.KindInvalid {
			t.Fatalf("esperava KindInvalid, veio %v", err)
		}
	})

	t.Run("sandbox_de_outra_conta", func(t *testing.T) {
		c := novoCenario(t)
		c.provisionado(t)
		outra := ctxutil.Into(context.Background(), ctxutil.Call{
			AccountID: "conta-b", ActorID: "u2", ActorKind: ctxutil.ActorUser,
		})
		// Toda consulta filtra por conta: o sandbox da conta A não existe para
		// a conta B, e a resposta é a mesma de "não existe" — a outra
		// confirmaria que o id é real.
		_, err := c.svc.RunCommand(outra, "demanda-1", ports.ExecRequest{Command: []string{"x"}})
		if errs.KindOf(err) != errs.KindNotFound {
			t.Fatalf("esperava KindNotFound, veio %v", err)
		}
	})
}
