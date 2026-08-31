// Package execution é o domínio do SUBSTRATO: onde e como uma demanda executa.
//
// Regra da casa: este pacote não conhece Kubernetes, Docker, Postgres nem gRPC.
// Ele declara o que precisa como PORTA (repository.go, mais ports.SandboxLauncher)
// e o composition root liga.
//
// A distinção que organiza tudo aqui é entre as DUAS coisas que um sandbox tem:
//
//   - a EXECUÇÃO — o pod, o contêiner, o agente rodando. Efêmera por natureza,
//     custa dinheiro enquanto existe e é barata de recriar;
//   - o WORKSPACE — os worktrees das branches da demanda. É o trabalho, e não
//     se recria: refazê-lo custa o tempo do agente e do humano que revisou.
//
// Suspender derruba a primeira e preserva o segundo. Destruir leva os dois.
// Essa diferença está no MODELO (ver Transition), não só nos nomes dos métodos:
// nome errado o compilador aceita, invariante violada ele recusa.
package execution

import (
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// State é o ciclo de vida do sandbox: ativo → suspenso → destruído (spec §3).
type State string

const (
	StateProvisioning State = "provisioning"
	StateActive       State = "active"
	StateSuspended    State = "suspended"
	StateDestroyed    State = "destroyed"
)

func ValidState(s State) bool {
	switch s {
	case StateProvisioning, StateActive, StateSuspended, StateDestroyed:
		return true
	}
	return false
}

// Transition descreve o que uma mudança de estado faz com a execução e com o
// workspace — as duas coisas que um sandbox tem.
//
// Isto existe para que a diferença entre suspender e destruir seja um FATO
// consultável, e não um detalhe que cada chamador precisa lembrar. É a mesma
// razão de EffectiveLevel morar no domínio de recurso: a regra que separa
// "economia" de "perda irreversível" é cara demais para viver espalhada.
type Transition struct {
	To State
	// StopsRuntime: a execução para. Verdadeiro nas duas — é justamente o que
	// as faz PARECEREM iguais de fora.
	StopsRuntime bool
	// DiscardsWorkspace: o trabalho da demanda vai junto. É AQUI que suspender
	// e destruir deixam de ser a mesma operação.
	DiscardsWorkspace bool
	// Reversible: existe caminho de volta pelo próprio domínio.
	Reversible bool
}

var (
	// SuspendTransition é economia: sem trabalho de agente e sem dev conectado,
	// o pod morre e o workspace sobrevive no PVC. Demandas esperam humanos por
	// horas — sandbox ocioso é o que separa paralelismo real de máquina afogada
	// (spec §3).
	SuspendTransition = Transition{
		To: StateSuspended, StopsRuntime: true, DiscardsWorkspace: false, Reversible: true,
	}
	// DestroyTransition é perda deliberada: leva a execução E o workspace, e
	// não volta. Nenhuma transição sai de destroyed — a invariante é reforçada
	// por trigger no banco (migração 0010), porque regra que ninguém pode
	// violar não pode depender de todo caminho de código lembrar dela.
	DestroyTransition = Transition{
		To: StateDestroyed, StopsRuntime: true, DiscardsWorkspace: true, Reversible: false,
	}
	// ResumeTransition recria a execução SOBRE o workspace existente.
	ResumeTransition = Transition{
		To: StateActive, StopsRuntime: false, DiscardsWorkspace: false, Reversible: true,
	}
)

// IsTerminal: destruído é absorvente. Depois dele o sandbox é só história.
func (s State) IsTerminal() bool { return s == StateDestroyed }

// PreservesWork responde a pergunta que o usuário realmente faz antes de clicar:
// "eu perco o que já foi feito?".
func (t Transition) PreservesWork() bool { return !t.DiscardsWorkspace }

// CanApply diz se a transição é legítima a partir do estado atual.
//
// A ordem das cláusulas é a regra:
//
//  1. de destroyed não sai nada. Irreversível quer dizer isso;
//  2. suspender só faz sentido a partir de ativo — suspender o que já está
//     suspenso é repetição, tratada como no-op pelo serviço, não como erro;
//  3. retomar exige workspace vivo, isto é, um sandbox suspenso;
//  4. destruir vale de qualquer estado não terminal: destruir é sempre possível.
func CanApply(from State, t Transition) bool {
	if from.IsTerminal() {
		return false
	}
	switch t.To {
	case StateSuspended:
		return from == StateActive
	case StateActive:
		return from == StateSuspended
	case StateDestroyed:
		return true
	}
	return false
}

// Sandbox é a unidade do substrato: uma demanda ativa, um sandbox (spec §1).
type Sandbox struct {
	ID        string
	AccountID string
	DemandID  string
	State     State
	// Tier é o que foi ENTREGUE, não o que foi pedido. O cliente vê o que
	// recebeu (spec §2) — e por isso é gravado, não recalculado na leitura.
	Tier           ports.IsolationTier
	Namespace      string
	Endpoints      []Endpoint
	IdempotencyKey string
	LastActiveAt   time.Time
	SuspendedAt    time.Time
	DestroyedAt    time.Time
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Endpoint é um serviço da pilha da demanda exposto ao dev.
type Endpoint struct {
	Name  string
	URL   string
	Port  int32
	State string // running | stopped
}

// Handle monta a referência que o launcher entende.
func (s Sandbox) Handle() ports.SandboxHandle {
	return ports.SandboxHandle{ID: s.ID, Namespace: s.Namespace}
}

// IsLive: o sandbox ainda ocupa lugar no substrato (ou execução, ou workspace).
func (s Sandbox) IsLive() bool { return !s.State.IsTerminal() }

// IdleFor diz há quanto tempo o sandbox está sem uso. É o número que o varredor
// de ociosidade compara com IdleTimeout.
func (s Sandbox) IdleFor(now time.Time) time.Duration {
	if s.LastActiveAt.IsZero() {
		return 0
	}
	return now.Sub(s.LastActiveAt)
}

// IdleTimeout: sem trabalho de agente e sem dev conectado por este tempo, o
// sandbox suspende (spec §3). Vive no domínio porque é política de produto —
// o adaptador não tem opinião sobre quanto tempo é "ocioso".
const IdleTimeout = 30 * time.Minute

// ShouldSuspend é a política de economia em uma função pura, testável sem
// substrato nenhum.
func (s Sandbox) ShouldSuspend(now time.Time) bool {
	return s.State == StateActive && s.IdleFor(now) >= IdleTimeout
}

// ── namespace ────────────────────────────────────────────────────────────────

// NamespaceFor deriva o namespace da demanda: dop-<id-curto>.
//
// Identificação hierárquica (conta, workspace, projeto, demanda) é LABEL, não
// nome (spec §1) — o nome carrega só o que precisa ser único e curto, porque
// nome de namespace é limitado a 63 caracteres em DNS-1123 e um id completo com
// hífens já come metade disso.
const namespaceIDLen = 8

func NamespaceFor(demandID string) string {
	short := strings.ToLower(strings.ReplaceAll(demandID, "-", ""))
	if len(short) > namespaceIDLen {
		short = short[:namespaceIDLen]
	}
	return "dop-" + short
}

// ── classificação de log ─────────────────────────────────────────────────────

// Source classifica de onde vem uma linha de log dentro do sandbox.
type Source string

const (
	SourceApp   Source = "app"
	SourceTest  Source = "test"
	SourceInfra Source = "infra"
)

// TestType é o tipo de teste, quando a linha vem de uma execução de testes.
type TestType string

const (
	TestAAA         TestType = "aaa"
	TestE2E         TestType = "e2e"
	TestIntegration TestType = "integracao"
)

// LogLine é a linha JÁ classificada, que sai pela RPC.
type LogLine struct {
	Source   Source
	Service  string
	TestType TestType
	Text     string
	At       time.Time
}

// Classify lê o prefixo de convenção de uma linha crua do substrato.
//
// A convenção mora AQUI, e não no adaptador, por um motivo prático: são dois
// adaptadores e um domínio. Colocada do lado de lá, a mesma regra de prefixo
// existiria duas vezes e divergiria no primeiro ajuste — e a suíte de contrato
// não pegaria, porque classificação de log não é garantia do substrato.
//
// Formato aceito, no começo da linha: "[app]", "[infra]", "[test:e2e]".
// Linha sem prefixo é infra: é o que o próprio substrato imprimiu.
func Classify(raw string) (Source, TestType, string) {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "[") {
		return SourceInfra, "", raw
	}
	end := strings.IndexByte(text, ']')
	if end < 0 {
		return SourceInfra, "", raw
	}
	tag := text[1:end]
	rest := strings.TrimLeft(text[end+1:], " ")

	src, tt := tag, ""
	if colon := strings.IndexByte(tag, ':'); colon >= 0 {
		src, tt = tag[:colon], tag[colon+1:]
	}
	switch Source(src) {
	case SourceApp, SourceTest, SourceInfra:
		return Source(src), TestType(tt), rest
	}
	// Colchete que não é tag nossa: a linha é do próprio processo, intacta.
	return SourceInfra, "", raw
}

// LogFilter é o recorte pedido pelo cliente no StreamLogs.
type LogFilter struct {
	Source   Source
	Service  string
	TestType TestType
}

// Matches aplica o filtro. Campo vazio não filtra — pedir tudo é o caso comum.
func (f LogFilter) Matches(l LogLine) bool {
	if f.Source != "" && f.Source != l.Source {
		return false
	}
	if f.TestType != "" && f.TestType != l.TestType {
		return false
	}
	if f.Service != "" && f.Service != l.Service {
		return false
	}
	return true
}

// ── validação ────────────────────────────────────────────────────────────────

// RequireTier é a regra que o dono do produto pediu por escrito: isolationTier é
// DECLARADO, nunca presumido.
//
// Chamador que não diz o nível é RECUSADO — não recebe um default. Escolher por
// ele seria decidir, em nome de outra pessoa, quanto isolamento a carga dela
// merece; e o erro dessa escolha só aparece quando já custou caro.
func RequireTier(t ports.IsolationTier) error {
	if t == ports.TierUnspecified {
		return errs.Invalid(
			"nível de isolamento não declarado: informe min_tier (hardware, " +
				"kernel_emulated ou namespace) — o substrato não escolhe por você")
	}
	if !ports.ValidIsolationTier(t) {
		return errs.Invalid("nível de isolamento desconhecido: %q", t)
	}
	return nil
}

// EndpointURL monta a URL pública de um serviço da demanda:
// <serviço>--<demanda-curta>.<domínio> (spec §5).
//
// Mora no domínio porque a nomeação é política do produto: se cada adaptador a
// montasse, a mesma regra existiria em dois lugares e o dev veria URLs
// diferentes conforme onde o sandbox tivesse subido.
func EndpointURL(baseDomain, demandID, service string) string {
	if baseDomain == "" || service == "" {
		return ""
	}
	short := strings.TrimPrefix(NamespaceFor(demandID), "dop-")
	return "https://" + service + "--" + short + "." + baseDomain
}
