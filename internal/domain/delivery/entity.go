// Package delivery é o domínio da entrega: o caminho do código verificado até
// a `main` — evidência de verde, pull request, fila de merge por repositório e
// as diretrizes de coordenação do techlead.
//
// Regra da casa: este pacote não conhece Postgres, gRPC nem SDK de provedor
// git. Ele declara o que precisa como PORTA (repository.go) e o composition
// root liga.
//
// Duas decisões organizam tudo o que está aqui:
//
//   - **Evidência é dado, não confiança** (ADR-0007). Não existe campo
//     `verified bool` vindo do chamador. O que existe é uma lista de execuções
//     — qual suíte, sobre qual commit, com qual resultado, com que rastro — e o
//     verde é uma FUNÇÃO dessa lista. Um "sem verde, sem PR" que aceita um
//     booleano de quem chama não prova nada.
//   - **Diretriz coordena, nunca pausa** (ADR-0015 §5). O vocabulário de ações
//     de uma diretriz é fechado e não contém "pausar", "bloquear" nem
//     "suspender" — e a porta para o domínio de demanda é somente leitura. Não
//     há caminho, nem por engano, pelo qual uma decisão de coordenação pare uma
//     demanda que já está andando.
package delivery

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ─────────────────────────── evidência de verde ───────────────────────────

// CheckKind é a natureza de uma execução de verificação.
//
// `critic` é uma execução como as outras de propósito: o parecer do crítico
// (ADR-0007 §3) é evidência com resultado, rastro e commit — não um adjetivo
// pendurado no PR.
type CheckKind string

const (
	CheckAcceptance CheckKind = "acceptance" // critérios executáveis da spec
	CheckUnit       CheckKind = "unit"
	CheckE2E        CheckKind = "e2e"
	CheckCritic     CheckKind = "critic" // parecer da instância independente
)

func ValidCheckKind(k CheckKind) bool {
	switch k {
	case CheckAcceptance, CheckUnit, CheckE2E, CheckCritic:
		return true
	}
	return false
}

// Outcome é o veredito de UMA execução. Não há "desconhecido": execução sem
// resultado não é evidência, é ruído.
type Outcome string

const (
	OutcomePassed  Outcome = "passed"
	OutcomeFailed  Outcome = "failed"
	OutcomeErrored Outcome = "errored" // nem passou nem falhou: quebrou no meio
)

func ValidOutcome(o Outcome) bool {
	switch o {
	case OutcomePassed, OutcomeFailed, OutcomeErrored:
		return true
	}
	return false
}

// VerificationRun é UMA execução verificável — a unidade da evidência.
//
// Commit é o campo que dá valor a tudo: evidência que não diz sobre QUAL
// código rodou serve para provar qualquer coisa, e portanto não prova nada. É
// por isso que o verde é sempre perguntado para um commit específico, e não
// para "a demanda".
type VerificationRun struct {
	ID        string
	AccountID string
	DemandID  string
	RepoID    string
	Commit    string // SHA exato sobre o qual a execução rodou
	Kind      CheckKind
	Suite     string // nome da suíte ou do critério — o QUE rodou
	Outcome   Outcome
	Total     int
	Passed    int
	Failed    int
	// Rastro: onde rodou (sandbox) e onde estão os logs (ObjectStore).
	// Um dos dois é obrigatório — "passou" sem rastro é palavra, não evidência.
	SandboxID string
	LogRef    string
	Detail    map[string]any // parecer do crítico, motivos da falha
	Attempts  int            // quantas vezes esta suíte rodou neste commit
	StartedAt time.Time
	EndedAt   time.Time
}

// Validate recusa a execução que não serviria como prova.
func (r VerificationRun) Validate() error {
	if strings.TrimSpace(r.DemandID) == "" || strings.TrimSpace(r.RepoID) == "" {
		return errs.Invalid("execução de verificação sem demanda ou repositório")
	}
	if strings.TrimSpace(r.Commit) == "" {
		return errs.Invalid("execução de verificação sem commit: evidência que não diz sobre qual código rodou não é evidência")
	}
	if !ValidCheckKind(r.Kind) {
		return errs.Invalid("tipo de verificação desconhecido: %q", r.Kind)
	}
	if !ValidOutcome(r.Outcome) {
		return errs.Invalid("resultado de verificação desconhecido: %q", r.Outcome)
	}
	if strings.TrimSpace(r.Suite) == "" {
		return errs.Invalid("execução de verificação sem identificação do que rodou")
	}
	if r.SandboxID == "" && r.LogRef == "" {
		return errs.Invalid("execução sem rastro (sandbox ou log): resultado sem onde conferir não prova verde")
	}
	if r.Outcome == OutcomePassed && r.Failed > 0 {
		return errs.Invalid("execução aprovada com %d falha(s) é incoerente", r.Failed)
	}
	return nil
}

// Evidence é o pacote de evidência de UM commit: as execuções que provaram —
// ou não provaram — que aquele código exato está verde.
type Evidence struct {
	DemandID string
	RepoID   string
	Commit   string
	Runs     []VerificationRun
}

// Missing devolve TUDO o que falta para o commit estar verde, em português.
//
// Lista, e não booleano, porque quem for recusado precisa saber o que
// providenciar. "Precondição falhou" sem dizer qual precondição é o mesmo que
// não responder.
func (e Evidence) Missing() []string {
	var falta []string
	if strings.TrimSpace(e.Commit) == "" {
		return []string{"não há commit a verificar: o PR precisa apontar para um commit"}
	}

	var aceitacao, critico int
	for _, r := range e.Runs {
		// Execução de outro commit não conta — nem contra, nem a favor. Foi o
		// que barrou a quebra semântica que a ADR-0008 descreve: o verde de
		// ontem não é o verde de agora.
		if r.Commit != e.Commit {
			falta = append(falta, fmt.Sprintf(
				"a execução %q é do commit %s, não do commit %s em revisão",
				r.Suite, curto(r.Commit), curto(e.Commit)))
			continue
		}
		if r.Outcome != OutcomePassed {
			falta = append(falta, fmt.Sprintf(
				"a execução %q (%s) terminou em %s", r.Suite, r.Kind, r.Outcome))
			continue
		}
		switch r.Kind {
		case CheckAcceptance:
			aceitacao++
		case CheckCritic:
			critico++
		}
	}
	if aceitacao == 0 {
		falta = append(falta, fmt.Sprintf(
			"nenhuma execução de aceitação aprovada para o commit %s (ADR-0007 §1)", curto(e.Commit)))
	}
	if critico == 0 {
		falta = append(falta, fmt.Sprintf(
			"falta o parecer do crítico para o commit %s (ADR-0007 §3)", curto(e.Commit)))
	}
	return falta
}

// Green é a pergunta que a ADR-0007 faz. A resposta vem da lista de execuções,
// nunca de um campo que alguém preencheu.
func (e Evidence) Green() bool { return len(e.Missing()) == 0 }

// Reason junta o que falta numa frase única, para a mensagem de recusa.
func (e Evidence) Reason() string { return strings.Join(e.Missing(), "; ") }

func curto(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// ─────────────────────────── pull request ───────────────────────────

type Reviewer struct {
	Name     string
	Initials string
	Status   string // approved | rejected | pending
}

// PullRequest é o PR já aberto — e, por construção, um PR só existe se o
// commit dele estava verde no momento da abertura (ADR-0007). A regra é
// verificada aqui, no serviço, e de novo por TRIGGER no banco: invariante que
// não pode ser violada por nenhum caminho não vive só no código de aplicação.
type PullRequest struct {
	ID           string
	AccountID    string
	DemandID     string
	RepoID       string
	Repo         string // nome legível, "org/repo"
	SourceBranch string
	TargetBranch string
	HeadCommit   string // o commit que a evidência cobre
	URL          string
	ExternalID   string // número/id no provedor
	Merged       bool
	HasConflict  bool
	Reviewers    []Reviewer
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ─────────────────────────── fila de merge ───────────────────────────

// QueueState são os estados da ADR-0008: na fila → rebase → re-verificação →
// merge, um de cada vez, por repositório.
type QueueState string

const (
	StateQueued    QueueState = "queued"
	StateRebasing  QueueState = "rebasing"
	StateVerifying QueueState = "verifying"
	StateMerged    QueueState = "merged"
	StateConflict  QueueState = "conflict"
)

func ValidQueueState(s QueueState) bool {
	switch s {
	case StateQueued, StateRebasing, StateVerifying, StateMerged, StateConflict:
		return true
	}
	return false
}

// IsTerminal: merge é o fim da linha. Conflito NÃO é terminal — é espera por
// gente, e volta para o rebase quando alguém resolver.
func (s QueueState) IsTerminal() bool { return s == StateMerged }

// NeedsHuman marca o estado que alimenta a caixa de atenção.
func (s QueueState) NeedsHuman() bool { return s == StateConflict }

// CanTransitionTo escreve o fluxo da ADR-0008 como máquina de estados. Sem
// isso, "verificando → na fila" acontece por acidente de código e ninguém nota.
func (s QueueState) CanTransitionTo(n QueueState) bool {
	switch s {
	case StateQueued:
		return n == StateRebasing || n == StateConflict
	case StateRebasing:
		return n == StateVerifying || n == StateConflict
	case StateVerifying:
		// Re-verificação reprovada devolve o PR ao agente: o conflito aqui é
		// semântico, não textual — é exatamente o caso que a fila existe para
		// pegar.
		return n == StateMerged || n == StateConflict
	case StateConflict:
		return n == StateRebasing // resolvido, tenta de novo
	}
	return false
}

// DefaultPriority é a prioridade de quem entra sem diretriz de ordem. Vale
// deixá-la longe de zero: diretriz de ordem precisa poder colocar alguém ANTES
// do que já está na fila sem renumerar o mundo.
const DefaultPriority = 100

// ConflictReport é o conflito virado DADO — o que a caixa de atenção precisa
// para o humano decidir sem arqueologia (ADR-0008 §2).
type ConflictReport struct {
	Files      []string
	BaseCommit string // contra qual `main` o rebase foi tentado
	Attempts   int    // quantas vezes o agente tentou antes de escalar
	Detail     string
	ReportedAt time.Time
}

func (c ConflictReport) Validate() error {
	if len(c.Files) == 0 && strings.TrimSpace(c.Detail) == "" {
		return errs.Invalid("relato de conflito sem arquivos nem descrição não serve para ninguém decidir")
	}
	return nil
}

// MergeQueueEntry é a posição de um PR na fila de UM repositório (ADR-0008).
type MergeQueueEntry struct {
	ID            string
	AccountID     string
	RepoID        string
	DemandID      string
	PullRequestID string
	// Seq é a sequência de chegada DENTRO do repositório, única por
	// repositório (constraint no banco). É o desempate que impede duas
	// entradas de ficarem ambiguamente lado a lado.
	Seq int64
	// Priority é onde a diretriz de ordem preferencial (ADR-0015) atua. Menor
	// entra antes. Repare que reordenar não pausa ninguém: a demanda que
	// perdeu a vez continua correndo, só mergeia depois.
	Priority         int
	Position         int32 // DERIVADO da ordem; não é estado persistido
	State            QueueState
	OverlappingFiles []string // detecção do techlead (ADR-0008 §3)
	Conflict         *ConflictReport
	EnqueuedAt       time.Time
	UpdatedAt        time.Time
}

// Before é a ordem TOTAL da fila: (prioridade, sequência).
//
// Total, e não parcial, é o ponto inteiro: como Seq é única por repositório,
// não existe par de entradas para o qual `a.Before(b)` e `b.Before(a)` sejam
// ambos falsos. Fila com empate é fila cuja ordem depende de quem executou o
// ORDER BY — e aí a posição mostrada no cockpit muda sozinha entre dois
// refreshes.
func (e MergeQueueEntry) Before(o MergeQueueEntry) bool {
	if e.Priority != o.Priority {
		return e.Priority < o.Priority
	}
	return e.Seq < o.Seq
}

// SortQueue ordena e NUMERA as posições (1-based).
//
// A ordem é do domínio, não do ORDER BY: o adaptador já devolve ordenado, e
// ainda assim reordenamos aqui. Uma fila serializa merges — a regra de quem
// vai antes é regra de negócio, e regra de negócio que mora só no SQL não é
// testável sem banco.
func SortQueue(entries []MergeQueueEntry) []MergeQueueEntry {
	out := make([]MergeQueueEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Before(out[j]) })
	for i := range out {
		out[i].Position = int32(i + 1)
	}
	return out
}

// ─────────────────────────── diretrizes ───────────────────────────

// DirectiveKind é o vocabulário INICIAL da ADR-0015 §6 — e também o vocabulário
// das ações que uma diretriz instrui.
//
// Repare no que não existe aqui e não vai existir: pausar, bloquear, suspender,
// esperar. A regra de ouro da ADR-0015 ("transversal identificada NUNCA pausa
// demanda") não é um cuidado de quem implementa: é a ausência de um valor no
// tipo. Demanda 1 segue até onde der; quando a condição da diretriz se cumprir,
// aplica a coordenação e continua.
type DirectiveKind string

const (
	DirectiveCherryPick    DirectiveKind = "cherry_pick"
	DirectiveMergeOrder    DirectiveKind = "merge_order"
	DirectiveFilePartition DirectiveKind = "file_partition"
	DirectiveCrossVerify   DirectiveKind = "cross_verify"
)

func ValidDirectiveKind(k DirectiveKind) bool {
	switch k {
	case DirectiveCherryPick, DirectiveMergeOrder, DirectiveFilePartition, DirectiveCrossVerify:
		return true
	}
	return false
}

type DirectiveStatus string

const (
	DirectiveProposed   DirectiveStatus = "proposed"
	DirectiveDecided    DirectiveStatus = "decided"
	DirectiveSuperseded DirectiveStatus = "superseded"
)

// Instruction é o que uma diretriz decidida entrega a UMA demanda.
//
// É sempre TRABALHO A FAZER — "faça cherry-pick quando a 0 commitar", "rode a
// aceitação da 1 sobre o resultado da 0". When é a condição que dispara a
// aplicação, e existir uma condição é o oposto de bloquear: a demanda segue e
// aplica a coordenação quando a condição se cumprir.
type Instruction struct {
	DemandID string
	Action   DirectiveKind
	When     string // condição em linguagem do domínio: "demanda X commitou"
	Payload  map[string]any
}

func (i Instruction) Validate() error {
	if strings.TrimSpace(i.DemandID) == "" {
		return errs.Invalid("instrução de coordenação sem demanda destino")
	}
	// A porta estreita: só o vocabulário de coordenação passa. Uma "instrução"
	// de pausar simplesmente não tem como ser expressa.
	if !ValidDirectiveKind(i.Action) {
		return errs.Invalid(
			"ação de coordenação fora do vocabulário: %q — diretriz coordena, nunca pausa demanda (ADR-0015 §5)",
			i.Action)
	}
	return nil
}

// DirectiveOption é uma das saídas prontas que o techlead traz junto com o
// problema. A caixa de atenção recebe item de decisão, não alarme cru.
type DirectiveOption struct {
	Key          string
	Summary      string
	Instructions []Instruction
}

// Decision registra QUEM decidiu e POR QUÊ. As duas coisas são obrigatórias:
// coordenação entre demandas paralelas é decisão de engenharia, e decisão sem
// motivo registrado vira mágica invisível três semanas depois (ADR-0015).
type Decision struct {
	Option    string
	Rationale string
	DecidedBy string
	ActorKind string
	DecidedAt time.Time
}

// Directive é a provocação de decisão do techlead sobre uma transversal.
type Directive struct {
	ID        string
	AccountID string
	ProjectID string
	Kind      DirectiveKind
	Summary   string
	Payload   map[string]any // sinais: arquivos sobrepostos, diffs, specs lidas
	// AffectedDemands são as demandas que a transversal toca. Estão aqui como
	// referência, e SÓ como referência: nada neste domínio escreve no estado
	// delas.
	AffectedDemands []string
	Options         []DirectiveOption
	Recommended     string // chave da opção recomendada
	Status          DirectiveStatus
	Decision        *Decision
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Option devolve a opção pela chave.
func (d Directive) Option(key string) (DirectiveOption, bool) {
	for _, o := range d.Options {
		if o.Key == key {
			return o, true
		}
	}
	return DirectiveOption{}, false
}

// Validate exige o que a ADR-0015 §3 exige de uma provocação de decisão:
// opções prontas (no mínimo duas — com uma só não há o que decidir, é alarme
// com botão de OK) e uma recomendação entre elas.
func (d Directive) Validate() error {
	if strings.TrimSpace(d.ProjectID) == "" {
		return errs.Invalid("diretriz sem projeto")
	}
	if !ValidDirectiveKind(d.Kind) {
		return errs.Invalid("tipo de diretriz desconhecido: %q", d.Kind)
	}
	if strings.TrimSpace(d.Summary) == "" {
		return errs.Invalid("diretriz sem enunciado da transversal detectada")
	}
	if len(d.Options) < 2 {
		return errs.Invalid("diretriz precisa de ao menos duas opções: uma opção só é alarme, não decisão (ADR-0015 §3)")
	}
	vistas := make(map[string]bool, len(d.Options))
	for _, o := range d.Options {
		if strings.TrimSpace(o.Key) == "" {
			return errs.Invalid("opção de diretriz sem chave")
		}
		if vistas[o.Key] {
			return errs.Invalid("opção %q duplicada na diretriz", o.Key)
		}
		vistas[o.Key] = true
		if strings.TrimSpace(o.Summary) == "" {
			return errs.Invalid("opção %q sem descrição do que ela faz", o.Key)
		}
		if len(o.Instructions) == 0 {
			return errs.Invalid("opção %q não instrui nenhuma demanda: decisão que não vira coordenação não serve", o.Key)
		}
		for _, ins := range o.Instructions {
			if err := ins.Validate(); err != nil {
				return err
			}
		}
	}
	if _, ok := d.Option(d.Recommended); !ok {
		return errs.Invalid("a recomendação %q não está entre as opções oferecidas", d.Recommended)
	}
	return nil
}
