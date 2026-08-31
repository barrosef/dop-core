package cost

import (
	"context"
	"time"
)

// Repository é a PORTA de persistência do domínio de custo.
//
// Declarada aqui, em linguagem de domínio; implementada em
// internal/adapter/postgres. O domínio nunca vê SQL.
//
// Toda operação recebe accountID explicitamente: isolamento multi-tenant é
// parâmetro obrigatório da porta, não algo que o adaptador possa esquecer.
type Repository interface {
	// RecordUsage grava o consumo e acumula os orçamentos afetados numa ÚNICA
	// transação, junto com os eventos (ADR-0019). O contrato que o adaptador
	// tem de cumprir:
	//
	//  1. IDEMPOTÊNCIA REAL por (accountID, idempotencyKey): a repetição não
	//     grava linha nova, NÃO acumula orçamento de novo e devolve
	//     Duplicate=true com o estado corrente dos orçamentos. Sem isso o
	//     orçamento vira ficção — uma retentativa de rede contaria duas vezes;
	//  2. os orçamentos afetados são o da conta e, quando há demanda, o da
	//     demanda. O acumulado sobe no MESMO comando que devolve o valor
	//     anterior, para que a transição fique visível;
	//  3. emite `dop.cost.recorded` e, para cada orçamento em que
	//     BudgetState.JustExceeded(), `dop.cost.budget.exceeded` — tudo na
	//     mesma transação da escrita.
	RecordUsage(ctx context.Context, u *UsageEvent, idempotencyKey string) (*RecordResult, error)

	// BudgetOf devolve o orçamento do escopo. Escopo sem orçamento definido
	// NÃO é erro: devolve limite zero (sem teto) com o gasto real, porque
	// "quanto já se gastou" é pergunta legítima antes de existir teto.
	BudgetOf(ctx context.Context, accountID string, scope Scope, scopeID string) (*Budget, error)

	// SetBudget grava o teto PRESERVANDO o acumulado, e emite
	// `dop.cost.budget.set` na mesma transação. Devolve o antes e o depois:
	// rebaixar o teto abaixo do gasto corrente é um estouro tão real quanto
	// gastar além do teto, e sem o par isso passaria despercebido.
	SetBudget(ctx context.Context, b *Budget) (*BudgetState, error)

	// Summarize agrega por escopo e período. recentLimit ≤ 0 dispensa a lista
	// de eventos recentes — a agregação sozinha é barata, a lista não.
	Summarize(ctx context.Context, accountID string, scope Scope, scopeID string,
		since, until time.Time, recentLimit int) (*Summary, error)
}

// RecordResult é o que a escrita devolve.
//
// Budgets traz o antes/depois de cada escopo tocado; Duplicate diz se foi
// repetição. Repare que não há campo de erro para estouro: estourar orçamento
// NUNCA falha a escrita (ADR-0011 §2) — mata-se a demanda com uma pausa, não
// com uma perda de medição.
type RecordResult struct {
	Usage     *UsageEvent
	Duplicate bool
	Budgets   []BudgetState
}
