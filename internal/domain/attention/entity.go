// Package attention é a caixa de atenção: a fila única que responde "onde eu
// sou necessário, e em que ordem".
//
// É PROJEÇÃO (ADR-0006): todo item nasce de um evento e morre de outro. Este
// pacote não cria item — ele traduz evento em item e ORDENA. A tradução mora
// aqui, e não no adaptador, porque decidir o que merece atenção humana é regra
// de negócio, não detalhe de armazenamento.
package attention

import (
	"time"
)

// Kind é a natureza do item. É ela que determina o impacto na ordenação.
type Kind string

const (
	KindThreadBlocked     Kind = "thread_blocked"
	KindGatePending       Kind = "gate_pending"
	KindPRReview          Kind = "pr_review"
	KindMergeConflict     Kind = "merge_conflict"
	KindDirective         Kind = "directive"
	KindBudgetExceeded    Kind = "budget_exceeded"
	KindIntegrationBroken Kind = "integration_broken"
)

// Item é uma pendência que exige DECISÃO HUMANA.
//
// O que não exige decisão não entra: status e progresso ficam no cockpit. Caixa
// barulhenta vira ruído e é ignorada, e caixa ignorada não protege ninguém
// (risco R-1 da spec de conversação e atenção).
type Item struct {
	ID         string
	AccountID  string
	Kind       Kind
	TargetKind string // thread | stage | pull_request | directive | demand | resource
	TargetID   string
	DemandID   string // vazio em item de conta (integração quebrada)
	Title      string
	Summary    string
	OpenedAt   time.Time
	ResolvedAt *time.Time
	// EventID é o evento que ABRIU o item. Guardá-lo é o que torna a projeção
	// reconstruível e a reentrega inócua.
	EventID string
}

func (i Item) Open() bool { return i.ResolvedAt == nil }

// impacto é a tabela de urgência por tipo, e a ordem dela É a regra.
//
// Um lugar só, como o roteador de modelo: espalhar isso por ifs faria cada
// domínio decidir a própria urgência, e a fila deixaria de ter uma ordem só —
// que é exatamente o que a caixa existe para oferecer.
//
// A régua vem da spec: "prioridade por impacto (produção da fila de merge >
// pergunta exploratória) e idade". Traduzindo: o que trava ENTREGA vem antes do
// que trava UMA demanda, que vem antes do que trava UMA conversa.
var impacto = map[Kind]int{
	// Trava a entrega de todo mundo: a fila de merge é por repositório, e um
	// conflito escalado segura tudo que está atrás dele.
	KindMergeConflict: 10,
	// Trava a conta inteira: sem integração, nenhuma demanda anda.
	KindIntegrationBroken: 20,
	// Trava UMA demanda por inteiro.
	KindBudgetExceeded: 30,
	KindGatePending:    40,
	// Trabalho pronto esperando gente — custa dinheiro parado, mas não bloqueia
	// quem já está andando.
	KindPRReview: 50,
	// Coordenação: importante e nunca urgente. A demanda 1 segue até onde der;
	// ela NÃO para porque uma transversal foi identificada (ADR-0015).
	KindDirective: 60,
	// Uma conversa esperando resposta.
	KindThreadBlocked: 70,
}

// ImpactOf devolve o impacto do tipo. Tipo desconhecido cai no fim da fila, e
// não no começo: item que ninguém sabe classificar não pode empurrar para baixo
// um conflito de merge só porque é novo.
func ImpactOf(k Kind) int {
	if v, ok := impacto[k]; ok {
		return v
	}
	return 999
}

// Priority combina impacto e idade num número único, menor primeiro.
//
// A idade só desempata DENTRO do mesmo impacto — nunca atravessa faixas. Se
// atravessasse, uma pergunta exploratória de três dias passaria na frente de um
// conflito de produção de três minutos, que é precisamente a inversão que a
// spec proíbe.
func (i Item) Priority(agora time.Time) int32 {
	horas := int(agora.Sub(i.OpenedAt).Hours())
	if horas < 0 {
		horas = 0 // relógio andando para trás não vira prioridade máxima
	}
	// Teto de uma semana: passado disso a idade para de diferenciar, senão um
	// item esquecido há meses dominaria a faixa para sempre.
	if horas > 24*7 {
		horas = 24 * 7
	}
	// Impacto × 1000 reserva a casa da idade sem que ela invada a faixa acima.
	return int32(ImpactOf(i.Kind)*1000 + (24*7 - horas))
}

// Kinds ordenados por impacto — existe para o teste provar que a tabela cobre
// todos os tipos, e para a tela de calibração mostrar a régua.
func Kinds() []Kind {
	return []Kind{
		KindMergeConflict, KindIntegrationBroken, KindBudgetExceeded,
		KindGatePending, KindPRReview, KindDirective, KindThreadBlocked,
	}
}
