// Package demand é o domínio da demanda — o coração da plataforma.
//
// A premissa que organiza tudo aqui é a da ADR-0006: a demanda NÃO é uma linha
// que muda de status. Ela é um log append-only de eventos, e o que se chama de
// "estado da demanda" (etapa corrente, threads abertas, achados publicados) é
// PROJEÇÃO desse log. Por isso nenhuma operação deste pacote muda estado sem
// produzir o evento correspondente: as duas coisas viajam juntas até o
// repositório, que as grava na MESMA transação (ADR-0019). Ação sem evento é
// bug, não detalhe.
//
// Regra da casa: este pacote não conhece Postgres, gRPC nem SDK nenhum. O que
// precisa de fora entra como PORTA (repository.go) e o composition root liga.
package demand

import (
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── vocabulário ──────────────────────────────────────────────────────────────

// DopStatus é o status da demanda DENTRO da plataforma — distinto do status do
// provedor (Jira, ClickUp), que é texto livre e continua sendo dele.
type DopStatus string

const (
	StatusNew       DopStatus = "new"
	StatusDoing     DopStatus = "doing"
	StatusDone      DopStatus = "done"
	StatusDelivered DopStatus = "delivered"
)

type StageStatus string

const (
	StagePending StageStatus = "pending"
	StageRunning StageStatus = "running"
	StageBlocked StageStatus = "blocked"
	StageDone    StageStatus = "done"
)

// StageType é o vocabulário FECHADO de tipos da plataforma (ADR-0014): o tipo
// decide o renderizador na tela e o comportamento do agente. Composição nova é
// dado; tipo novo é evolução da plataforma — por isso a lista mora em código.
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

// Gate diz se a etapa termina sozinha ou depende de decisão humana.
type Gate string

const (
	GateNone  Gate = "none"
	GateHuman Gate = "human"
)

// ThreadState é o ciclo da conversa (spec conversacao-e-atencao §1):
// aberta → ativa → bloqueada → concluída. Concluir EXIGE achado publicado — a
// thread não morre em silêncio.
type ThreadState string

const (
	ThreadOpen      ThreadState = "aberta"
	ThreadActive    ThreadState = "ativa"
	ThreadBlocked   ThreadState = "bloqueada"
	ThreadConcluded ThreadState = "concluida"
)

type ArtifactKind string

const (
	ArtifactDocument ArtifactKind = "document"
	ArtifactSpec     ArtifactKind = "spec"
	ArtifactPlan     ArtifactKind = "plan"
	ArtifactTestPlan ArtifactKind = "test_plan"
	ArtifactDiagram  ArtifactKind = "diagram"
	ArtifactReport   ArtifactKind = "report"
)

// ── o fluxo congelado ────────────────────────────────────────────────────────

// StageSpec é uma etapa do FLUXO (o molde), não da demanda (a instância).
type StageSpec struct {
	Key       string
	Name      string
	Type      StageType
	Gate      Gate
	Artifacts []ArtifactKind
	Subtypes  []string // teste → aaa, e2e, integracao
}

// Flow é o fluxo efetivo devolvido pela cadeia de resolução
// plataforma ◁ conta ◁ workspace ◁ projeto ◁ demanda (ADR-0014).
type Flow struct {
	ID           string
	Name         string
	Version      int32
	ResolvedFrom string // rastro visível: "projeto ◂ workspace ◂ conta"
	Stages       []StageSpec
}

// Snapshot é o fluxo CONGELADO dentro da demanda.
//
// Guardar só (flow_id, version) não bastaria: versão é rótulo, e um fluxo
// editado no lugar — ou promovido, ou apagado — reescreveria o passado de toda
// demanda em andamento. O snapshot é a cópia imutável do molde no instante em
// que a demanda começou; é ele, e não o fluxo vivo, que dirige a máquina de
// etapas daqui em diante.
type Snapshot struct {
	FlowID       string
	Name         string
	Version      int32
	ResolvedFrom string
	Stages       []StageSpec
	FrozenAt     time.Time
}

func freeze(f Flow, at time.Time) Snapshot {
	stages := make([]StageSpec, len(f.Stages))
	copy(stages, f.Stages)
	return Snapshot{
		FlowID: f.ID, Name: f.Name, Version: f.Version,
		ResolvedFrom: f.ResolvedFrom, Stages: stages, FrozenAt: at,
	}
}

// ── entidades ────────────────────────────────────────────────────────────────

type Artifact struct {
	ID        string
	Kind      ArtifactKind
	Name      string
	ObjectRef string // ponteiro no ObjectStore — o binário não passa por aqui
	Version   int32
	CreatedAt time.Time
}

// Stage é a INSTÂNCIA da etapa nesta demanda: o molde vem do snapshot, o
// progresso vem do log.
type Stage struct {
	Key        string
	Name       string
	Type       StageType
	Gate       Gate
	Status     StageStatus
	Position   int
	Artifacts  []Artifact
	StartedAt  *time.Time
	FinishedAt *time.Time
	// Decisão do portão humano, quando houve: nil = ninguém decidiu ainda.
	GateApproved *bool
	GateComment  string
}

type Demand struct {
	ID             string
	AccountID      string
	ProjectID      string
	ExternalKey    string // SUOPT-1315
	Title          string
	CardType       string // dinâmico, do provedor
	ProviderStatus string
	Status         DopStatus
	Flow           Snapshot
	Stages         []Stage
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// AgentCard é a ficha do subagente: propósito, ferramentas concedidas, modelo e
// fatia do orçamento da demanda (ADR-0010).
type AgentCard struct {
	Purpose      string
	Tools        []string
	Model        string
	Effort       string // low | medium | high | xhigh | max
	BudgetMicros int64
}

type Thread struct {
	ID        string
	AccountID string
	DemandID  string
	Key       string // principal, forense-db, logs
	Card      AgentCard
	State     ThreadState
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Blocked é o que a caixa de atenção lê: pergunta pendente vira item de fila.
func (t Thread) Blocked() bool { return t.State == ThreadBlocked }

type Message struct {
	ID         string
	AccountID  string
	DemandID   string
	ThreadID   string
	AuthorID   string
	AuthorKind string
	AuthorName string
	Text       string
	At         time.Time
}

// Finding é a conclusão publicada por um agente. Vira contexto dos irmãos, do
// dossiê e da memória do projeto (ADR-0010 §4).
type Finding struct {
	ID        string
	AccountID string
	DemandID  string
	ThreadID  string
	Title     string
	Payload   map[string]any
	CreatedBy string
	CreatedAt time.Time
}

// ── regras da máquina de etapas (ADR-0014) ───────────────────────────────────

// stageTransitions é a máquina, escrita como dado.
//
// `done` é terminal de propósito: desfazer uma etapa concluída seria reescrever
// o passado da demanda, e o passado é o log. Retrabalho é etapa nova ou demanda
// nova, não um `done` que volta atrás.
var stageTransitions = map[StageStatus][]StageStatus{
	StagePending: {StageRunning},
	StageRunning: {StageBlocked, StageDone},
	StageBlocked: {StageRunning}, // desbloqueia antes de concluir
	StageDone:    {},
}

func ValidStageStatus(s StageStatus) bool {
	_, ok := stageTransitions[s]
	return ok
}

// StageByKey acha a etapa pela chave do fluxo congelado.
func (d *Demand) StageByKey(key string) (*Stage, error) {
	for i := range d.Stages {
		if d.Stages[i].Key == key {
			return &d.Stages[i], nil
		}
	}
	return nil, errs.Invalid(
		"a etapa %q não existe no fluxo congelado desta demanda (%s v%d)",
		key, d.Flow.Name, d.Flow.Version)
}

// CheckAdvance decide se a transição pedida é legítima, e RECUSA dizendo qual
// etapa e por quê — mensagem genérica aqui vira ticket de suporte depois.
//
// Devolve noop=true quando a etapa já está no status pedido: a entrega de
// eventos é ao-menos-uma-vez (ADR-0019), então repetir um avanço é rotina e não
// pode virar erro.
func (d *Demand) CheckAdvance(key string, to StageStatus) (st *Stage, noop bool, err error) {
	if !ValidStageStatus(to) {
		return nil, false, errs.Invalid("status de etapa desconhecido: %q", to)
	}
	s, err := d.StageByKey(key)
	if err != nil {
		return nil, false, err
	}
	if s.Status == to {
		return s, true, nil
	}

	// O TIPO da etapa dirige a máquina: portão humano não fecha sozinho.
	// Deixar AdvanceStage concluir uma etapa de validação humana seria a
	// plataforma aprovando em nome do humano — exatamente o que o portão
	// existe para impedir.
	if to == StageDone && s.RequiresHumanGate() {
		return nil, false, errs.Invalid(
			"a etapa %q (%s) tem portão humano: conclua por DecideGate, não por AdvanceStage",
			s.Key, s.Type)
	}

	allowed := stageTransitions[s.Status]
	if !contains(allowed, to) {
		return nil, false, errs.Invalid(
			"transição inválida na etapa %q: de %s para %s (permitido a partir de %s: %s)",
			s.Key, s.Status, to, s.Status, join(allowed))
	}

	// Ordem do fluxo: começar a etapa 4 com a 2 pendente esconderia trabalho
	// pulado atrás de um progresso que parece legítimo na tela.
	if to == StageRunning {
		if prev := d.firstUnfinishedBefore(s.Position); prev != nil {
			return nil, false, errs.Invalid(
				"a etapa %q não pode começar: a etapa anterior %q ainda está %s",
				s.Key, prev.Key, prev.Status)
		}
	}
	return s, false, nil
}

// RequiresHumanGate: o portão declarado no fluxo, mais o tipo `human_validation`,
// que é portão humano por definição — um fluxo que declare `gate: none` numa
// etapa de validação humana está se contradizendo, e o tipo vence.
func (s Stage) RequiresHumanGate() bool {
	return s.Gate == GateHuman || s.Type == TypeHumanValidation
}

// CheckDecideGate valida a decisão do portão.
func (d *Demand) CheckDecideGate(key string) (*Stage, error) {
	s, err := d.StageByKey(key)
	if err != nil {
		return nil, err
	}
	if !s.RequiresHumanGate() {
		return nil, errs.Invalid(
			"a etapa %q não tem portão humano: nada a decidir", s.Key)
	}
	switch s.Status {
	case StagePending:
		return nil, errs.Invalid(
			"a etapa %q ainda não começou: não há o que aprovar", s.Key)
	case StageDone:
		return nil, errs.Invalid(
			"a etapa %q já foi concluída: o passado da demanda não se reescreve", s.Key)
	}
	return s, nil
}

func (d *Demand) firstUnfinishedBefore(pos int) *Stage {
	for i := range d.Stages {
		if d.Stages[i].Position < pos && d.Stages[i].Status != StageDone {
			return &d.Stages[i]
		}
	}
	return nil
}

// ProjectStatus recalcula o status da demanda a partir das etapas — é
// projeção, nunca campo que alguém escreve à mão.
//
// `delivered` não é derivável daqui: quem entrega é o domínio de entrega, e o
// que ele já marcou não regride por causa de uma etapa.
func (d *Demand) ProjectStatus() DopStatus {
	if d.Status == StatusDelivered {
		return StatusDelivered
	}
	if len(d.Stages) == 0 {
		return StatusNew
	}
	done, touched := 0, false
	for _, s := range d.Stages {
		if s.Status == StageDone {
			done++
		}
		if s.Status != StagePending {
			touched = true
		}
	}
	switch {
	case done == len(d.Stages):
		return StatusDone
	case touched:
		return StatusDoing
	default:
		return StatusNew
	}
}

// ── regras da thread (spec conversacao-e-atencao §1) ─────────────────────────

// CheckConclude é a regra que dá nome à seção: a thread não morre em silêncio.
//
// hasFinding vem do repositório porque o achado é ESTADO — publicado na mesma
// transação do evento — e não pode ser consultado na projeção assíncrona, que
// pode ainda não ter visto a publicação de um segundo atrás.
func (t Thread) CheckConclude(hasFinding bool) error {
	if t.State == ThreadConcluded {
		return nil // já concluída: repetir é inócuo
	}
	if !hasFinding {
		return errs.Precondition(
			"a thread %q não pode ser concluída sem achado publicado: "+
				"publique a conclusão da investigação antes de encerrá-la", t.Key)
	}
	return nil
}

// CheckPost recusa mensagem em thread encerrada — depois do achado, o registro
// durável é o achado; reabrir conversa é thread nova.
func (t Thread) CheckPost() error {
	if t.State == ThreadConcluded {
		return errs.Precondition(
			"a thread %q está concluída e não recebe mensagens novas", t.Key)
	}
	return nil
}

// ValidateThreadKey: a chave é o endereço da thread na demanda (#principal,
// #forense-db) — precisa ser estável e digitável.
func ValidateThreadKey(k string) error {
	k = strings.TrimSpace(k)
	if k == "" {
		return errs.Invalid("a thread precisa de uma chave (ex.: principal, forense-db)")
	}
	if len(k) > 64 {
		return errs.Invalid("chave de thread pode ter no máximo 64 caracteres")
	}
	for _, r := range k {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return errs.Invalid(
				"chave de thread aceita apenas minúsculas, números, hífen e sublinhado: %q", k)
		}
	}
	return nil
}

// ── auxiliares ───────────────────────────────────────────────────────────────

func contains(list []StageStatus, v StageStatus) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func join(list []StageStatus) string {
	if len(list) == 0 {
		return "nenhum"
	}
	parts := make([]string, 0, len(list))
	for _, s := range list {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ", ")
}
