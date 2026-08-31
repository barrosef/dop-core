package attention

import "time"

// Este arquivo é o mapa entre o LOG e a CAIXA: quais eventos abrem item, quais
// fecham, e o que cada item diz.
//
// Ele é a fronteira do risco R-1 da spec ("caixa barulhenta vira ruído e é
// ignorada"). Cada linha aqui é uma decisão de que aquilo EXIGE decisão humana;
// tudo que não estiver listado é, por definição, coisa de cockpit e não de
// caixa. Acrescentar linha é fácil demais — a pergunta antes de acrescentar é
// "o dev precisa DECIDIR algo, ou só saber?".

// Tipos de evento que ABREM item.
const (
	EvThreadBlocked     = "dop.demand.thread.blocked"
	EvStageAdvanced     = "dop.demand.stage.advanced"
	EvPullRequestOpened = "dop.delivery.pull_request.opened"
	EvMergeConflict     = "dop.delivery.merge.conflict_escalated"
	EvDirectiveProposed = "dop.delivery.directive.proposed"
	EvBudgetExceeded    = "dop.cost.budget.exceeded"
)

// Tipos de evento que FECHAM item.
const (
	EvThreadResumed   = "dop.demand.thread.resumed"
	EvThreadConcluded = "dop.demand.thread.concluded"
	EvGateDecided     = "dop.demand.gate.decided"
	EvDirectiveDecide = "dop.delivery.directive.decided"
	EvMergeState      = "dop.delivery.merge.state_changed"
	EvBudgetSet       = "dop.cost.budget.set"
)

// Subjects é o que o consumidor assina. Assinar `dop.>` e descartar 90% seria
// desperdício de entrega; assinar demais também é ruído, só que de rede.
func Subjects() []string {
	return []string{"dop.demand.>", "dop.delivery.>", "dop.cost.>"}
}

// Event é o mínimo que a regra precisa saber do evento. Existe para que este
// pacote não importe nem o adaptador nem o envelope do barramento.
type Event struct {
	ID          string
	AccountID   string
	Aggregate   string
	AggregateID string
	Type        string
	OccurredAt  time.Time
	Payload     map[string]any
}

// Decision é o que a regra devolve: abrir um item, fechar os itens de um alvo,
// ou ignorar o evento.
type Decision struct {
	Open  *Item
	Close *CloseSpec
}

// CloseSpec fecha por ALVO, não por id de item: quem destrava a thread não sabe
// (nem deveria saber) qual item a caixa criou para ela.
type CloseSpec struct {
	Kind       Kind
	TargetKind string
	TargetID   string
}

// Apply traduz um evento em decisão. Devolve zero-value quando o evento não
// interessa à caixa — que é o caso da esmagadora maioria deles.
func Apply(e Event) Decision {
	switch e.Type {

	case EvThreadBlocked:
		return abrir(e, KindThreadBlocked, "thread", threadID(e),
			str(e.Payload, "question", "Um agente precisa de resposta"),
			str(e.Payload, "detail", ""))

	case EvStageAdvanced:
		// Só entra quando a etapa PAROU num portão humano. Etapa avançando é
		// progresso, e progresso é cockpit — se toda transição virasse item, a
		// caixa encheria de coisa que ninguém precisa decidir.
		//
		// O campo é `to`, o estado PARA ONDE a etapa foi. A primeira versão
		// lia `status`, que o evento nunca teve: a caixa só sabia FECHAR um
		// item que nunca abria, e o teste de unidade não pegou porque fabricava
		// o evento com o formato suposto em vez do formato emitido. É o teste
		// de integração no fim deste domínio que fecha esse buraco.
		if str(e.Payload, "to", "") != "blocked" {
			return Decision{}
		}
		if str(e.Payload, "gate", "") == "none" {
			return Decision{}
		}
		return abrir(e, KindGatePending, "stage", str(e.Payload, "stage_key", ""),
			"Etapa aguardando decisão: "+str(e.Payload, "stage_key", ""),
			str(e.Payload, "reason", ""))

	case EvPullRequestOpened:
		return abrir(e, KindPRReview, "pull_request", str(e.Payload, "pull_request_id", e.AggregateID),
			"PR aguardando revisão", str(e.Payload, "title", ""))

	case EvMergeConflict:
		return abrir(e, KindMergeConflict, "pull_request", str(e.Payload, "pull_request_id", e.AggregateID),
			"Conflito escalado na fila de merge", str(e.Payload, "detail", ""))

	case EvDirectiveProposed:
		return abrir(e, KindDirective, "directive", str(e.Payload, "directive_id", e.AggregateID),
			"Transversal detectada — decisão de coordenação",
			str(e.Payload, "recommendation", ""))

	case EvBudgetExceeded:
		return abrir(e, KindBudgetExceeded, "demand", demandID(e),
			"Orçamento estourado — demanda pausada",
			str(e.Payload, "scope", ""))

	// ── fechamento ──────────────────────────────────────────────────────────

	case EvThreadResumed, EvThreadConcluded:
		return fechar(KindThreadBlocked, "thread", threadID(e))

	case EvGateDecided:
		return fechar(KindGatePending, "stage", str(e.Payload, "stage_key", ""))

	case EvDirectiveDecide:
		return fechar(KindDirective, "directive", str(e.Payload, "directive_id", e.AggregateID))

	case EvMergeState:
		// Só fecha quando o conflito deixou de existir. Mudança de estado para
		// "rebasing" não resolve conflito nenhum.
		if s := str(e.Payload, "state", ""); s != "merged" && s != "cancelled" {
			return Decision{}
		}
		return fechar(KindMergeConflict, "pull_request",
			str(e.Payload, "pull_request_id", e.AggregateID))

	case EvBudgetSet:
		// Teto novo pode ter destravado a demanda. Fechar aqui e deixar o
		// próximo estouro reabrir é mais honesto do que manter item de um
		// bloqueio que talvez não exista mais.
		return fechar(KindBudgetExceeded, "demand", demandID(e))
	}

	return Decision{}
}

func abrir(e Event, k Kind, targetKind, targetID, title, summary string) Decision {
	if targetID == "" {
		// Item sem alvo é item que não dá para clicar — e item que não leva a
		// lugar nenhum é pior que item ausente.
		return Decision{}
	}
	return Decision{Open: &Item{
		AccountID:  e.AccountID,
		Kind:       k,
		TargetKind: targetKind,
		TargetID:   targetID,
		DemandID:   demandID(e),
		Title:      title,
		Summary:    summary,
		OpenedAt:   e.OccurredAt,
		EventID:    e.ID,
	}}
}

func fechar(k Kind, targetKind, targetID string) Decision {
	if targetID == "" {
		return Decision{}
	}
	return Decision{Close: &CloseSpec{Kind: k, TargetKind: targetKind, TargetID: targetID}}
}

// demandID: os eventos da demanda têm agregado "demand"; os de entrega e custo
// carregam o id no payload.
func demandID(e Event) string {
	if e.Aggregate == "demand" {
		return e.AggregateID
	}
	return str(e.Payload, "demand_id", "")
}

func threadID(e Event) string { return str(e.Payload, "thread_id", "") }

func str(p map[string]any, chave, padrao string) string {
	if v, ok := p[chave].(string); ok && v != "" {
		return v
	}
	return padrao
}
