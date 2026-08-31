package cost

import (
	"fmt"
	"strings"
)

// ════════════════════════════════════════════════════════════════════════════
// ModelRouter — a decisão tarefa → (modelo, effort) da ADR-0011 §3.
//
// ESTA É A PARTE EM RASCUNHO da ADR. A tabela abaixo é palpite informado, não
// resultado de medição, e o código diz isso em três lugares de propósito: no
// nome da constante de proveniência, na justificativa que sai em toda decisão,
// e neste comentário.
//
// COMO RECALIBRAR (pendência P-7 do ROADMAP):
//
//	1. a telemetria vem dos eventos de custo — cost_usage tem model,
//	   input/output e os dois campos de cache por chamada, e SummarizeCost
//	   agrega por escopo e período;
//	2. mude LINHAS de `routingTable` e/ou o `ModelCatalog`. Nada mais. Se um
//	   dia a decisão precisar de um `if` fora daqui, a política deixou de ser
//	   tabela e a mudança é de desenho, não de calibração;
//	3. atualize `RoutingProvenance` para deixar de dizer "rascunho" — a
//	   justificativa que chega ao auditor muda junto, sem tocar em mais nada.
//
// Por que tabela e não heurística espalhada: uma política que nasce errada
// precisa ser AUDITÁVEL e SUBSTITUÍVEL de uma vez só. Quinze `if` distribuídos
// por chamadores dão a mesma resposta hoje e são impossíveis de recalibrar
// amanhã — e ninguém consegue explicar por que um agente rodou no modelo caro.
// ════════════════════════════════════════════════════════════════════════════

// TaskKind é a natureza do trabalho. É o ÚNICO eixo da decisão — a ADR-0011
// descartou explicitamente rotear por tamanho de prompt: o que importa é a
// natureza da tarefa, não o comprimento dela.
type TaskKind string

const (
	// TaskMechanical: commit, resumo de log, dossiê, i18n. Trabalho de forma,
	// não de raciocínio.
	TaskMechanical TaskKind = "mechanical"
	// TaskInvestigation: subagente lendo logs e fazendo forense (ADR-0010).
	TaskInvestigation TaskKind = "investigation"
	// TaskImplementation: planejar e escrever o código.
	TaskImplementation TaskKind = "implementation"
	// TaskCritic: o parecer da ADR-0007. É o freio do fluxo.
	TaskCritic TaskKind = "critic"
)

// ModelClass é CLASSE, não nome de modelo. Nome de modelo muda de líder por
// semestre (ADR-0001) e não sobrevive a uma tabela de política; a classe é o
// que a decisão realmente significa. O nome concreto vem do catálogo.
type ModelClass string

const (
	ClassCheap  ModelClass = "cheap"  // classe Haiku
	ClassMedium ModelClass = "medium" // classe Sonnet
	ClassStrong ModelClass = "strong" // classe Opus
)

// Effort é o esforço de raciocínio. Reduz preâmbulo e tool calls DENTRO da
// classe — é o segundo eixo, e o mais barato de ajustar: mexer no effort não
// troca de modelo, só encurta o caminho até a resposta.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// RoutingProvenance entra em TODA justificativa. Existe para que quem lê uma
// decisão no log de auditoria saiba de onde ela veio sem abrir o código — e
// para que a frase "isto ainda não foi medido" seja impossível de esquecer.
const RoutingProvenance = "ADR-0011 §3 (rascunho — calibrar com telemetria, P-7)"

// routingRule é uma LINHA da tabela de decisão.
type routingRule struct {
	Kind   TaskKind
	Class  ModelClass
	Effort Effort
	// Why é o porquê da linha, não a repetição do que ela faz. Sem isto a
	// decisão não é auditável: ninguém consegue discordar de "haiku/low", mas
	// qualquer um consegue discordar de "trabalho de forma não paga raciocínio".
	Why string
}

// routingTable — A TABELA. Espelha a ADR-0011 §3 linha a linha.
//
// Slice e não map: a ordem é a da ADR, e ler o código lado a lado com o
// documento tem de ser possível sem esforço. São quatro linhas; se um dia
// forem quarenta, o problema é a política, não a estrutura de dados.
var routingTable = []routingRule{
	{
		Kind: TaskMechanical, Class: ClassCheap, Effort: EffortLow,
		Why: "trabalho de forma (commit, resumo de log, dossiê, i18n) não paga " +
			"raciocínio longo; o spread entre classes é ~5×",
	},
	{
		Kind: TaskInvestigation, Class: ClassMedium, Effort: EffortMedium,
		Why: "forense de subagente lê muito e conclui pouco; a classe média " +
			"sustenta o volume sem o preço da classe forte, e o achado é o que " +
			"volta ao principal (ADR-0010)",
	},
	{
		Kind: TaskImplementation, Class: ClassStrong, Effort: EffortHigh,
		Why: "planejar e implementar é onde o erro custa retrabalho; economizar " +
			"aqui devolve a economia em revisão",
	},
	{
		Kind: TaskCritic, Class: ClassStrong, Effort: EffortMax,
		// Esta linha é a REGRA FIXA da ADR-0011/0012, não um ponto de
		// calibração: recalibrar as outras três é esperado, rebaixar esta não.
		Why: "não se economiza no crítico — é o freio (ADR-0007); economizar no " +
			"freio devolve o custo em PR reprovado, o retrabalho mais caro do fluxo",
	},
}

// fallbackRule atende tipo de trabalho fora do vocabulário.
//
// Cai para o lado CARO de propósito: um tipo desconhecido é, por definição,
// trabalho que ninguém classificou, e errar para barato num trabalho que
// exigia raciocínio custa retrabalho — enquanto errar para caro custa dinheiro
// e aparece na medição, que é justamente o que existe aqui.
var fallbackRule = routingRule{
	Class: ClassStrong, Effort: EffortHigh,
	Why: "tipo de trabalho fora do vocabulário: na dúvida não se economiza, e o " +
		"gasto aparece na medição — o inverso não aparece",
}

// ModelCatalog resolve CLASSE → nome concreto de modelo.
//
// Separado da tabela porque as duas coisas mudam por motivos diferentes e em
// ritmos diferentes: a política (que classe para que trabalho) muda com
// telemetria; o catálogo (que modelo é a classe forte hoje) muda quando o
// fornecedor lança modelo novo. Amarrar os dois obrigaria a revisar a política
// a cada lançamento.
type ModelCatalog map[ModelClass]string

// DefaultCatalog é o ponto de partida. Nomes ilustrativos e substituíveis: o
// composition root pode passar outro catálogo sem tocar na política.
func DefaultCatalog() ModelCatalog {
	return ModelCatalog{
		ClassCheap:  "claude-haiku",
		ClassMedium: "claude-sonnet",
		ClassStrong: "claude-opus",
	}
}

// Decision é a decisão devolvida — e ela CARREGA O PORQUÊ.
//
// Reason não é enfeite: sem ela ninguém audita ("por que esta demanda rodou no
// modelo caro?") nem calibra ("esta linha da tabela está errada por quê?").
// Uma decisão sem justificativa é um número que não se pode contestar.
type Decision struct {
	TaskKind TaskKind
	Class    ModelClass
	Model    string
	Effort   Effort
	Reason   string
}

// Router aplica a tabela. Sem estado, sem I/O, sem relógio: a mesma entrada dá
// sempre a mesma saída, e é isso que torna a política testável e auditável.
type Router struct {
	catalog ModelCatalog
	table   []routingRule
}

// NewRouter aceita catálogo nulo e cai no padrão — ao contrário do relógio,
// que é PORTA e recusa nil. A diferença é real: relógio nulo desliga uma
// abstração em silêncio e leva o teste de volta ao relógio de parede; catálogo
// nulo apenas usa a política de rascunho que a ADR já escreveu.
func NewRouter(catalog ModelCatalog) *Router {
	if len(catalog) == 0 {
		catalog = DefaultCatalog()
	}
	return &Router{catalog: catalog, table: routingTable}
}

// Route é a decisão. Erra apenas quando não há o que decidir: tipo VAZIO é
// requisição incompleta, enquanto tipo desconhecido é vocabulário novo — e
// vocabulário novo não pode parar trabalho em andamento, cai no fallback e diz
// que caiu.
func (r *Router) Route(kind TaskKind) (Decision, error) {
	k := TaskKind(strings.ToLower(strings.TrimSpace(string(kind))))
	if k == "" {
		return Decision{}, fmt.Errorf("tipo de trabalho não informado")
	}

	rule := fallbackRule
	rule.Kind = k
	for _, cand := range r.table {
		if cand.Kind == k {
			rule = cand
			break
		}
	}

	model, ok := r.catalog[rule.Class]
	if !ok {
		// Catálogo incompleto é erro de montagem, mas não pode virar trabalho
		// parado: devolve a classe como nome e denuncia na justificativa.
		model = string(rule.Class)
	}

	return Decision{
		TaskKind: k,
		Class:    rule.Class,
		Model:    model,
		Effort:   rule.Effort,
		Reason:   fmt.Sprintf("%s: %s", RoutingProvenance, rule.Why),
	}, nil
}

// Table devolve uma cópia legível da política, para auditoria e para a tela de
// calibração de P-7. Cópia, e não a tabela: política que o chamador consegue
// editar em memória deixa de ser política.
func (r *Router) Table() []Decision {
	out := make([]Decision, 0, len(r.table))
	for _, rule := range r.table {
		d, err := r.Route(rule.Kind)
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out
}
