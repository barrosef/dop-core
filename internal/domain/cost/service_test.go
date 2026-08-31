package cost_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/cost"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM banco: o repositório é porta, e aqui entra um duplo
// em memória que reproduz a garantia que o adaptador tem de cumprir.

const (
	contaA   = "11111111-1111-1111-1111-111111111111"
	demandaA = "22222222-2222-2222-2222-222222222222"
)

var instante = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// relogioFixo é o duplo do Clock. Mora aqui, e não em internal/adapter/clock,
// porque o teste de arquitetura reprova QUALQUER import de adaptador sob
// internal/domain — inclusive em arquivo _test.go.
type relogioFixo struct{ t time.Time }

func (r relogioFixo) Now() time.Time { return r.t }

// ── duplo do repositório ─────────────────────────────────────────────────────

// repoMem reproduz a única garantia que importa aqui: a chave de idempotência
// é a guarda, e a repetição NÃO acumula orçamento de novo. Se este duplo
// contasse duas vezes, o teste passaria a medir o duplo em vez da regra.
type repoMem struct {
	chaves    map[string]*cost.UsageEvent // conta|chave → uso gravado
	orcamento map[string]*cost.Budget     // conta|escopo|id → orçamento
	usos      []cost.UsageEvent
	erro      error // injetável, para o caminho de falha do repositório
}

func novoRepo() *repoMem {
	return &repoMem{
		chaves:    map[string]*cost.UsageEvent{},
		orcamento: map[string]*cost.Budget{},
	}
}

func chaveOrc(accountID string, s cost.Scope, id string) string {
	return accountID + "|" + string(s) + "|" + id
}

func (r *repoMem) escopos(u *cost.UsageEvent) []cost.Budget {
	out := []cost.Budget{{AccountID: u.AccountID, Scope: cost.ScopeAccount, ScopeID: u.AccountID}}
	if u.DemandID != "" {
		out = append(out, cost.Budget{AccountID: u.AccountID, Scope: cost.ScopeDemand, ScopeID: u.DemandID})
	}
	return out
}

func (r *repoMem) linha(b cost.Budget) *cost.Budget {
	k := chaveOrc(b.AccountID, b.Scope, b.ScopeID)
	if cur, ok := r.orcamento[k]; ok {
		return cur
	}
	novo := b
	novo.Currency = cost.DefaultCurrency
	r.orcamento[k] = &novo
	return &novo
}

func (r *repoMem) RecordUsage(_ context.Context, u *cost.UsageEvent, key string) (*cost.RecordResult, error) {
	if r.erro != nil {
		return nil, r.erro
	}
	res := &cost.RecordResult{}

	if gravado, ok := r.chaves[u.AccountID+"|"+key]; ok {
		// Repetição: nada é escrito, o orçamento é devolvido como está.
		res.Duplicate = true
		res.Usage = gravado
		for _, e := range r.escopos(gravado) {
			cur := *r.linha(e)
			res.Budgets = append(res.Budgets, cost.BudgetState{Before: cur, After: cur})
		}
		return res, nil
	}

	u.ID = "uso-" + key
	r.usos = append(r.usos, *u)
	gravado := *u
	r.chaves[u.AccountID+"|"+key] = &gravado
	res.Usage = &gravado

	for _, e := range r.escopos(u) {
		linha := r.linha(e)
		antes := *linha
		linha.SpentMicros += u.CostMicros
		res.Budgets = append(res.Budgets, cost.BudgetState{Before: antes, After: *linha})
	}
	return res, nil
}

func (r *repoMem) BudgetOf(_ context.Context, accountID string, s cost.Scope, id string) (*cost.Budget, error) {
	if r.erro != nil {
		return nil, r.erro
	}
	b := *r.linha(cost.Budget{AccountID: accountID, Scope: s, ScopeID: id})
	return &b, nil
}

func (r *repoMem) SetBudget(_ context.Context, b *cost.Budget) (*cost.BudgetState, error) {
	if r.erro != nil {
		return nil, r.erro
	}
	linha := r.linha(*b)
	antes := *linha
	linha.LimitMicros = b.LimitMicros // acumulado PRESERVADO
	linha.UpdatedAt = b.UpdatedAt
	return &cost.BudgetState{Before: antes, After: *linha}, nil
}

func (r *repoMem) Summarize(_ context.Context, accountID string, s cost.Scope, id string,
	since, until time.Time, recentLimit int) (*cost.Summary, error) {
	if r.erro != nil {
		return nil, r.erro
	}
	out := &cost.Summary{Scope: s, ScopeID: id, Since: since, Until: until, Currency: cost.DefaultCurrency}
	for _, u := range r.usos {
		if u.AccountID != accountID {
			continue
		}
		if s == cost.ScopeDemand && u.DemandID != id {
			continue
		}
		if u.At.Before(since) || !u.At.Before(until) {
			continue
		}
		out.TotalMicros += u.CostMicros
		out.Calls++
		out.InputTokens += u.InputTokens
		out.OutputTokens += u.OutputTokens
		out.CacheReadTokens += u.CacheReadTokens
		out.CacheCreationTokens += u.CacheCreationTokens
		if len(out.Recent) < recentLimit {
			out.Recent = append(out.Recent, u)
		}
	}
	return out, nil
}

var _ cost.Repository = (*repoMem)(nil)

// ── auxiliares ───────────────────────────────────────────────────────────────

func ctxConta() context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		RequestID: "req-1", AccountID: contaA,
		ActorID: "ator-1", ActorKind: ctxutil.ActorAgent,
	})
}

func novoServico(r *repoMem) *cost.Service {
	return cost.NewService(r, relogioFixo{t: instante}, nil)
}

func usoDe(micros cost.Micros) cost.UsageEvent {
	return cost.UsageEvent{
		DemandID: demandaA, ThreadID: "th-1", Model: "claude-opus",
		InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 8000,
		CostMicros: micros, At: instante,
	}
}

// ════════════════════════════════════════════════════════════════════════════
// A garantia central: reenviar não conta duas vezes.
// ════════════════════════════════════════════════════════════════════════════

func TestRecordUsageReenviadoNaoContaDuasVezes(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	ctx := ctxConta()

	primeira, err := svc.RecordUsage(ctx, usoDe(500_000), "chave-1")
	if err != nil {
		t.Fatalf("primeiro registro falhou: %v", err)
	}
	if primeira.Duplicate {
		t.Fatal("o primeiro registro não pode ser marcado como repetição")
	}

	// Mesma chamada, reenviada — retentativa de rede, redelivery do broker,
	// cliente impaciente. Qualquer um dos três dobraria o orçamento sem isto.
	segunda, err := svc.RecordUsage(ctx, usoDe(500_000), "chave-1")
	if err != nil {
		t.Fatalf("reenvio falhou (deveria ser aceito como repetição): %v", err)
	}
	if !segunda.Duplicate {
		t.Error("reenvio da MESMA chave deveria ser reconhecido como repetição")
	}

	b, err := svc.GetBudget(ctx, cost.ScopeAccount, "")
	if err != nil {
		t.Fatalf("leitura do orçamento falhou: %v", err)
	}
	if b.SpentMicros != 500_000 {
		t.Errorf("gasto acumulado = %d micros, esperado 500000 — o reenvio contou duas vezes",
			b.SpentMicros)
	}
	if len(repo.usos) != 1 {
		t.Errorf("registros gravados = %d, esperado 1", len(repo.usos))
	}

	// Chave DIFERENTE é consumo diferente — a guarda não pode virar mordaça.
	if _, err := svc.RecordUsage(ctx, usoDe(500_000), "chave-2"); err != nil {
		t.Fatalf("segundo consumo legítimo falhou: %v", err)
	}
	b, _ = svc.GetBudget(ctx, cost.ScopeAccount, "")
	if b.SpentMicros != 1_000_000 {
		t.Errorf("gasto após consumo novo = %d, esperado 1000000", b.SpentMicros)
	}
}

func TestRecordUsageExigeChaveDeIdempotencia(t *testing.T) {
	svc := novoServico(novoRepo())
	// Sem chave a duplicata não colide com nada: entraria como consumo
	// legítimo e o orçamento viraria ficção. Recusar é a decisão.
	_, err := svc.RecordUsage(ctxConta(), usoDe(1), "   ")
	if err == nil {
		t.Fatal("registro sem chave de idempotência deveria ser recusado")
	}
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("erro = %v (%s), esperado invalid_argument", err, errs.KindOf(err))
	}
}

func TestRecordUsageExigeContaAtiva(t *testing.T) {
	svc := novoServico(novoRepo())
	if _, err := svc.RecordUsage(context.Background(), usoDe(1), "k"); err == nil {
		t.Fatal("registro sem conta ativa deveria ser recusado")
	}
}

func TestRecordUsageIgnoraContaDoCorpo(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	u := usoDe(10)
	u.AccountID = "conta-do-vizinho"
	if _, err := svc.RecordUsage(ctxConta(), u, "k"); err != nil {
		t.Fatalf("registro falhou: %v", err)
	}
	if repo.usos[0].AccountID != contaA {
		t.Errorf("conta gravada = %q, esperado a do contexto (%q)", repo.usos[0].AccountID, contaA)
	}
}

// ════════════════════════════════════════════════════════════════════════════
// Estouro PAUSA — não mata.
// ════════════════════════════════════════════════════════════════════════════

func TestEstouroDeOrcamentoPausaEmVezDeMatar(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	ctx := ctxConta()

	if _, err := svc.SetBudget(ctx, cost.Budget{
		Scope: cost.ScopeDemand, ScopeID: demandaA, LimitMicros: 1_000_000,
	}); err != nil {
		t.Fatalf("definição do teto falhou: %v", err)
	}

	// Consumo abaixo do teto: nada acontece.
	dentro, err := svc.RecordUsage(ctx, usoDe(400_000), "k1")
	if err != nil {
		t.Fatalf("registro dentro do teto falhou: %v", err)
	}
	if dentro.BudgetExceeded {
		t.Error("400000 de 1000000 não deveria acusar estouro")
	}

	// Consumo que cruza o teto: a escrita CONTINUA VALENDO e o aviso vem
	// junto. Um erro aqui perderia a medição justo quando ela mais importa.
	estoura, err := svc.RecordUsage(ctx, usoDe(700_000), "k2")
	if err != nil {
		t.Fatalf("estouro NÃO pode falhar a escrita — trabalho em andamento não morre em silêncio: %v", err)
	}
	if !estoura.BudgetExceeded {
		t.Fatal("1100000 de 1000000 deveria acusar estouro")
	}
	if len(estoura.Exceeded) == 0 {
		t.Error("o escopo estourado precisa vir na resposta — é o item da caixa de atenção")
	}

	// E o trabalho em andamento segue registrando: pausar é decisão de quem
	// consome o evento, não recusa desta escrita.
	depois, err := svc.RecordUsage(ctx, usoDe(100_000), "k3")
	if err != nil {
		t.Fatalf("registro após o estouro falhou — isso seria matar em vez de pausar: %v", err)
	}
	if !depois.BudgetExceeded {
		t.Error("o aviso de estouro precisa persistir enquanto o orçamento estiver estourado")
	}

	b, _ := svc.GetBudget(ctx, cost.ScopeDemand, demandaA)
	if b.SpentMicros != 1_200_000 {
		t.Errorf("gasto = %d, esperado 1200000 — nenhuma escrita pode ter sido perdida", b.SpentMicros)
	}
	if len(repo.usos) != 3 {
		t.Errorf("registros gravados = %d, esperado 3", len(repo.usos))
	}
}

func TestTetoRebaixadoAbaixoDoGastoEstoura(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	ctx := ctxConta()

	if _, err := svc.RecordUsage(ctx, usoDe(900_000), "k1"); err != nil {
		t.Fatalf("registro falhou: %v", err)
	}
	// Operador fecha a torneira de uma demanda que está queimando dinheiro.
	antes := cost.Budget{AccountID: contaA, Scope: cost.ScopeDemand, ScopeID: demandaA,
		LimitMicros: 0, SpentMicros: 900_000}
	depois := antes
	depois.LimitMicros = 500_000
	if !cost.NewlyExceeded(antes, depois) {
		t.Error("rebaixar o teto abaixo do gasto é estouro tão real quanto gastar além do teto")
	}

	b, err := svc.SetBudget(ctx, cost.Budget{
		Scope: cost.ScopeDemand, ScopeID: demandaA, LimitMicros: 500_000,
	})
	if err != nil {
		t.Fatalf("rebaixar o teto falhou: %v", err)
	}
	// O acumulado é do sistema: definir teto não zera gasto.
	if b.SpentMicros != 900_000 {
		t.Errorf("gasto após SetBudget = %d, esperado 900000 (preservado)", b.SpentMicros)
	}
	if !b.Exceeded() {
		t.Error("orçamento rebaixado abaixo do gasto deveria estar estourado")
	}
}

func TestLimiteZeroEhSemTetoNaoTetoZero(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()

	// Conta nova, sem orçamento definido: o primeiro token NÃO pode nascer
	// estourado, senão nada funciona antes de alguém configurar teto.
	out, err := svc.RecordUsage(ctx, usoDe(999_999_999), "k1")
	if err != nil {
		t.Fatalf("registro falhou: %v", err)
	}
	if out.BudgetExceeded {
		t.Error("ausência de orçamento não pode virar orçamento zero")
	}
	b := cost.Budget{LimitMicros: 0, SpentMicros: 1}
	if !b.Unlimited() || b.Exceeded() {
		t.Error("limite zero significa SEM TETO")
	}
}

func TestNewlyExceededEhTransicaoNaoEstado(t *testing.T) {
	estourado := cost.Budget{LimitMicros: 100, SpentMicros: 150}
	maisEstourado := cost.Budget{LimitMicros: 100, SpentMicros: 200}
	// Já estourado gera UM item na caixa de atenção, não um por turno.
	if cost.NewlyExceeded(estourado, maisEstourado) {
		t.Error("orçamento já estourado não deveria emitir estouro de novo")
	}
	if !cost.NewlyExceeded(cost.Budget{LimitMicros: 100, SpentMicros: 50}, estourado) {
		t.Error("cruzar o teto deveria ser transição")
	}
}

func TestEscopoInvalidoEhRecusado(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()
	if _, err := svc.GetBudget(ctx, "galaxia", "x"); err == nil {
		t.Error("escopo fora do vocabulário deveria ser recusado")
	}
	if _, err := svc.GetBudget(ctx, cost.ScopeDemand, ""); err == nil {
		t.Error("escopo de demanda sem identificador deveria ser recusado")
	}
	// Escopo de conta usa SEMPRE a conta ativa, mesmo se pedirem outra.
	b, err := svc.GetBudget(ctx, cost.ScopeAccount, "conta-do-vizinho")
	if err != nil {
		t.Fatalf("leitura falhou: %v", err)
	}
	if b.ScopeID != contaA {
		t.Errorf("escopo de conta = %q, esperado a conta ativa (%q)", b.ScopeID, contaA)
	}
}

func TestNewServiceRecusaRelogioNulo(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("relógio nulo deveria provocar panic no boot, não silêncio em produção")
		}
	}()
	cost.NewService(novoRepo(), nil, nil)
}

// ════════════════════════════════════════════════════════════════════════════
// A tabela do roteador produz decisão COM justificativa.
// ════════════════════════════════════════════════════════════════════════════

func TestTabelaDoRoteadorProduzDecisaoComJustificativa(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()

	casos := []struct {
		kind   cost.TaskKind
		classe cost.ModelClass
		effort cost.Effort
	}{
		{cost.TaskMechanical, cost.ClassCheap, cost.EffortLow},
		{cost.TaskInvestigation, cost.ClassMedium, cost.EffortMedium},
		{cost.TaskImplementation, cost.ClassStrong, cost.EffortHigh},
		{cost.TaskCritic, cost.ClassStrong, cost.EffortMax},
	}
	for _, c := range casos {
		d, err := svc.RouteModel(ctx, c.kind, demandaA)
		if err != nil {
			t.Fatalf("%s: roteamento falhou: %v", c.kind, err)
		}
		if d.Class != c.classe || d.Effort != c.effort {
			t.Errorf("%s → (%s, %s), esperado (%s, %s) — ADR-0011 §3",
				c.kind, d.Class, d.Effort, c.classe, c.effort)
		}
		if d.Model == "" {
			t.Errorf("%s: a classe precisa resolver para um modelo concreto", c.kind)
		}
		// Sem o porquê ninguém audita nem calibra.
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("%s: decisão sem justificativa", c.kind)
		}
		// E a justificativa diz de onde veio — inclusive que ainda é rascunho.
		if !strings.Contains(d.Reason, "ADR-0011") || !strings.Contains(d.Reason, "P-7") {
			t.Errorf("%s: justificativa %q não declara proveniência nem a pendência de calibração",
				c.kind, d.Reason)
		}
	}
}

func TestNaoSeEconomizaNoCritico(t *testing.T) {
	svc := novoServico(novoRepo())
	critico, err := svc.RouteModel(ctxConta(), cost.TaskCritic, "")
	if err != nil {
		t.Fatalf("roteamento falhou: %v", err)
	}
	// Regra FIXA da ADR-0011/0012: o crítico é o freio, e o freio é o último
	// lugar onde se economiza.
	if critico.Class != cost.ClassStrong || critico.Effort != cost.EffortMax {
		t.Errorf("crítico = (%s, %s), esperado (strong, max)", critico.Class, critico.Effort)
	}
}

func TestTipoDeTrabalhoDesconhecidoCaiNoLadoCaro(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()

	d, err := svc.RouteModel(ctx, "arqueologia", "")
	if err != nil {
		t.Fatalf("tipo desconhecido não pode parar trabalho em andamento: %v", err)
	}
	if d.Class != cost.ClassStrong {
		t.Errorf("fallback = %s, esperado strong — na dúvida não se economiza", d.Class)
	}
	if !strings.Contains(d.Reason, "fora do vocabulário") {
		t.Errorf("o fallback precisa DIZER que caiu no fallback; justificativa = %q", d.Reason)
	}

	// Tipo VAZIO é outra coisa: requisição incompleta.
	if _, err := svc.RouteModel(ctx, "", ""); err == nil {
		t.Error("tipo de trabalho vazio deveria ser recusado")
	}
}

func TestRoteadorEhTabelaEmUmLugarSo(t *testing.T) {
	// Table() é o que a tela de calibração de P-7 lê. Se a política deixar de
	// ser tabela, este teste é o primeiro a notar.
	tabela := cost.NewRouter(nil).Table()
	if len(tabela) != 4 {
		t.Fatalf("a tabela tem %d linhas, esperado 4 (ADR-0011 §3)", len(tabela))
	}
	vistos := map[cost.TaskKind]bool{}
	for _, d := range tabela {
		if vistos[d.TaskKind] {
			t.Errorf("tipo %s aparece duas vezes na tabela", d.TaskKind)
		}
		vistos[d.TaskKind] = true
	}
}

func TestCatalogoSubstituivelSemTocarNaPolitica(t *testing.T) {
	// Nome de modelo muda de líder por semestre (ADR-0001); a política não
	// pode mudar junto.
	r := cost.NewRouter(cost.ModelCatalog{
		cost.ClassCheap:  "modelo-barato-do-fornecedor-x",
		cost.ClassMedium: "modelo-medio-do-fornecedor-x",
		cost.ClassStrong: "modelo-forte-do-fornecedor-x",
	})
	d, err := r.Route(cost.TaskCritic)
	if err != nil {
		t.Fatalf("roteamento falhou: %v", err)
	}
	if d.Model != "modelo-forte-do-fornecedor-x" {
		t.Errorf("modelo = %q, o catálogo não foi respeitado", d.Model)
	}
	if d.Effort != cost.EffortMax {
		t.Error("trocar o catálogo não pode mexer na política")
	}
}

// ════════════════════════════════════════════════════════════════════════════
// Telemetria de cache — o material de calibração (ADR-0012, P-7).
// ════════════════════════════════════════════════════════════════════════════

func TestTaxaDeAcertoDeCacheUsaOPromptInteiro(t *testing.T) {
	s := cost.Summary{InputTokens: 1000, CacheReadTokens: 9000, CacheCreationTokens: 0}
	if got := s.CacheHitRatio(); got < 0.89 || got > 0.91 {
		t.Errorf("taxa = %.3f, esperado ~0.900 (9000 de 10000 do prompt)", got)
	}
	if (cost.Summary{}).CacheHitRatio() != 0 {
		t.Error("período sem uso deveria devolver zero, não NaN")
	}
}

func TestAlertaDeInvalidadorSilenciosoDeCache(t *testing.T) {
	// Prefixo grande sem NENHUMA leitura de cache: alguém está pagando 10× o
	// mesmo prefixo (ADR-0012 §1).
	suspeito := cost.UsageEvent{InputTokens: 50_000, CacheReadTokens: 0}
	if !suspeito.SuspectCacheMiss() {
		t.Error("prompt grande sem leitura de cache deveria ser suspeito")
	}
	// Turno com cache servido: normal.
	ok := cost.UsageEvent{InputTokens: 50_000, CacheReadTokens: 40_000}
	if ok.SuspectCacheMiss() {
		t.Error("turno com leitura de cache não é suspeito")
	}
	// Prompt pequeno: não vale cache, não é alerta.
	pequeno := cost.UsageEvent{InputTokens: 10, CacheReadTokens: 0}
	if pequeno.SuspectCacheMiss() {
		t.Error("prompt pequeno demais para cachear não é alerta")
	}
}

func TestSummarizeUsaMesCorrentePorPadrao(t *testing.T) {
	repo := novoRepo()
	svc := novoServico(repo)
	ctx := ctxConta()

	if _, err := svc.RecordUsage(ctx, usoDe(250_000), "k1"); err != nil {
		t.Fatalf("registro falhou: %v", err)
	}
	// Consumo do mês passado não entra na janela padrão.
	antigo := usoDe(999_000)
	antigo.At = instante.AddDate(0, -1, 0)
	if _, err := svc.RecordUsage(ctx, antigo, "k0"); err != nil {
		t.Fatalf("registro antigo falhou: %v", err)
	}

	var zero time.Time
	sum, err := svc.Summarize(ctx, cost.ScopeAccount, "", zero, zero, 10)
	if err != nil {
		t.Fatalf("resumo falhou: %v", err)
	}
	if sum.TotalMicros != 250_000 {
		t.Errorf("total = %d, esperado 250000 (só o mês corrente)", sum.TotalMicros)
	}
	if sum.Calls != 1 {
		t.Errorf("chamadas = %d, esperado 1", sum.Calls)
	}

	desde, ate := cost.CurrentMonth(instante)
	if desde != time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) || ate != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("janela padrão = [%s, %s), esperado agosto/2026", desde, ate)
	}
}

func TestValidacaoDeUso(t *testing.T) {
	svc := novoServico(novoRepo())
	ctx := ctxConta()

	semModelo := usoDe(1)
	semModelo.Model = ""
	if _, err := svc.RecordUsage(ctx, semModelo, "k1"); err == nil {
		t.Error("uso sem modelo deveria ser recusado — sem ele não há calibração")
	}

	negativo := usoDe(1)
	negativo.InputTokens = -1
	if _, err := svc.RecordUsage(ctx, negativo, "k2"); err == nil {
		t.Error("contagem negativa de tokens deveria ser recusada")
	}

	// Instante ausente é preenchido pelo relógio da PORTA, nunca por time.Now.
	semInstante := usoDe(1)
	semInstante.At = time.Time{}
	out, err := svc.RecordUsage(ctx, semInstante, "k3")
	if err != nil {
		t.Fatalf("registro sem instante falhou: %v", err)
	}
	if !out.Usage.At.Equal(instante) {
		t.Errorf("instante = %s, esperado o do relógio injetado (%s)", out.Usage.At, instante)
	}
}
