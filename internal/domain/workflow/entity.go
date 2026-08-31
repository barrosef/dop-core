// Package workflow é o domínio do fluxo de trabalho: a sequência tipada de
// etapas que uma demanda percorre, e a cadeia de herança que decide QUAL
// sequência vale para cada demanda (ADR-0014).
//
// Regra da casa: este pacote não conhece Postgres, gRPC nem SDK nenhum. Ele
// declara o que precisa como PORTA (repository.go) e o composition root liga.
//
// Duas invariantes sustentam tudo o que está aqui:
//
//  1. VERSÃO É IMUTÁVEL. Atualizar um fluxo GERA versão nova; a anterior fica
//     como está, para sempre. Uma demanda em andamento aponta para a versão que
//     congelou ao iniciar (ADR-0014 §4) — alterar a versão no lugar reescreveria
//     o passado dela, e o histórico deixaria de explicar o que aconteceu.
//  2. RESOLVER É SOBREPOR, COM PROCEDÊNCIA. O fluxo efetivo de uma demanda é a
//     sobreposição da cadeia plataforma ◁ conta ◁ workspace ◁ projeto ◁ demanda.
//     Guardar só o resultado seria jogar fora a única informação que responde
//     "por que esta demanda seguiu um fluxo que ninguém lembra de ter escrito".
package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── escopos da cadeia ────────────────────────────────────────────────────────

// Scope é um nível da cadeia de herança (ADR-0014 §3).
type Scope string

const (
	ScopePlatform  Scope = "platform"
	ScopeAccount   Scope = "account"
	ScopeWorkspace Scope = "workspace"
	ScopeProject   Scope = "project"
	ScopeDemand    Scope = "demand"
)

// Chain é a cadeia inteira, do MAIS GENÉRICO ao MAIS ESPECÍFICO. A ordem desta
// fatia é a definição da herança: quem vem depois sobrepõe quem veio antes.
var Chain = []Scope{ScopePlatform, ScopeAccount, ScopeWorkspace, ScopeProject, ScopeDemand}

// Rank é a posição na cadeia; -1 para escopo desconhecido. É o que permite
// dizer "acima" e "abaixo" sem espalhar switch por todo lado.
func Rank(s Scope) int {
	for i, c := range Chain {
		if c == s {
			return i
		}
	}
	return -1
}

func ValidScope(s Scope) bool { return Rank(s) >= 0 }

// Label é o rótulo que aparece no rastro da tela. O usuário lê "projeto ◂
// workspace ◂ conta", não "project ◂ workspace ◂ account".
func (s Scope) Label() string {
	switch s {
	case ScopePlatform:
		return "plataforma"
	case ScopeAccount:
		return "conta"
	case ScopeWorkspace:
		return "workspace"
	case ScopeProject:
		return "projeto"
	case ScopeDemand:
		return "demanda"
	}
	return string(s)
}

// ScopeRef endereça UM nível concreto da cadeia. ID vazio só é legítimo no
// nível plataforma, que é único por definição.
type ScopeRef struct {
	Scope Scope
	ID    string
}

func (r ScopeRef) String() string {
	if r.ID == "" {
		return r.Scope.Label()
	}
	return r.Scope.Label() + " " + r.ID
}

// ── vocabulário das etapas ───────────────────────────────────────────────────

// StageType é o tipo semântico da etapa. O vocabulário é da PLATAFORMA: o tipo
// decide o renderizador na tela e o comportamento do agente (que artefato
// produzir, onde parar). Tipo novo exige evolução da plataforma; composição
// nova, não (ADR-0014 §1) — e é por isso que tipo desconhecido é erro de
// contrato, não dado do usuário.
type StageType string

const (
	TypeContext         StageType = "context"
	TypeSpec            StageType = "spec"
	TypePlan            StageType = "plan"
	TypeImplementation  StageType = "implementation"
	TypeTest            StageType = "test"
	TypeHumanValidation StageType = "human_validation"
	TypeFinalization    StageType = "finalization"
	TypeGeneric         StageType = "generic"
)

func ValidStageType(t StageType) bool {
	switch t {
	case TypeContext, TypeSpec, TypePlan, TypeImplementation,
		TypeTest, TypeHumanValidation, TypeFinalization, TypeGeneric:
		return true
	}
	return false
}

// ArtifactKind é o que a etapa põe na mesa: produzido pelo agente ou assinado
// pelo humano. É o que a tela renderiza quando a etapa abre.
type ArtifactKind string

const (
	ArtifactDocument ArtifactKind = "document"
	ArtifactSpec     ArtifactKind = "spec"
	ArtifactPlan     ArtifactKind = "plan"
	ArtifactTestPlan ArtifactKind = "test_plan"
	ArtifactDiagram  ArtifactKind = "diagram"
	ArtifactReport   ArtifactKind = "report"
)

func ValidArtifactKind(a ArtifactKind) bool {
	switch a {
	case ArtifactDocument, ArtifactSpec, ArtifactPlan,
		ArtifactTestPlan, ArtifactDiagram, ArtifactReport:
		return true
	}
	return false
}

// GateKind diz se a etapa PARA para alguém decidir. São dois valores e de
// propósito: portão com regra condicional é motor de workflow, explicitamente
// fora da v1 (ADR-0014, alternativas consideradas).
type GateKind string

const (
	GateNone  GateKind = "none"
	GateHuman GateKind = "human"
)

func ValidGate(g GateKind) bool { return g == GateNone || g == GateHuman }

// StageSpec é uma etapa. Sem condicional, sem paralelismo, sem DSL: a v1 é
// deliberadamente uma sequência (ADR-0014 §2).
type StageSpec struct {
	// Key é o endereço da etapa. O avanço da demanda é EVENTO, e o evento
	// aponta para a etapa pela chave — por isso ela é obrigatória e única.
	Key       string
	Name      string // livre, do autor
	Type      StageType
	Artifacts []ArtifactKind
	Gate      GateKind
	Subtypes  []string // ex.: teste → aaa, e2e, integração
}

// Flow é uma versão CONGELADA de um fluxo num nível da cadeia.
//
// Version não é metadado: é parte da identidade. Duas versões do mesmo ID são
// dois documentos diferentes, e a demanda que apontou para a versão 3 continua
// vendo a versão 3 depois de a 4 nascer.
type Flow struct {
	ID          string
	AccountID   string // vazio APENAS no catálogo da plataforma, que não tem dono
	OwnerScope  Scope
	OwnerID     string // vazio no nível plataforma
	Name        string
	Description string
	Version     int32
	Stages      []StageSpec
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (f Flow) Ref() ScopeRef { return ScopeRef{Scope: f.OwnerScope, ID: f.OwnerID} }

// Normalize apara o que veio do cliente e aplica o único default que existe:
// etapa sem portão declarado não para. Exigir "none" explícito em toda etapa
// seria ruído — o silêncio já diz o que se espera.
func (f *Flow) Normalize() {
	f.Name = strings.TrimSpace(f.Name)
	f.Description = strings.TrimSpace(f.Description)
	for i := range f.Stages {
		f.Stages[i].Key = strings.TrimSpace(f.Stages[i].Key)
		f.Stages[i].Name = strings.TrimSpace(f.Stages[i].Name)
		if f.Stages[i].Gate == "" {
			f.Stages[i].Gate = GateNone
		}
		if f.Stages[i].Name == "" {
			f.Stages[i].Name = f.Stages[i].Key
		}
	}
}

// SameStages compara o conteúdo executável de dois fluxos. É o que separa uma
// atualização de verdade de um reenvio — e reenvio não pode gerar versão, senão
// um cliente com retry automático versionaria o fluxo para sempre.
func (f Flow) SameStages(other Flow) bool {
	if f.Name != other.Name || f.Description != other.Description ||
		len(f.Stages) != len(other.Stages) {
		return false
	}
	for i := range f.Stages {
		a, b := f.Stages[i], other.Stages[i]
		if a.Key != b.Key || a.Name != b.Name || a.Type != b.Type || a.Gate != b.Gate ||
			!sameArtifacts(a.Artifacts, b.Artifacts) || !sameStrings(a.Subtypes, b.Subtypes) {
			return false
		}
	}
	return true
}

// ── fluxo efetivo ────────────────────────────────────────────────────────────

// StageOrigin é a procedência de UMA etapa: de qual nível da cadeia veio a
// versão dela que sobreviveu à sobreposição.
type StageOrigin struct {
	Key  string
	From ScopeRef
}

// EffectiveFlow é o resultado da cadeia — e o rastro de como se chegou nele.
//
// Contributors traz os níveis que de fato declararam algo, do mais específico
// ao mais genérico; Origins responde etapa por etapa. Sem esses dois campos o
// suporte não tem como explicar um fluxo que ninguém lembra de ter escrito
// (ADR-0014, consequências).
type EffectiveFlow struct {
	Flow         Flow
	Contributors []ScopeRef
	Origins      []StageOrigin
	ResolvedFrom string
}

// OriginOf devolve de onde veio a etapa.
func (e EffectiveFlow) OriginOf(key string) (ScopeRef, bool) {
	for _, o := range e.Origins {
		if o.Key == key {
			return o.From, true
		}
	}
	return ScopeRef{}, false
}

// MergeChain sobrepõe os fluxos da cadeia. levels vem do MAIS GENÉRICO ao MAIS
// ESPECÍFICO — a ordem é o contrato desta função.
//
// As regras, inteiras:
//
//   - nível que não declara etapa nenhuma HERDA por omissão (é ignorado aqui);
//   - etapa com chave já vista é SUBSTITUÍDA — o nível mais específico vence;
//   - etapa com chave nova entra no FIM;
//   - a posição é de quem INTRODUZIU a etapa. Reordenar o que se herdou não é
//     expressível na v1: quem precisa de outra ordem declara o fluxo inteiro
//     com as suas próprias chaves. É a mesma escolha da ADR-0014 §2 — a
//     estrutura fica simples e o que falta evolui sobre ela, em vez de nascer
//     como um merge de ordenação que ninguém consegue prever de cabeça.
func MergeChain(levels []Flow) EffectiveFlow {
	var (
		eff     EffectiveFlow
		stages  []StageSpec
		posOf   = map[string]int{}
		fromOf  = map[string]ScopeRef{}
		sources []ScopeRef
	)
	for _, lv := range levels {
		if len(lv.Stages) == 0 {
			continue // herda por omissão
		}
		ref := lv.Ref()
		sources = append(sources, ref)

		// A identidade do fluxo efetivo é a do nível mais específico que
		// declarou: é o documento que o usuário abre para editar.
		eff.Flow.ID, eff.Flow.Version = lv.ID, lv.Version
		eff.Flow.AccountID = lv.AccountID
		eff.Flow.OwnerScope, eff.Flow.OwnerID = lv.OwnerScope, lv.OwnerID
		eff.Flow.Name, eff.Flow.Description = lv.Name, lv.Description
		eff.Flow.CreatedBy = lv.CreatedBy
		eff.Flow.CreatedAt, eff.Flow.UpdatedAt = lv.CreatedAt, lv.UpdatedAt

		for _, st := range lv.Stages {
			if i, seen := posOf[st.Key]; seen {
				stages[i] = st
			} else {
				posOf[st.Key] = len(stages)
				stages = append(stages, st)
			}
			fromOf[st.Key] = ref
		}
	}

	eff.Flow.Stages = stages
	eff.Origins = make([]StageOrigin, 0, len(stages))
	for _, st := range stages {
		eff.Origins = append(eff.Origins, StageOrigin{Key: st.Key, From: fromOf[st.Key]})
	}
	// Do mais específico ao mais genérico: é assim que a frase se lê na tela.
	for i := len(sources) - 1; i >= 0; i-- {
		eff.Contributors = append(eff.Contributors, sources[i])
	}
	eff.ResolvedFrom = renderTrail(eff.Contributors, eff.Origins)
	return eff
}

// maxTrailStages limita o detalhe por etapa: o rastro é uma frase de tela, não
// um relatório.
const maxTrailStages = 12

// renderTrail escreve o rastro visível: "projeto ◂ workspace ◂ conta". Quando
// as etapas vêm de níveis diferentes, o detalhe por etapa entra junto — é
// exatamente o caso em que a pergunta "de onde veio ISSO?" aparece.
func renderTrail(sources []ScopeRef, origins []StageOrigin) string {
	if len(sources) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sources))
	for _, s := range sources {
		parts = append(parts, s.Scope.Label())
	}
	trail := strings.Join(parts, " ◂ ")
	if len(sources) == 1 {
		return trail
	}

	detail := make([]string, 0, len(origins))
	for i, o := range origins {
		if i == maxTrailStages {
			detail = append(detail, "…")
			break
		}
		detail = append(detail, fmt.Sprintf("%s (%s)", o.Key, o.From.Scope.Label()))
	}
	return trail + " — etapas: " + strings.Join(detail, ", ")
}

// ── validação ────────────────────────────────────────────────────────────────

const (
	maxStages     = 50
	maxStageKey   = 40
	maxNameLength = 120
)

// Report é o resultado de ValidateFlow: o que IMPEDE e o que apenas incomoda.
//
// A distinção é operacional: erro recusa a escrita, aviso aparece na tela e o
// autor decide. Fluxo sem etapa de spec funciona — só costuma ser esquecimento.
type Report struct {
	Errors   []string
	Warnings []string
}

func (r Report) Valid() bool { return len(r.Errors) == 0 }

// Err traduz o relatório para o erro de domínio. Recusa de fluxo inválido é
// erro do CLIENTE, com a lista inteira: devolver um problema por vez faria o
// autor consertar sete vezes o mesmo documento.
func (r Report) Err() error {
	if r.Valid() {
		return nil
	}
	return errs.Invalid("fluxo inválido: %s", strings.Join(r.Errors, "; "))
}

func (r *Report) errf(format string, a ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
}
func (r *Report) warnf(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// Validate recusa o que quebraria uma demanda EM EXECUÇÃO.
//
// O critério de cada regra é sempre o mesmo: existe uma demanda em andamento
// para a qual este documento não tem resposta? Se existe, é erro. Toda mensagem
// diz QUAL etapa e POR QUÊ — mensagem de validação que não localiza o problema
// obriga o autor a adivinhar.
//
// Sobre "ciclo" e "etapa órfã": a estrutura da v1 não tem arestas (ADR-0014
// §2), então a ordem é a própria sequência e o grafo mora nas CHAVES. Uma etapa
// sem chave é órfã de verdade — o evento de avanço endereça por chave, e
// nenhum evento consegue apontar para ela. Uma chave repetida é o ciclo — o
// avanço por chave volta para uma etapa já vencida e a régua da tela nunca
// fecha. São os dois defeitos de grafo que esta estrutura consegue ter.
func Validate(f Flow) Report {
	var r Report

	if strings.TrimSpace(f.Name) == "" {
		r.errf("fluxo sem nome: ninguém consegue escolher na tela um fluxo que não se chama nada")
	}
	if len(f.Name) > maxNameLength {
		r.errf("nome do fluxo tem mais de %d caracteres", maxNameLength)
	}
	if len(f.Stages) == 0 {
		r.errf("fluxo sem etapa nenhuma: a demanda que o adotasse não teria por onde começar")
		return r
	}
	if len(f.Stages) > maxStages {
		r.errf("fluxo com %d etapas excede o limite de %d", len(f.Stages), maxStages)
	}

	seen := make(map[string]int, len(f.Stages))
	hasSpec, hasGate := false, false

	for i, st := range f.Stages {
		where := fmt.Sprintf("%q", st.Key)
		if st.Key == "" {
			where = fmt.Sprintf("na posição %d", i+1)
		}

		switch {
		case strings.TrimSpace(st.Key) == "":
			r.errf("etapa %s está órfã: o avanço da demanda é registrado POR CHAVE e ela não tem chave — nenhum evento consegue apontar para essa etapa", where)
		case len(st.Key) > maxStageKey:
			r.errf("chave da etapa %s tem mais de %d caracteres", where, maxStageKey)
		case strings.ContainsAny(st.Key, " \t\n"):
			r.errf("chave da etapa %s tem espaço: a chave viaja em evento e em assunto de mensagem, onde espaço não sobrevive", where)
		default:
			if prev, dup := seen[st.Key]; dup {
				r.errf("etapa %q aparece nas posições %d e %d: é um ciclo — o avanço por chave voltaria para uma etapa já vencida e a régua da demanda nunca fecharia", st.Key, prev+1, i+1)
			} else {
				seen[st.Key] = i
			}
		}

		if !ValidStageType(st.Type) {
			r.errf("etapa %s tem tipo desconhecido %q: o tipo decide o renderizador na tela e o comportamento do agente, e a plataforma não sabe executar um tipo que não existe (ADR-0014 §1)", where, st.Type)
		}
		for _, a := range st.Artifacts {
			if !ValidArtifactKind(a) {
				r.errf("etapa %s declara artefato desconhecido %q: nenhuma tela sabe renderizá-lo", where, a)
			}
		}

		// Portão sem decisor — as duas formas que a estrutura permite.
		switch {
		case !ValidGate(st.Gate):
			r.errf("etapa %s tem portão desconhecido %q: o portão só pode ser humano ou nenhum", where, st.Gate)
		case st.Type == TypeHumanValidation && st.Gate != GateHuman:
			r.errf("etapa %s é de validação humana e não tem portão: sem portão ninguém é chamado a decidir e o agente passa direto — validação que não interrompe não valida nada", where)
		case st.Gate == GateHuman && len(st.Artifacts) == 0:
			r.errf("etapa %s tem portão humano e não põe artefato nenhum na mesa: o decisor abre a tela e não há sobre o que decidir", where)
		}

		if len(st.Subtypes) > 0 && st.Type != TypeTest {
			r.warnf("etapa %s declara subetapas fora de uma etapa de teste: só o tipo teste tem renderizador para elas hoje", where)
		}
		for _, sub := range st.Subtypes {
			if strings.TrimSpace(sub) == "" {
				r.errf("etapa %s tem subetapa em branco", where)
			}
		}

		if st.Type == TypeSpec {
			hasSpec = true
		}
		if st.Gate == GateHuman {
			hasGate = true
		}
	}

	if !hasSpec {
		r.warnf("fluxo sem etapa de spec: o agente vai implementar a partir do enunciado da demanda, sem critério de aceite escrito")
	}
	if !hasGate {
		r.warnf("fluxo sem nenhum portão humano: a demanda vai do início ao fim sem ninguém aprovar")
	}
	return r
}

// ── auxiliares ───────────────────────────────────────────────────────────────

func sameStrings(a, b []string) bool {
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

func sameArtifacts(a, b []ArtifactKind) bool {
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
