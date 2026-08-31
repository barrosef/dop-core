// Package cost é o domínio da governança de gasto com LLM: quanto se gastou,
// quanto se pode gastar e qual modelo atende cada tipo de trabalho (ADR-0011).
//
// Regra da casa: este pacote não conhece Postgres, gRPC nem SDK nenhum. Ele
// declara o que precisa como PORTA (repository.go) e o composition root liga.
//
// A ADR-0011 tem duas partes firmes e uma em rascunho, e o código separa as
// três de propósito: medição e orçamento são regra (entity.go, service.go); a
// política tarefa→modelo é palpite informado e mora sozinha em router.go, para
// ser recalibrada em um lugar só quando a telemetria chegar (P-7).
package cost

import (
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Micros é valor monetário em 10^-6 da unidade da moeda.
//
// Dinheiro NÃO é float aqui, e a razão é aritmética, não estilo: soma de
// milhões de linhas em ponto flutuante binário acumula erro, e orçamento que
// erra não é orçamento. Inteiro em unidade mínima é exato por construção,
// compara e soma sem surpresa, e é o que o contrato já fala
// (Money.amount_micros, api/proto/dop/v1/common.proto) — converter na borda
// criaria dois vocabulários de dinheiro no mesmo sistema.
//
// Micros, e não centavos, porque um único token de saída custa fração de
// centavo: em centavos, todo registro individual arredondaria para zero e o
// acumulado seria sistematicamente menor que a fatura.
type Micros int64

// Currency acompanha SEMPRE o valor. Micros solto convida a somar dólar com
// real e a descobrir isso na fatura.
const DefaultCurrency = "USD"

// Scope é a unidade de orçamento. São dois, e de propósito: a fatia por thread
// vem da ficha do subagente (ADR-0010), não é orçamento próprio.
type Scope string

const (
	ScopeAccount Scope = "account"
	ScopeDemand  Scope = "demand"
)

func ValidScope(s Scope) bool { return s == ScopeAccount || s == ScopeDemand }

// UsageEvent é UM consumo de modelo: um turno, um subagente, uma chamada.
//
// Os quatro contadores de token são separados porque têm preços de ordens de
// grandeza diferentes (ADR-0012): leitura de prefixo cacheado sai a ~0,1× do
// input e escrita de cache a 1,25×. Guardar só um "total de tokens" jogaria
// fora exatamente a informação que calibra o roteador (P-7) e que denuncia o
// invalidador silencioso de cache.
type UsageEvent struct {
	ID        string
	AccountID string
	DemandID  string // vazio = consumo da conta, sem demanda (batch, indexação)
	ThreadID  string
	Model     string

	InputTokens         int64 // input NÃO cacheado
	OutputTokens        int64
	CacheReadTokens     int64 // prefixo servido do cache — o barato
	CacheCreationTokens int64 // prefixo gravado no cache — 1,25× o input

	CostMicros Micros
	Currency   string
	At         time.Time
}

// PromptTokens é tudo que entrou no prompt, cacheado ou não. É o denominador
// honesto da taxa de acerto de cache: o numerador só faz sentido contra o
// prompt inteiro, não contra o input não cacheado.
func (u UsageEvent) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// SuspectCacheMiss sinaliza o alerta da ADR-0012 §1: prefixo grande entrando
// sem NENHUMA leitura de cache. Ou o prefixo mudou (byte volátil no pacote de
// contexto), ou o TTL de 5 min venceu — nos dois casos alguém está pagando
// 10× pelo mesmo prefixo e ninguém percebeu.
//
// É heurística, e assumidamente: o limiar existe para não gritar no primeiro
// turno de uma thread, que legitimamente não tem cache para ler.
func (u UsageEvent) SuspectCacheMiss() bool {
	return u.CacheReadTokens == 0 && u.InputTokens >= cacheMissFloor
}

// cacheMissFloor: abaixo disto o prompt é pequeno demais para valer cache — o
// mínimo cacheável da API é dessa ordem. Recalibrar com P-7.
const cacheMissFloor = 2048

func (u UsageEvent) Validate() error {
	if u.AccountID == "" {
		return errs.Invalid("uso sem conta")
	}
	if u.Model == "" {
		// Sem modelo não há calibração possível: o registro entraria como
		// linha de custo anônima e sairia da telemetria de P-7.
		return errs.Invalid("uso sem modelo")
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 ||
		u.CacheReadTokens < 0 || u.CacheCreationTokens < 0 {
		return errs.Invalid("contagem de tokens não pode ser negativa")
	}
	if u.CostMicros < 0 {
		return errs.Invalid("custo não pode ser negativo")
	}
	return nil
}

// Budget é o teto e o acumulado de um escopo.
type Budget struct {
	AccountID   string
	Scope       Scope
	ScopeID     string
	LimitMicros Micros
	SpentMicros Micros
	Currency    string
	UpdatedAt   time.Time
}

// Unlimited: limite zero é AUSÊNCIA de teto, não teto zero. A distinção é a
// diferença entre "conta nova funciona" e "conta nova nasce pausada no
// primeiro token".
func (b Budget) Unlimited() bool { return b.LimitMicros <= 0 }

// Exceeded é o estado corrente. Estourado NÃO significa bloqueado: o que
// acontece com o estouro é decisão do serviço (pausa, ADR-0011 §2), e nunca
// recusa de registro.
func (b Budget) Exceeded() bool { return !b.Unlimited() && b.SpentMicros >= b.LimitMicros }

// Remaining nunca é negativo: quem consome esse número quer saber quanto ainda
// dá para gastar, e "menos vinte" não responde essa pergunta.
func (b Budget) Remaining() Micros {
	if b.Unlimited() {
		return 0
	}
	if b.SpentMicros >= b.LimitMicros {
		return 0
	}
	return b.LimitMicros - b.SpentMicros
}

// NewlyExceeded é a regra de EMISSÃO do evento de estouro, e mora aqui porque
// o adaptador precisa dela dentro da transação — deixá-la em SQL espalharia
// regra de negócio pelo schema.
//
// O que importa é a TRANSIÇÃO, não o estado: um orçamento já estourado gera um
// item na caixa de atenção, não um por turno até alguém olhar. Vale tanto para
// o gasto que cruza o teto quanto para o teto que é rebaixado sob o gasto —
// os dois são o mesmo evento visto de lados diferentes.
func NewlyExceeded(before, after Budget) bool {
	return !before.Exceeded() && after.Exceeded()
}

// BudgetState é o par antes/depois de uma escrita. Existe porque a decisão de
// emitir depende da transição, e a transição só é visível se os dois lados
// atravessarem a fronteira juntos.
type BudgetState struct {
	Before Budget
	After  Budget
}

func (s BudgetState) JustExceeded() bool { return NewlyExceeded(s.Before, s.After) }

// Summary é a agregação por escopo e período.
type Summary struct {
	Scope    Scope
	ScopeID  string
	Since    time.Time
	Until    time.Time
	Currency string

	TotalMicros Micros
	Calls       int64

	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64

	Recent []UsageEvent
}

// CacheHitRatio é a fração do prompt que veio do cache no período.
//
// O denominador é o prompt INTEIRO (input + leitura + escrita de cache) e não
// apenas o input: é a única forma de o número responder "quanto do que eu
// mandei saiu barato". Perto de zero num fluxo de agente = prefixo instável
// (ADR-0012 §1), que é a fatura mais cara que existe.
func (s Summary) CacheHitRatio() float64 {
	total := s.InputTokens + s.CacheReadTokens + s.CacheCreationTokens
	if total <= 0 {
		return 0
	}
	return float64(s.CacheReadTokens) / float64(total)
}
