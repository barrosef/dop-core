package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ════════════════════════════════════════════════════════════════════════════
// O CICLO DO TURNO, e as decisões que não são óbvias.
//
// 0. O PROVEDOR PRIMEIRO. Credencial ausente é descoberta na primeira linha, não
//    depois de montar contexto e gastar duas leituras. O erro que chega ao
//    usuário fala de configuração, que é o que é.
//
// 1. CONTEXTO E THREAD. O pacote já vem cortado por orçamento de tokens
//    (ADR-0009 §3) e informa o DESCARTE — que não some: entra no prefixo (o
//    agente precisa saber que lê contexto parcial) e vira mensagem na thread (o
//    humano precisa saber por que a resposta ficou como ficou).
//
//    A versão do BFF fazia estas duas leituras em PARALELO, e com razão: eram
//    duas idas de rede, e somar as latências transformava a agregação num custo.
//    Aqui são duas chamadas em processo, e a concorrência compraria microssegundos
//    ao preço de uma goroutine, um canal e duas ordens possíveis de erro. Ficou
//    sequencial de propósito — é uma das seis idas e voltas de gRPC que a
//    ADR-0023 foi buscar.
//
// 2. O ROTEAMENTO É DO DOMÍNIO DE CUSTO, e a justificativa dele viaja inteira
//    (ADR-0011 §3). O que o runtime faz é a metade que a política não pode fazer:
//    traduzir a CLASSE para o nome concreto do fornecedor ATIVO — a mesma
//    separação política × catálogo de `cost/router.go`. Quando a ficha da thread
//    declara modelo (ADR-0010 §2), ela vence: ficha é o contrato congelado
//    daquela thread, e trocar o modelo dela no meio invalidaria o prefixo
//    cacheado de todos os turnos anteriores, porque cache é por modelo.
//
// 3. PREFIXO ESTÁVEL PRIMEIRO, VOLÁTIL DEPOIS. Ver prompt.go.
//
// 4. A MEDIÇÃO É IDEMPOTENTE, E A CHAVE É DERIVADA DO TURNO. Uma duplicata de
//    registro de consumo não colide com nada: entraria como gasto legítimo e o
//    orçamento viraria ficção. Todas as escritas deste ciclo derivam da MESMA
//    chave (`:msg-in`, `:notice`, `:usage`, `:msg-out`, `:finding`), de modo que
//    repetir a mesma requisição repete ZERO efeitos.
//
//    Diferença deliberada em relação à versão do BFF: lá, sem chave do cliente,
//    o runtime GERAVA uma — cada chamada virava um turno novo. Aqui a chave é
//    OBRIGATÓRIA. Do lado do núcleo, gerar seria transformar um retry de rede em
//    consumo em dobro e mensagem duplicada na thread, exatamente o que
//    `cost.Service.RecordUsage` recusa fazer ao exigir a chave. Quem sabe se está
//    retentando é o cliente, e agora ele precisa dizer.
//
// 5. TODA MENSAGEM É EVENTO (ADR-0006), e é assim que o cockpit fica sabendo: o
//    WatchDemand que já existe entrega os eventos sozinho. NÃO há um segundo
//    caminho de streaming aqui, de propósito — seria uma segunda fonte da verdade
//    para a mesma timeline.
//
// 6. A AUTORIA DA RESPOSTA É DO AGENTE. A pergunta é do humano que a escreveu; a
//    resposta é do agente que a produziu. O núcleo deriva autoria do
//    `ctxutil.Call`, então o ciclo TROCA o ator antes de publicar a resposta e o
//    achado. Gravar fala de agente como fala de humano faria o log de eventos —
//    que é a verdade da demanda (ADR-0006) — mentir sobre quem fez o quê, numa
//    plataforma cuja premissa inteira é distinguir os dois.
//
// 7. CONCLUIR EXIGE PUBLICAR ACHADO (spec §1). A recusa da conclusão vazia
//    acontece em turn.go; aqui só se publica o que passou.
//
// 8. ORÇAMENTO ESTOURADO PAUSA, NÃO MATA (ADR-0011 §2). O turno que já rodou é
//    entregue inteiro — a resposta é publicada e o achado também —, e o resultado
//    sai com `Paused` e o aviso que a caixa de atenção mostra. O próximo turno é
//    que não sai. Abortar aqui seria o corte duro que a ADR recusou, e ainda por
//    cima jogaria fora tokens já pagos.
// ════════════════════════════════════════════════════════════════════════════

// Service executa turnos de agente. Recebe apenas PORTAS.
type Service struct {
	providers Providers
	knowledge Knowledge
	routing   Routing
	conv      Conversation
}

// NewService recusa dependência nula.
//
// Panic é deliberado, e pela mesma razão de `identity.NewService`: isto é erro de
// MONTAGEM, e erro de montagem tem que aparecer no boot, não às três da manhã no
// primeiro turno que alguém tentar rodar. Aceitar nil e cair num fallback por
// dentro é o que transforma porta em enfeite.
//
// Repare no que NÃO está na lista: `ports.Clock`. Este serviço não carimba
// instante nenhum — quem grava mensagem, consumo e achado é o domínio dono de
// cada um, e cada um tem o próprio relógio. Mais que isso: o prefixo do prompt
// precisa ser livre de relógio (ADR-0012 §1, camada 2 de prompt.go), e um relógio
// disponível no serviço seria um convite permanente a carimbar o prefixo.
func NewService(providers Providers, knowledge Knowledge, routing Routing, conv Conversation) *Service {
	switch {
	case providers == nil:
		panic("agent.NewService: fábrica de provedores obrigatória — sem ela não há com quem conversar")
	case knowledge == nil:
		panic("agent.NewService: porta de conhecimento obrigatória — agente sem contexto é agente cego")
	case routing == nil:
		panic("agent.NewService: porta de custo obrigatória — turno sem medição é orçamento fictício")
	case conv == nil:
		panic("agent.NewService: porta de demanda obrigatória — resposta que não vira mensagem some")
	}
	return &Service{providers: providers, knowledge: knowledge, routing: routing, conv: conv}
}

// TurnRequest é um turno a executar numa thread.
type TurnRequest struct {
	DemandID string
	ThreadID string
	Text     string
	// TaskKind é vocabulário ABERTO: o roteador trata o desconhecido caindo no
	// caro e DIZ que caiu (ADR-0011 §3). Vazio é que não passa — sem tipo de
	// trabalho não há decisão a auditar.
	TaskKind string
	// ResourceID é o recurso de categoria `agent` (ADR-0013) que atende este
	// turno. Vazio = o provedor padrão da conta.
	ResourceID string
	// OperatorNote é a intervenção do OPERADOR, vinda da caixa de atenção.
	// Entra pelo canal de autoridade do fornecedor, nunca como texto de
	// usuário (ver D3).
	OperatorNote    string
	MaxOutputTokens int
}

// RoutingView é a decisão que valeu, com a justificativa INTEIRA.
type RoutingView struct {
	TaskKind      string
	Class         ModelClass
	Model         string
	Effort        Effort
	EffortApplied Effort
	Reason        string
	// FromAgentCard é verdadeiro quando a ficha da thread venceu o roteador.
	FromAgentCard bool
}

// TurnUsage são as quatro parcelas disjuntas, o custo e — o que mais importa —
// se cada número é CONHECIDO.
type TurnUsage struct {
	Usage
	CostMicros Micros
	Currency   string
	// CacheCreationKnown falso significa que o provedor não reporta criação de
	// cache (D1). Zero afirmaria que nada foi escrito, que é outra coisa.
	CacheCreationKnown bool
	// CostKnown falso significa que não há tabela de preço para este modelo. O
	// custo NÃO vira zero de consolo: um orçamento alimentado com zeros é a
	// ficção que a ADR-0011 §2 existe para impedir.
	CostKnown bool
}

// TurnOutcome é o resultado do turno.
type TurnOutcome struct {
	DemandID string
	ThreadID string
	Provider string
	Routing  RoutingView
	Reply    string
	// MessageIDs são as mensagens publicadas na thread, na ordem em que
	// entraram.
	MessageIDs       []string
	Concluded        bool
	Finding          *FindingRef
	Usage            TurnUsage
	ContextTruncated bool
	// Paused: o orçamento estourou e a demanda vira item de decisão (ADR-0011 §2).
	Paused   bool
	Notice   string
	Budgets  []BudgetView
	Warnings []string
}

// chaves deriva as chaves de idempotência das cinco escritas deste turno.
//
// Derivadas e não sorteadas: é o que faz reenviar a mesma requisição repetir ZERO
// efeitos — a mensagem não duplica, o consumo não conta duas vezes e o achado não
// é publicado de novo.
func chaves(turnKey string) map[string]string {
	m := make(map[string]string, 5)
	for _, alvo := range []string{"msg-in", "notice", "usage", "msg-out", "finding"} {
		m[alvo] = turnKey + ":" + alvo
	}
	return m
}

// comoAgente devolve o contexto com a autoria trocada para o AGENTE.
//
// A conta, o request-id e tudo mais seguem intactos: o que muda é QUEM fala. O
// ator é a thread — é o que `dop.v1.ActorRef` já documenta para agente ("id =
// thread_id do agente") — e o nome é a chave da thread, que é como o humano a vê
// no cockpit.
//
// Esta função é a linha mais importante deste arquivo do ponto de vista do
// produto. Sem ela, a resposta do agente entraria no log de eventos assinada por
// quem apertou o botão, e a plataforma perderia a única distinção que ela existe
// para manter.
func comoAgente(ctx context.Context, t Thread) context.Context {
	call, _ := ctxutil.From(ctx)
	call.ActorKind = ctxutil.ActorAgent
	call.ActorID = t.ID
	call.ActorName = t.Key
	return ctxutil.Into(ctx, call)
}

// RunTurn executa UM turno numa thread. Ver o ciclo no cabeçalho deste arquivo.
func (s *Service) RunTurn(ctx context.Context, req TurnRequest, idempotencyKey string) (*TurnOutcome, error) {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.DemandID) == "" || strings.TrimSpace(req.ThreadID) == "" {
		return nil, errs.Invalid("turno exige demanda e thread")
	}
	if strings.TrimSpace(req.Text) == "" {
		return nil, errs.Invalid("turno sem texto")
	}
	if strings.TrimSpace(req.TaskKind) == "" {
		return nil, errs.Invalid("turno sem tipo de trabalho: sem ele não há roteamento a auditar")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		// Ver a decisão 4 no cabeçalho: gerar aqui transformaria um retry de
		// rede em consumo em dobro e mensagem duplicada.
		return nil, errs.Invalid("turno exige chave de idempotência: as cinco escritas derivam dela")
	}
	ks := chaves(idempotencyKey)

	// 0. Provedor ANTES de qualquer outra coisa: credencial ausente falha aqui,
	// barato, e com mensagem que fala de configuração.
	provider, err := s.providers.For(ctx, req.ResourceID)
	if err != nil {
		return nil, err
	}
	info := provider.Info()

	// 1. Thread (ficha e chave) e pacote de contexto.
	thread, err := s.conv.Thread(ctx, req.DemandID, req.ThreadID)
	if err != nil {
		return nil, err
	}
	pkg, err := s.knowledge.ContextPackage(ctx, req.DemandID)
	if err != nil {
		return nil, err
	}

	// 2. Roteamento.
	decisao, err := s.routing.Route(ctx, req.TaskKind, req.DemandID)
	if err != nil {
		return nil, err
	}
	modelo, esforco, daFicha := modeloEEffort(info, decisao, thread.Card)

	// A pergunta entra na thread ANTES da chamada ao modelo: se o fornecedor
	// cair, a conversa mostra o que foi perguntado em vez de um buraco.
	var ids []string
	entrada, err := s.conv.PostMessage(ctx, thread.ID, req.Text, ks["msg-in"])
	if err != nil {
		return nil, err
	}
	ids = append(ids, entrada)

	aviso := TruncationNotice(pkg)
	if aviso != "" {
		// O truncamento não some: quem lê a thread precisa saber que a resposta
		// abaixo foi produzida sem parte do contexto.
		nota, err := s.conv.PostMessage(ctx, thread.ID, aviso, ks["notice"])
		if err != nil {
			return nil, err
		}
		ids = append(ids, nota)
	}

	// 3. Prefixo estável primeiro, volátil depois (ADR-0012 §1).
	turno := BuildTurn(pkg, thread.Key, thread.Card, req.Text, req.OperatorNote, req.MaxOutputTokens)
	execucao, err := executeTurn(ctx, provider, turno, modelo, esforco)
	if err != nil {
		return nil, err
	}
	resposta := execucao.ModelReply
	avisos := execucao.Warnings

	// 4. Medição, idempotente e com os campos de cache (ADR-0011 §4). Vem ANTES
	// de publicar a resposta: se o processo morrer no meio, é melhor ter
	// registrado tokens já pagos do que ter publicado uma resposta de graça.
	modeloEfetivo := resposta.Model
	if modeloEfetivo == "" {
		modeloEfetivo = modelo
	}
	preco, precoConhecido := info.PriceFor(modeloEfetivo)
	if !precoConhecido {
		avisos = append(avisos, fmt.Sprintf(
			"sem tabela de preço para %q em %q: o consumo foi registrado em tokens, "+
				"e o CUSTO ficou zerado por AUSÊNCIA de tabela — não por ser de graça",
			modeloEfetivo, info.Name))
	}
	var custo Micros
	if precoConhecido {
		custo = preco.CostMicros(resposta.Usage)
	}
	conta, err := s.routing.RecordUsage(ctx, Consumption{
		DemandID:            req.DemandID,
		ThreadID:            thread.ID,
		Model:               modeloEfetivo,
		InputTokens:         resposta.Usage.InputTokens,
		OutputTokens:        resposta.Usage.OutputTokens,
		CacheReadTokens:     resposta.Usage.CacheReadTokens,
		CacheCreationTokens: resposta.Usage.CacheCreationTokens,
		CostMicros:          custo,
		Currency:            preco.Currency,
	}, ks["usage"])
	if err != nil {
		return nil, err
	}

	// 5 e 6. A resposta na thread, assinada pelo AGENTE.
	comoAgenteCtx := comoAgente(ctx, thread)
	saida, err := s.conv.PostMessage(comoAgenteCtx, thread.ID, execucao.Reply, ks["msg-out"])
	if err != nil {
		return nil, err
	}
	ids = append(ids, saida)

	// 7. Concluir exige publicar achado (spec §1) — e o achado também é do
	// agente, pelo mesmo motivo da mensagem.
	var achadoRef *FindingRef
	if execucao.Concluded && execucao.Finding != nil {
		payload := map[string]any{}
		for k, v := range execucao.Finding.Payload {
			payload[k] = v
		}
		// A PROVENIÊNCIA entra no achado: quem auditar precisa saber com que
		// modelo e sob que política ele foi produzido, e o achado é durável —
		// vai para a memória do projeto e para o contexto dos irmãos.
		payload["provider"] = info.Name
		payload["model"] = modeloEfetivo
		payload["effort"] = string(resposta.EffortApplied)
		payload["routing_reason"] = decisao.Reason

		ref, err := s.conv.PublishFinding(comoAgenteCtx, req.DemandID, thread.ID,
			execucao.Finding.Title, payload, ks["finding"])
		if err != nil {
			return nil, err
		}
		achadoRef = &ref
	}

	return &TurnOutcome{
		DemandID: req.DemandID,
		ThreadID: thread.ID,
		Provider: info.Name,
		Routing: RoutingView{
			TaskKind:      decisao.TaskKind,
			Class:         decisao.Class,
			Model:         modeloEfetivo,
			Effort:        esforco,
			EffortApplied: resposta.EffortApplied,
			Reason:        decisao.Reason,
			FromAgentCard: daFicha,
		},
		Reply:      execucao.Reply,
		MessageIDs: ids,
		Concluded:  execucao.Concluded,
		Finding:    achadoRef,
		Usage: TurnUsage{
			Usage:              resposta.Usage,
			CostMicros:         custo,
			Currency:           preco.Currency,
			CacheCreationKnown: info.Supports(CapCacheCreationAccounting),
			CostKnown:          precoConhecido,
		},
		ContextTruncated: aviso != "",
		Paused:           conta.BudgetExceeded,
		Notice:           avisoDeOrcamento(conta),
		Budgets:          conta.Exceeded,
		Warnings:         avisos,
	}, nil
}

// modeloEEffort resolve (modelo concreto, effort, a ficha venceu?).
//
// A regra, em três degraus:
//
//  1. FICHA DA THREAD primeiro (ADR-0010 §2). O nome dela passa INTACTO: é um
//     nome do cardápio das integrações de agente, não uma classe, e traduzi-lo
//     seria desfazer a escolha congelada da thread;
//  2. senão, CLASSE → catálogo DESTE fornecedor. É a metade que a política não
//     pode fazer, porque a classe forte muda de nome a cada fornecedor;
//  3. senão, o nome que o roteador devolveu, INTACTO. Acontece quando a decisão
//     não trouxe classe — e adivinhar a classe de um nome desconhecido trocaria
//     em silêncio o modelo que a política escolheu, que é o defeito que o
//     falecido `catalog.py` carregava por desenho.
func modeloEEffort(info ProviderInfo, d Decision, card AgentCard) (string, Effort, bool) {
	daFicha := strings.TrimSpace(card.Model) != ""

	modelo := strings.TrimSpace(card.Model)
	switch {
	case daFicha:
	case d.Class == ClassCheap || d.Class == ClassMedium || d.Class == ClassStrong:
		modelo = info.ResolveModel(d.Class)
	default:
		modelo = d.Model
	}

	// O effort da ficha vence o do roteador quando ela declara um válido. Valor
	// fora do vocabulário cai no alto (ver NormalizeEffort), nunca no baixo.
	esforco := d.Effort
	if e := Effort(strings.ToLower(strings.TrimSpace(card.Effort))); ValidEffort(e) {
		esforco = e
	}
	return modelo, NormalizeEffort(esforco), daFicha
}

// avisoDeOrcamento redige o item que a caixa de atenção mostra.
//
// A frase é montada aqui, e não no domínio de custo, porque ela é sobre O TURNO:
// o que aconteceu, o que continua valendo e o que o humano precisa decidir. O
// domínio de custo responde com fatos (que escopos estouraram); traduzir fato em
// decisão é trabalho de quem conhece o fluxo.
func avisoDeOrcamento(a Accounting) string {
	if !a.BudgetExceeded {
		return ""
	}
	escopos := make([]string, 0, len(a.Exceeded))
	for _, b := range a.Exceeded {
		escopos = append(escopos, fmt.Sprintf("%s %s (%d de %d micros %s)",
			b.Scope, b.ScopeID, b.SpentMicros, b.LimitMicros, b.Currency))
	}
	return "Orçamento estourado em " + strings.Join(escopos, "; ") +
		". Este turno foi entregue inteiro; o próximo não sai até alguém decidir " +
		"(aumentar o teto, cortar escopo ou encerrar) — ADR-0011 §2."
}
