package app

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
	"github.com/Digital-Business-One/dop-core/internal/domain/cost"
	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Cola entre o runtime de agente e os três domínios que ele usa.
//
// Mesma disciplina de glue.go, e mora em arquivo próprio só para não misturar a
// cola nova com a que já existia: cada domínio declara a porta ESTREITA do que
// precisa do vizinho, em vez de importar o pacote dele. O preço é este arquivo; o
// que se compra é que `agent` não sabe que `knowledge`, `cost` e `demand` existem.
//
// É deliberado que a cola seja chata e mecânica: no dia em que uma destas funções
// precisar de um `if` de regra, a regra está no domínio errado.

// ── knowledge → agent ───────────────────────────────────────────────────────

// agentKnowledge entrega ao runtime a bagagem de bordo da demanda.
//
// O orçamento vai ZERADO de propósito: `BuildContextPackage` lê zero como "use o
// teto do serviço" (ver knowledge.Budget.Normalize). Escolher um teto aqui
// colocaria política de custo de contexto no composition root, longe do lugar
// onde ela é decidida e medida.
type agentKnowledge struct{ k *knowledge.Service }

var _ agent.Knowledge = agentKnowledge{}

func (a agentKnowledge) ContextPackage(ctx context.Context, demandID string) (agent.ContextPackage, error) {
	pkg, err := a.k.BuildContextPackage(ctx, demandID, knowledge.Budget{})
	if err != nil {
		return agent.ContextPackage{}, err
	}
	return agent.ContextPackage{
		Rules:    pkg.Rules,
		Index:    artefatosDoPacote(pkg.Index),
		Memories: artefatosDoPacote(pkg.Memories),
		Findings: achadosDoPacote(pkg.Findings),
		Dropped: agent.ContextDropped{
			Rules:    pkg.Dropped.Rules,
			Findings: pkg.Dropped.Findings,
			Index:    pkg.Dropped.Index,
			Memories: pkg.Dropped.Memories,
		},
	}, nil
}

// artefatosDoPacote converte preservando a ORDEM da curadoria: ela é a
// prioridade do `SelectPackage` (ADR-0009 §3), e reordenar aqui desfaria a
// seleção que consumiu o orçamento inteiro.
//
// `ID` e `Version` ficam para trás porque a porta do runtime nem os tem: eles
// mudam quando o núcleo regrava o artefato sem que o conteúdo mude, e entrariam
// no prefixo cacheado para invalidá-lo à toa (ADR-0012 §1).
func artefatosDoPacote(itens []knowledge.Artifact) []agent.ContextArtifact {
	out := make([]agent.ContextArtifact, 0, len(itens))
	for _, a := range itens {
		out = append(out, agent.ContextArtifact{
			Name: a.Name, Body: a.Body, ObjectRef: a.ObjectRef,
		})
	}
	return out
}

func achadosDoPacote(itens []knowledge.Finding) []agent.ContextFinding {
	out := make([]agent.ContextFinding, 0, len(itens))
	for _, f := range itens {
		out = append(out, agent.ContextFinding{Title: f.Title, Summary: f.Summary})
	}
	return out
}

// ── cost → agent ────────────────────────────────────────────────────────────

// agentRouting liga a decisão e a medição do domínio de custo.
//
// A CLASSE atravessa aqui — é o campo que a fronteira de rede comia quando o
// runtime vivia no BFF, e é ele que aposenta a tabela de tradução inversa que
// existia lá (ADR-0023).
type agentRouting struct{ c *cost.Service }

var _ agent.Routing = agentRouting{}

func (a agentRouting) Route(ctx context.Context, taskKind, demandID string) (agent.Decision, error) {
	d, err := a.c.RouteModel(ctx, cost.TaskKind(taskKind), demandID)
	if err != nil {
		return agent.Decision{}, err
	}
	// Os vocabulários batem STRING A STRING (classe e effort). A conversão é
	// troca de tipo nomeado, não tradução; se um dia divergirem, é aqui que
	// quebra — e quebrar aqui é melhor do que rotear em silêncio para o modelo
	// errado.
	return agent.Decision{
		TaskKind: string(d.TaskKind),
		Class:    agent.ModelClass(d.Class),
		Model:    d.Model,
		Effort:   agent.Effort(d.Effort),
		Reason:   d.Reason,
	}, nil
}

func (a agentRouting) RecordUsage(ctx context.Context, c agent.Consumption, idemKey string) (agent.Accounting, error) {
	out, err := a.c.RecordUsage(ctx, cost.UsageEvent{
		DemandID:            c.DemandID,
		ThreadID:            c.ThreadID,
		Model:               c.Model,
		InputTokens:         c.InputTokens,
		OutputTokens:        c.OutputTokens,
		CacheReadTokens:     c.CacheReadTokens,
		CacheCreationTokens: c.CacheCreationTokens,
		CostMicros:          cost.Micros(c.CostMicros),
		Currency:            c.Currency,
		// AccountID e At ficam de fora: o serviço de custo os toma do contexto
		// e do relógio dele. Preenchê-los aqui permitiria lançar consumo na
		// conta do vizinho e gravar na partição errada.
	}, idemKey)
	if err != nil {
		return agent.Accounting{}, err
	}
	estourados := make([]agent.BudgetView, 0, len(out.Exceeded))
	for _, b := range out.Exceeded {
		estourados = append(estourados, agent.BudgetView{
			Scope:       string(b.Scope),
			ScopeID:     b.ScopeID,
			LimitMicros: agent.Micros(b.LimitMicros),
			SpentMicros: agent.Micros(b.SpentMicros),
			Currency:    b.Currency,
		})
	}
	return agent.Accounting{BudgetExceeded: out.BudgetExceeded, Exceeded: estourados}, nil
}

// ── demand → agent ──────────────────────────────────────────────────────────

// agentConversation liga a thread, a mensagem e o achado.
//
// Repare no que NÃO passa por aqui: a AUTORIA. `demand.Service` a lê do
// `ctxutil.Call`, e é o runtime que troca o ator para o agente antes de publicar
// a resposta. Se a autoria fosse parâmetro desta cola, ela viraria algo que se
// pode esquecer de passar — e a fala do agente entraria no log como fala de
// humano, que é a única coisa que este sistema não pode confundir.
type agentConversation struct{ d *demand.Service }

var _ agent.Conversation = agentConversation{}

func (a agentConversation) Thread(ctx context.Context, demandID, threadID string) (agent.Thread, error) {
	threads, err := a.d.ListThreads(ctx, demandID)
	if err != nil {
		return agent.Thread{}, err
	}
	for _, t := range threads {
		if t.ID == threadID {
			return agent.Thread{
				ID:  t.ID,
				Key: t.Key,
				Card: agent.AgentCard{
					Purpose:      t.Card.Purpose,
					Tools:        t.Card.Tools,
					Model:        t.Card.Model,
					Effort:       t.Card.Effort,
					BudgetMicros: t.Card.BudgetMicros,
				},
			}, nil
		}
	}
	// Thread inexistente e thread de OUTRA demanda saem como o MESMO erro:
	// `ListThreads` já filtra por conta e por demanda, e distinguir os dois
	// casos vazaria a existência de ids alheios para quem ficasse tentando.
	return agent.Thread{}, errs.NotFound("thread %s nesta demanda", threadID)
}

func (a agentConversation) PostMessage(ctx context.Context, threadID, text, idemKey string) (string, error) {
	m, err := a.d.PostMessage(ctx, threadID, text, idemKey)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (a agentConversation) PublishFinding(ctx context.Context, demandID, threadID, title string,
	payload map[string]any, idemKey string) (agent.FindingRef, error) {
	f, err := a.d.PublishFinding(ctx, demandID, threadID, title, payload, idemKey)
	if err != nil {
		return agent.FindingRef{}, err
	}
	return agent.FindingRef{ID: f.ID, Title: f.Title}, nil
}
