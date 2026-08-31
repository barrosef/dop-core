package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── duplos das quatro portas ────────────────────────────────────────────────

type provedorFalso struct {
	info     agent.ProviderInfo
	resposta *agent.Reply
	erro     error

	modeloPedido string
	effortPedido agent.Effort
	turnoPedido  agent.Turn
}

func (p *provedorFalso) Info() agent.ProviderInfo { return p.info }

func (p *provedorFalso) Render(t agent.Turn, model string, e agent.Effort) ([]byte, []string, error) {
	b, err := json.Marshal(map[string]any{"model": model, "prefix": t.StablePrefix})
	return b, nil, err
}

func (p *provedorFalso) Send(_ context.Context, t agent.Turn, model string, e agent.Effort) (*agent.Reply, error) {
	p.modeloPedido, p.effortPedido, p.turnoPedido = model, e, t
	if p.erro != nil {
		return nil, p.erro
	}
	r := *p.resposta
	if r.Capabilities == nil {
		r.Capabilities = p.info.Capabilities
	}
	if r.Provider == "" {
		r.Provider = p.info.Name
	}
	return &r, nil
}

func (p *provedorFalso) For(context.Context, string) (agent.AgentProvider, error) { return p, nil }

type conhecimentoFalso struct{ pkg agent.ContextPackage }

func (c conhecimentoFalso) ContextPackage(context.Context, string) (agent.ContextPackage, error) {
	return c.pkg, nil
}

type custoFalso struct {
	decisao   agent.Decision
	conta     agent.Accounting
	consumos  []agent.Consumption
	chavesUso []string
}

func (c *custoFalso) Route(context.Context, string, string) (agent.Decision, error) {
	return c.decisao, nil
}

func (c *custoFalso) RecordUsage(_ context.Context, u agent.Consumption, k string) (agent.Accounting, error) {
	c.consumos = append(c.consumos, u)
	c.chavesUso = append(c.chavesUso, k)
	return c.conta, nil
}

// mensagemGravada guarda o que o log de eventos guardaria — em especial QUEM
// falou, que é o que o ciclo do turno decide.
type mensagemGravada struct {
	texto     string
	chave     string
	autorTipo ctxutil.ActorKind
	autorID   string
	autorNome string
}

type conversaFalsa struct {
	thread    agent.Thread
	mensagens []mensagemGravada
	achados   []mensagemGravada
}

func (c *conversaFalsa) Thread(context.Context, string, string) (agent.Thread, error) {
	return c.thread, nil
}

func (c *conversaFalsa) PostMessage(ctx context.Context, _, text, idemKey string) (string, error) {
	call, _ := ctxutil.From(ctx)
	c.mensagens = append(c.mensagens, mensagemGravada{
		texto: text, chave: idemKey,
		autorTipo: call.ActorKind, autorID: call.ActorID, autorNome: call.ActorName,
	})
	return "msg-" + idemKey, nil
}

func (c *conversaFalsa) PublishFinding(ctx context.Context, _, _, title string,
	payload map[string]any, idemKey string) (agent.FindingRef, error) {
	call, _ := ctxutil.From(ctx)
	c.achados = append(c.achados, mensagemGravada{
		texto: title, chave: idemKey, autorTipo: call.ActorKind, autorID: call.ActorID,
	})
	_ = payload
	return agent.FindingRef{ID: "ach-1", Title: title}, nil
}

// ── montagem ────────────────────────────────────────────────────────────────

func contexto() context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		RequestID: "req-1", AccountID: "acct-1",
		ActorID: "usr-ana", ActorKind: ctxutil.ActorUser, ActorName: "Ana",
	})
}

func fichaDoProvedor() agent.ProviderInfo {
	return agent.ProviderInfo{
		Name: "fornecedor-x",
		Catalog: map[agent.ModelClass]string{
			agent.ClassCheap:  "x-pequeno",
			agent.ClassMedium: "x-medio",
			agent.ClassStrong: "x-grande",
		},
		Capabilities: agent.Capabilities{agent.CapCacheCreationAccounting},
		Prices: map[string]agent.Price{
			"x-grande": {Currency: "USD", InputPer1k: 5_000, OutputPer1k: 25_000,
				CacheReadPer1k: 500, CacheCreationPer1k: 6_250},
		},
	}
}

func respostaConcluindo() *agent.Reply {
	return &agent.Reply{
		Text:  `{"reply":"pronto"}`,
		Model: "x-grande",
		Usage: agent.Usage{InputTokens: 1000, OutputTokens: 200,
			CacheReadTokens: 400, CacheCreationTokens: 100},
		StopReason:    agent.StopCompleted,
		EffortApplied: agent.EffortHigh,
		Data: map[string]any{
			"reply": "pronto", "concluded": true,
			"finding_title": "o bug estava no cache", "finding_summary": "resumo concreto",
			"finding_evidence": []any{"log da linha 42"},
		},
	}
}

type cenario struct {
	svc  *agent.Service
	prov *provedorFalso
	cust *custoFalso
	conv *conversaFalsa
}

func montar(t *testing.T, pkg agent.ContextPackage, resposta *agent.Reply,
	card agent.AgentCard, conta agent.Accounting) cenario {
	t.Helper()
	prov := &provedorFalso{info: fichaDoProvedor(), resposta: resposta}
	cust := &custoFalso{
		decisao: agent.Decision{TaskKind: "implementation", Class: agent.ClassStrong,
			Model: "claude-opus", Effort: agent.EffortHigh, Reason: "ADR-0011 §3: porque sim"},
		conta: conta,
	}
	conv := &conversaFalsa{thread: agent.Thread{ID: "thr-1", Key: "principal", Card: card}}
	return cenario{
		svc:  agent.NewService(prov, conhecimentoFalso{pkg}, cust, conv),
		prov: prov, cust: cust, conv: conv,
	}
}

func pedido() agent.TurnRequest {
	return agent.TurnRequest{
		DemandID: "dem-1", ThreadID: "thr-1",
		Text: "por que o build quebrou?", TaskKind: "implementation",
	}
}

// ── testes ──────────────────────────────────────────────────────────────────

// A autoria é a razão de a plataforma existir: distinguir o que o humano fez do
// que o agente fez. Se este teste cair, o log de eventos — que é a verdade da
// demanda (ADR-0006) — passa a mentir sobre quem fez o quê.
func TestAutoriaDaRespostaEhDoAgente(t *testing.T) {
	c := montar(t, agent.ContextPackage{}, respostaConcluindo(), agent.AgentCard{}, agent.Accounting{})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-1"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(c.conv.mensagens) != 2 {
		t.Fatalf("esperava 2 mensagens (pergunta e resposta), veio %d", len(c.conv.mensagens))
	}

	pergunta, resposta := c.conv.mensagens[0], c.conv.mensagens[1]
	if pergunta.autorTipo != ctxutil.ActorUser || pergunta.autorID != "usr-ana" {
		t.Fatalf("a PERGUNTA deixou de ser do humano: %+v", pergunta)
	}
	if resposta.autorTipo != ctxutil.ActorAgent {
		t.Fatalf("a RESPOSTA foi gravada como %q: fala de agente registrada como fala de "+
			"humano faz o log de eventos mentir sobre quem fez o quê", resposta.autorTipo)
	}
	if resposta.autorID != "thr-1" || resposta.autorNome != "principal" {
		t.Fatalf("o ator do agente é a THREAD (dop.v1.ActorRef): veio id=%q nome=%q",
			resposta.autorID, resposta.autorNome)
	}
	// E o achado também: ele é durável, vai para a memória do projeto, e sair
	// assinado pelo humano faria a auditoria apontar para a pessoa errada.
	if len(c.conv.achados) != 1 || c.conv.achados[0].autorTipo != ctxutil.ActorAgent {
		t.Fatalf("o ACHADO não saiu assinado pelo agente: %+v", c.conv.achados)
	}
}

// As cinco escritas derivam da MESMA chave: é o que faz reenviar a requisição
// repetir zero efeitos.
func TestChavesDeIdempotenciaSaoDerivadas(t *testing.T) {
	c := montar(t, agent.ContextPackage{Dropped: agent.ContextDropped{Rules: 1}},
		respostaConcluindo(), agent.AgentCard{}, agent.Accounting{})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-42"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	esperadas := []string{"turno-42:msg-in", "turno-42:notice", "turno-42:msg-out"}
	for i, quero := range esperadas {
		if c.conv.mensagens[i].chave != quero {
			t.Fatalf("mensagem %d com chave %q, esperava %q", i, c.conv.mensagens[i].chave, quero)
		}
	}
	if c.cust.chavesUso[0] != "turno-42:usage" {
		t.Fatalf("consumo com chave %q", c.cust.chavesUso[0])
	}
	if c.conv.achados[0].chave != "turno-42:finding" {
		t.Fatalf("achado com chave %q", c.conv.achados[0].chave)
	}
}

func TestChaveDeIdempotenciaEhObrigatoria(t *testing.T) {
	c := montar(t, agent.ContextPackage{}, respostaConcluindo(), agent.AgentCard{}, agent.Accounting{})
	_, err := c.svc.RunTurn(contexto(), pedido(), "  ")
	if err == nil {
		t.Fatal("turno sem chave deveria ser recusado: gerar uma aqui transformaria um " +
			"retry de rede em consumo em dobro")
	}
	if errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("erro do tipo %q, esperava argumento inválido", errs.KindOf(err))
	}
}

// Concluir EXIGE achado (spec §1): sem título e resumo, a conclusão é recusada e
// a thread continua ativa, com aviso.
func TestConclusaoSemAchadoEhRecusada(t *testing.T) {
	r := respostaConcluindo()
	r.Data = map[string]any{"reply": "acho que terminei", "concluded": true}
	c := montar(t, agent.ContextPackage{}, r, agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.Concluded {
		t.Fatal("conclusão vazia foi aceita: a thread morreria em silêncio com um `true` de enfeite")
	}
	if len(c.conv.achados) != 0 {
		t.Fatal("publicou achado vazio")
	}
	if !contemAviso(out.Warnings, "RECUSADA") {
		t.Fatalf("a recusa não virou aviso legível: %v", out.Warnings)
	}
}

// Orçamento estourado PAUSA e não mata: o turno que já rodou é entregue inteiro.
func TestOrcamentoEstouradoPausaSemPerderOTurno(t *testing.T) {
	conta := agent.Accounting{BudgetExceeded: true, Exceeded: []agent.BudgetView{
		{Scope: "demand", ScopeID: "dem-1", LimitMicros: 1000, SpentMicros: 4200, Currency: "USD"},
	}}
	c := montar(t, agent.ContextPackage{}, respostaConcluindo(), agent.AgentCard{}, conta)

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatal("estouro de orçamento virou ERRO: a ADR-0011 §2 recusou o corte duro")
	}
	if !out.Paused {
		t.Fatal("o estouro não pausou")
	}
	if out.Reply == "" || out.Finding == nil {
		t.Fatal("o turno já pago foi jogado fora: a resposta e o achado precisam sair inteiros")
	}
	if !strings.Contains(out.Notice, "4200") || !strings.Contains(out.Notice, "dem-1") {
		t.Fatalf("o aviso não diz o que a caixa de atenção precisa mostrar: %q", out.Notice)
	}
}

// O truncamento aparece na CONVERSA, não só no resultado.
func TestTruncamentoViraMensagemNaThread(t *testing.T) {
	pkg := agent.ContextPackage{Dropped: agent.ContextDropped{Rules: 2, Memories: 3}}
	c := montar(t, pkg, respostaConcluindo(), agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !out.ContextTruncated {
		t.Fatal("o pacote veio truncado e o resultado não disse")
	}
	if len(c.conv.mensagens) != 3 || !strings.Contains(c.conv.mensagens[1].texto, "truncado") {
		t.Fatalf("o aviso de truncamento não entrou na thread: %+v", c.conv.mensagens)
	}
	// E o agente também precisa saber, ANTES de afirmar coisas sobre o que não leu.
	if !strings.Contains(c.prov.turnoPedido.StablePrefix, "TRUNCADO") {
		t.Fatal("o prefixo não avisou o agente de que o contexto veio parcial")
	}
}

// A CLASSE chega inteira e vira nome pelo catálogo do fornecedor ATIVO — é a
// tradução que aposentou o `catalog.py` do BFF (ADR-0023).
func TestClasseViraNomePeloCatalogoDoFornecedor(t *testing.T) {
	c := montar(t, agent.ContextPackage{}, respostaConcluindo(), agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	// O roteador devolveu "claude-opus" (catálogo DELE) com classe `strong`; o
	// fornecedor ativo chama a classe forte de outra coisa.
	if c.prov.modeloPedido != "x-grande" {
		t.Fatalf("mandou %q ao fornecedor, esperava o nome do catálogo dele (x-grande)",
			c.prov.modeloPedido)
	}
	if out.Routing.Class != agent.ClassStrong || out.Routing.Reason == "" {
		t.Fatalf("a decisão perdeu classe ou justificativa: %+v", out.Routing)
	}
}

// A ficha da thread vence o roteador, e o nome dela passa INTACTO.
func TestFichaDaThreadVenceORoteador(t *testing.T) {
	card := agent.AgentCard{Purpose: "forense", Model: "modelo-congelado-da-thread", Effort: "max"}
	c := montar(t, agent.ContextPackage{}, respostaConcluindo(), card, agent.Accounting{})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if c.prov.modeloPedido != "modelo-congelado-da-thread" {
		t.Fatalf("o modelo da ficha não passou intacto: %q", c.prov.modeloPedido)
	}
	if c.prov.effortPedido != agent.EffortMax {
		t.Fatalf("o effort da ficha não venceu: %q", c.prov.effortPedido)
	}
	if !out.Routing.FromAgentCard {
		t.Fatal("o resultado não registrou que a ficha venceu")
	}
}

// Preço desconhecido NÃO vira zero em silêncio.
func TestPrecoDesconhecidoSaiComoAusencia(t *testing.T) {
	r := respostaConcluindo()
	r.Model = "modelo-sem-tabela"
	c := montar(t, agent.ContextPackage{}, r, agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.Usage.CostKnown {
		t.Fatal("afirmou conhecer o preço de um modelo fora da tabela")
	}
	if out.Usage.CostMicros != 0 || out.Usage.Currency != "" {
		t.Fatalf("inventou custo: %+v", out.Usage)
	}
	if !contemAviso(out.Warnings, "AUSÊNCIA de tabela") {
		t.Fatalf("o custo zerado não veio explicado: %v", out.Warnings)
	}
	// E o consumo em TOKENS foi registrado assim mesmo: medição que some
	// quando o preço falta é medição que some justo quando importa.
	if c.cust.consumos[0].InputTokens != 1000 {
		t.Fatalf("o consumo em tokens não foi registrado: %+v", c.cust.consumos[0])
	}
}

// O custo é aritmética INTEIRA, por 1.000 tokens, e vai para o registro.
func TestCustoEmMicrosInteiros(t *testing.T) {
	c := montar(t, agent.ContextPackage{}, respostaConcluindo(), agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	// 1000*5000 + 200*25000 + 400*500 + 100*6250 = 10.825.000 → /1000 = 10.825
	const esperado = agent.Micros(10_825)
	if out.Usage.CostMicros != esperado {
		t.Fatalf("custo %d, esperava %d", out.Usage.CostMicros, esperado)
	}
	if c.cust.consumos[0].CostMicros != esperado || c.cust.consumos[0].Currency != "USD" {
		t.Fatalf("o custo não chegou ao registro: %+v", c.cust.consumos[0])
	}
}

// Quando o provedor não reporta criação de cache, o zero sai DECLARADO como
// ausência — e não como afirmação de que nada foi escrito (D1).
func TestCriacaoDeCacheDesconhecidaSaiDeclarada(t *testing.T) {
	prov := &provedorFalso{info: agent.ProviderInfo{
		Name:         "sem-contabilidade",
		Catalog:      map[agent.ModelClass]string{agent.ClassStrong: "y-grande"},
		Capabilities: agent.Capabilities{},
	}, resposta: respostaConcluindo()}
	cust := &custoFalso{decisao: agent.Decision{Class: agent.ClassStrong, Effort: agent.EffortHigh}}
	conv := &conversaFalsa{thread: agent.Thread{ID: "thr-1", Key: "principal"}}
	svc := agent.NewService(prov, conhecimentoFalso{}, cust, conv)

	out, err := svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.Usage.CacheCreationKnown {
		t.Fatal("afirmou conhecer criação de cache num provedor que não reporta")
	}
	if !contemAviso(out.Warnings, "AUSÊNCIA de informação") {
		t.Fatalf("o zero não veio explicado: %v", out.Warnings)
	}
}

// Indisponibilidade do fornecedor sobe INTACTA, com o Kind traduzido — nunca
// como erro interno nosso.
func TestIndisponibilidadeDoFornecedorNaoViraErroNosso(t *testing.T) {
	c := montar(t, agent.ContextPackage{}, nil, agent.AgentCard{}, agent.Accounting{})
	c.prov.erro = agent.Unavailability("fornecedor-x", agent.ReasonRejectedCredential, "cru")

	_, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err == nil {
		t.Fatal("esperava erro")
	}
	if k := errs.KindOf(err); k != errs.KindUnauthorized {
		t.Fatalf("Kind %q, esperava não autenticado — o classificador não está registrado", k)
	}
	if strings.Contains(err.Error(), "cru") {
		t.Fatalf("o detalhe cru do fornecedor vazou para a mensagem: %v", err)
	}
	// A pergunta do humano JÁ está na thread: se o fornecedor cai, a conversa
	// mostra o que foi perguntado em vez de um buraco.
	if len(c.conv.mensagens) != 1 {
		t.Fatalf("a pergunta não entrou antes da chamada ao modelo: %+v", c.conv.mensagens)
	}
}

func TestRunTurnRecusaRequisicaoIncompleta(t *testing.T) {
	c := montar(t, agent.ContextPackage{}, respostaConcluindo(), agent.AgentCard{}, agent.Accounting{})
	casos := map[string]agent.TurnRequest{
		"sem_texto":            {DemandID: "d", ThreadID: "t", TaskKind: "implementation"},
		"sem_tipo_de_trabalho": {DemandID: "d", ThreadID: "t", Text: "oi"},
		"sem_thread":           {DemandID: "d", Text: "oi", TaskKind: "implementation"},
	}
	for nome, req := range casos {
		t.Run(nome, func(t *testing.T) {
			if _, err := c.svc.RunTurn(contexto(), req, "turno-1"); err == nil {
				t.Fatal("esperava recusa")
			}
		})
	}
	// E sem conta ativa não há turno: isolamento multi-tenant é constraint.
	if _, err := c.svc.RunTurn(context.Background(), pedido(), "turno-1"); err == nil {
		t.Fatal("turno sem conta ativa deveria ser recusado")
	}
}

func contemAviso(avisos []string, trecho string) bool {
	for _, a := range avisos {
		if strings.Contains(a, trecho) {
			return true
		}
	}
	return false
}
