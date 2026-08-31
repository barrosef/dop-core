package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/cost"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// CostRepo implementa cost.Repository. É o ÚNICO lugar com SQL de custo — o
// domínio nunca vê uma query.
//
// Três coisas valem por todo o arquivo:
//
//   - account_id entra em TODA cláusula WHERE, sem exceção. Isolamento
//     multi-tenant é constraint, não confiança no chamador;
//   - toda mudança de estado grava o evento na MESMA transação, por InTx +
//     Emit: commit ⇒ estado e evento, ou nenhum dos dois (ADR-0019);
//   - a decisão de QUANDO emitir estouro é do domínio
//     (cost.BudgetState.JustExceeded), não deste arquivo. Regra de negócio em
//     SQL é regra que ninguém encontra depois.
type CostRepo struct{ pool *pgxpool.Pool }

// rowQuerier é o mínimo de uma leitura de linha única: pool e tx satisfazem os
// dois. Existe para que o MESMO carregador de orçamento sirva dentro e fora de
// transação — ler o orçamento por dois caminhos diferentes é como as duas
// leituras acabam divergindo.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func NewCostRepo(pool *pgxpool.Pool) *CostRepo { return &CostRepo{pool: pool} }

// ── registro de uso ──────────────────────────────────────────────────────────

// RecordUsage é o caminho quente da plataforma e o único ponto em que a
// idempotência é a diferença entre orçamento e ficção.
//
// A guarda é o INSERT em cost_usage_keys: ele é a primeira coisa da transação
// e, quando não devolve linha, a chave já foi usada — a transação segue apenas
// para LER o estado corrente, sem gravar nada e sem acumular orçamento. A
// tabela de chaves é separada de cost_usage porque cost_usage é particionada e
// toda UNIQUE de tabela particionada precisa conter a chave de partição; a
// justificativa longa está em migrations/0008_cost.sql.
func (r *CostRepo) RecordUsage(ctx context.Context, u *cost.UsageEvent, idempotencyKey string) (*cost.RecordResult, error) {
	out := &cost.RecordResult{}

	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var usageID string
		var at time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO cost_usage_keys (account_id, idempotency_key, usage_id, occurred_at)
			VALUES ($1, $2, gen_random_uuid(), $3)
			ON CONFLICT (account_id, idempotency_key) DO NOTHING
			RETURNING usage_id, occurred_at`,
			u.AccountID, idempotencyKey, u.At).Scan(&usageID, &at)

		if NoRows(err) {
			// Repetição legítima. Devolve o que foi gravado da primeira vez e
			// o orçamento COMO ESTÁ — antes = depois, então nada "acabou de
			// estourar", mas quem perguntou continua sabendo se está estourado.
			return carregarRepeticao(ctx, tx, u, idempotencyKey, out)
		}
		if err != nil {
			return Translate(err, "chave de uso")
		}

		saved, err := insertUsage(ctx, tx, usageID, at, u)
		if err != nil {
			return err
		}
		out.Usage = saved

		out.Budgets, err = accumulate(ctx, tx, saved)
		if err != nil {
			return err
		}
		return emitUsageEvents(ctx, tx, saved, out.Budgets)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// carregarRepeticao devolve o estado corrente sem escrever nada.
func carregarRepeticao(ctx context.Context, tx pgx.Tx, u *cost.UsageEvent,
	idempotencyKey string, out *cost.RecordResult) error {

	out.Duplicate = true

	var usageID string
	var at time.Time
	if err := tx.QueryRow(ctx, `
		SELECT usage_id, occurred_at FROM cost_usage_keys
		 WHERE account_id = $1 AND idempotency_key = $2`,
		u.AccountID, idempotencyKey).Scan(&usageID, &at); err != nil {
		return Translate(err, "chave de uso")
	}

	saved, err := usageByID(ctx, tx, u.AccountID, usageID, at)
	if err != nil {
		return err
	}
	out.Usage = saved

	scopes := affectedScopes(saved)
	for _, sc := range scopes {
		b, err := budgetOf(ctx, tx, saved.AccountID, sc.scope, sc.id)
		if err != nil {
			return err
		}
		// Antes = depois: a escrita não aconteceu, logo não houve transição.
		out.Budgets = append(out.Budgets, cost.BudgetState{Before: *b, After: *b})
	}
	return nil
}

const usageCols = `id, account_id, COALESCE(demand_id::text,''), thread_id, model,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
	cost_micros, currency, occurred_at`

func scanUsage(row pgx.Row) (*cost.UsageEvent, error) {
	var u cost.UsageEvent
	var micros int64
	if err := row.Scan(&u.ID, &u.AccountID, &u.DemandID, &u.ThreadID, &u.Model,
		&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheCreationTokens,
		&micros, &u.Currency, &u.At); err != nil {
		return nil, err
	}
	u.CostMicros = cost.Micros(micros)
	return &u, nil
}

// insertUsage grava a linha com o id que a guarda já reservou — é o que amarra
// a chave de idempotência ao registro correspondente.
//
// occurred_at fora das partições declaradas falha aqui, com violação de
// restrição. É o comportamento certo: melhor recusar o registro do que gravar
// custo num mês que ninguém varre.
func insertUsage(ctx context.Context, tx pgx.Tx, id string, at time.Time, u *cost.UsageEvent) (*cost.UsageEvent, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO cost_usage (id, account_id, demand_id, thread_id, model,
			input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			cost_micros, currency, occurred_at)
		VALUES ($1, $2, NULLIF($3,'')::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+usageCols,
		id, u.AccountID, u.DemandID, u.ThreadID, u.Model,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens,
		int64(u.CostMicros), u.Currency, at)

	saved, err := scanUsage(row)
	if err != nil {
		return nil, Translate(err, "uso")
	}
	return saved, nil
}

// usageByID lê pela PK completa (id, occurred_at): sem a chave de partição o
// planejador varreria TODAS as partições para achar uma linha.
func usageByID(ctx context.Context, tx pgx.Tx, accountID, id string, at time.Time) (*cost.UsageEvent, error) {
	saved, err := scanUsage(tx.QueryRow(ctx,
		`SELECT `+usageCols+` FROM cost_usage
		  WHERE id = $1 AND occurred_at = $2 AND account_id = $3`, id, at, accountID))
	if err != nil {
		return nil, Translate(err, "uso")
	}
	return saved, nil
}

type scopeRef struct {
	scope cost.Scope
	id    string
}

// affectedScopes: todo consumo toca o orçamento da conta; consumo com demanda
// toca também o dela. A ordem é fixa para que o evento de estouro saia sempre
// na mesma sequência — log de eventos com ordem instável é log difícil de ler.
func affectedScopes(u *cost.UsageEvent) []scopeRef {
	scopes := []scopeRef{{cost.ScopeAccount, u.AccountID}}
	if u.DemandID != "" {
		scopes = append(scopes, scopeRef{cost.ScopeDemand, u.DemandID})
	}
	return scopes
}

// accumulate soma o consumo em cada escopo e devolve o ANTES e o DEPOIS.
//
// O antes sai do mesmo UPDATE (spent_micros - $delta), e não de um SELECT
// anterior: dois comandos abririam janela para duas escritas concorrentes
// lerem o mesmo "antes" e nenhuma das duas enxergar a transição de estouro.
func accumulate(ctx context.Context, tx pgx.Tx, u *cost.UsageEvent) ([]cost.BudgetState, error) {
	delta := int64(u.CostMicros)
	var out []cost.BudgetState

	for _, sc := range affectedScopes(u) {
		var limit, before, after int64
		var currency string
		var updatedAt time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO cost_budgets (account_id, scope, scope_id, limit_micros, spent_micros, currency)
			VALUES ($1, $2, $3, 0, $4, $5)
			ON CONFLICT (account_id, scope, scope_id) DO UPDATE
			   SET spent_micros = cost_budgets.spent_micros + EXCLUDED.spent_micros,
			       updated_at   = now()
			RETURNING limit_micros, spent_micros - $4, spent_micros, currency, updated_at`,
			u.AccountID, string(sc.scope), sc.id, delta, u.Currency,
		).Scan(&limit, &before, &after, &currency, &updatedAt); err != nil {
			return nil, Translate(err, "orçamento")
		}

		base := cost.Budget{
			AccountID: u.AccountID, Scope: sc.scope, ScopeID: sc.id,
			LimitMicros: cost.Micros(limit), Currency: currency, UpdatedAt: updatedAt,
		}
		antes, depois := base, base
		antes.SpentMicros = cost.Micros(before)
		depois.SpentMicros = cost.Micros(after)
		out = append(out, cost.BudgetState{Before: antes, After: depois})
	}
	return out, nil
}

// emitUsageEvents grava os eventos na transação da escrita.
//
// São dois tipos e eles têm leitores diferentes: `dop.cost.recorded` alimenta
// medição e calibração (P-7); `dop.cost.budget.exceeded` é o que faz a demanda
// PAUSAR e virar item da caixa de atenção (ADR-0011 §2). Por isso o segundo sai
// só na TRANSIÇÃO — um evento por estouro, não um por turno depois dele.
func emitUsageEvents(ctx context.Context, tx pgx.Tx, u *cost.UsageEvent, states []cost.BudgetState) error {
	aggregate, aggregateID := "account", u.AccountID
	if u.DemandID != "" {
		aggregate, aggregateID = "demand", u.DemandID
	}

	if err := Emit(ctx, tx, ports.Event{
		AccountID: u.AccountID, Aggregate: aggregate, AggregateID: aggregateID,
		Type:       "dop.cost.recorded",
		OccurredAt: u.At,
		Payload: mustJSON(map[string]any{
			"usage_id": u.ID, "thread_id": u.ThreadID, "model": u.Model,
			"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
			"cache_read_tokens": u.CacheReadTokens, "cache_creation_tokens": u.CacheCreationTokens,
			"cost_micros": int64(u.CostMicros), "currency": u.Currency,
			// Cache zerado em prompt grande é o invalidador silencioso da
			// ADR-0012 §1 — vai no evento para virar alerta sem que ninguém
			// precise reprocessar a tabela inteira para descobrir.
			"suspect_cache_miss": u.SuspectCacheMiss(),
		}),
	}); err != nil {
		return err
	}

	for _, st := range states {
		if !st.JustExceeded() {
			continue
		}
		if err := emitExceeded(ctx, tx, st.After); err != nil {
			return err
		}
	}
	return nil
}

func emitExceeded(ctx context.Context, tx pgx.Tx, b cost.Budget) error {
	aggregate := "account"
	if b.Scope == cost.ScopeDemand {
		aggregate = "demand"
	}
	return Emit(ctx, tx, ports.Event{
		AccountID: b.AccountID, Aggregate: aggregate, AggregateID: b.ScopeID,
		Type: "dop.cost.budget.exceeded",
		Payload: mustJSON(map[string]any{
			"scope": string(b.Scope), "scope_id": b.ScopeID,
			"limit_micros": int64(b.LimitMicros), "spent_micros": int64(b.SpentMicros),
			"currency": b.Currency,
		}),
	})
}

// ── orçamento ────────────────────────────────────────────────────────────────

func (r *CostRepo) BudgetOf(ctx context.Context, accountID string, scope cost.Scope, scopeID string) (*cost.Budget, error) {
	return budgetOf(ctx, r.pool, accountID, scope, scopeID)
}

// budgetOf serve pool e transação pelo mesmo caminho.
//
// Escopo sem linha em cost_budgets NÃO é "não encontrado": significa que
// ninguém definiu teto ainda, e o gasto real continua sendo pergunta legítima.
// A soma sobre cost_usage só roda nesse caso — no caminho normal existe linha
// com o acumulado, e a maior tabela do sistema não é tocada.
func budgetOf(ctx context.Context, q rowQuerier, accountID string, scope cost.Scope, scopeID string) (*cost.Budget, error) {

	b := cost.Budget{AccountID: accountID, Scope: scope, ScopeID: scopeID}
	var limit, spent int64

	err := q.QueryRow(ctx, `
		SELECT limit_micros, spent_micros, currency, updated_at
		  FROM cost_budgets
		 WHERE account_id = $1 AND scope = $2 AND scope_id = $3`,
		accountID, string(scope), scopeID).
		Scan(&limit, &spent, &b.Currency, &b.UpdatedAt)

	if NoRows(err) {
		return budgetFromUsage(ctx, q, accountID, scope, scopeID)
	}
	if err != nil {
		return nil, Translate(err, "orçamento")
	}
	b.LimitMicros, b.SpentMicros = cost.Micros(limit), cost.Micros(spent)
	return &b, nil
}

func budgetFromUsage(ctx context.Context, q rowQuerier, accountID string, scope cost.Scope, scopeID string) (*cost.Budget, error) {

	var spent int64
	var currency string
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0), COALESCE(MIN(currency), 'USD')
		  FROM cost_usage
		 WHERE account_id = $1
		   AND ($2::uuid IS NULL OR demand_id = $2::uuid)`,
		accountID, demandFilter(scope, scopeID)).Scan(&spent, &currency); err != nil {
		return nil, Translate(err, "orçamento")
	}
	// LimitMicros zero: SEM TETO. Ausência de orçamento nunca vira teto zero.
	return &cost.Budget{
		AccountID: accountID, Scope: scope, ScopeID: scopeID,
		SpentMicros: cost.Micros(spent), Currency: currency,
	}, nil
}

// demandFilter devolve nil no escopo de conta — o `$n::uuid IS NULL` das
// consultas transforma isso em "sem filtro de demanda" sem montar SQL na mão.
func demandFilter(scope cost.Scope, scopeID string) any {
	if scope == cost.ScopeDemand && scopeID != "" {
		return scopeID
	}
	return nil
}

// SetBudget grava o teto preservando o acumulado, e devolve antes/depois.
//
// A CTE `antes` lê a linha ANTIGA: dentro do mesmo comando ela enxerga o
// snapshot anterior ao upsert, que é exatamente o que se precisa para saber se
// rebaixar o teto acabou de estourar o orçamento.
func (r *CostRepo) SetBudget(ctx context.Context, b *cost.Budget) (*cost.BudgetState, error) {
	var st cost.BudgetState

	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var limAntes, gastoAntes, limDepois, gastoDepois int64
		var currency string
		var updatedAt time.Time

		if err := tx.QueryRow(ctx, `
			WITH antes AS (
			  SELECT limit_micros, spent_micros FROM cost_budgets
			   WHERE account_id = $1 AND scope = $2 AND scope_id = $3
			), upsert AS (
			  INSERT INTO cost_budgets (account_id, scope, scope_id, limit_micros, spent_micros, currency)
			  VALUES ($1, $2, $3, $4, 0, $5)
			  ON CONFLICT (account_id, scope, scope_id) DO UPDATE
			     SET limit_micros = EXCLUDED.limit_micros,
			         currency     = EXCLUDED.currency,
			         updated_at   = now()
			  RETURNING limit_micros, spent_micros, currency, updated_at
			)
			SELECT COALESCE((SELECT limit_micros FROM antes), 0),
			       COALESCE((SELECT spent_micros FROM antes), 0),
			       u.limit_micros, u.spent_micros, u.currency, u.updated_at
			  FROM upsert u`,
			b.AccountID, string(b.Scope), b.ScopeID, int64(b.LimitMicros), b.Currency,
		).Scan(&limAntes, &gastoAntes, &limDepois, &gastoDepois, &currency, &updatedAt); err != nil {
			return Translate(err, "orçamento")
		}

		st.Before = cost.Budget{
			AccountID: b.AccountID, Scope: b.Scope, ScopeID: b.ScopeID,
			LimitMicros: cost.Micros(limAntes), SpentMicros: cost.Micros(gastoAntes),
			Currency: currency,
		}
		st.After = cost.Budget{
			AccountID: b.AccountID, Scope: b.Scope, ScopeID: b.ScopeID,
			LimitMicros: cost.Micros(limDepois), SpentMicros: cost.Micros(gastoDepois),
			Currency: currency, UpdatedAt: updatedAt,
		}

		if err := Emit(ctx, tx, ports.Event{
			AccountID: b.AccountID, Aggregate: budgetAggregate(b.Scope), AggregateID: b.ScopeID,
			Type: "dop.cost.budget.set",
			Payload: mustJSON(map[string]any{
				"scope": string(b.Scope), "scope_id": b.ScopeID,
				"limit_micros": int64(limDepois), "previous_limit_micros": int64(limAntes),
				"spent_micros": int64(gastoDepois), "currency": currency,
			}),
		}); err != nil {
			return err
		}

		// Teto rebaixado abaixo do gasto é estouro de verdade, e precisa
		// pausar a demanda pelo mesmo caminho do estouro por consumo.
		if st.JustExceeded() {
			return emitExceeded(ctx, tx, st.After)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func budgetAggregate(s cost.Scope) string {
	if s == cost.ScopeDemand {
		return "demand"
	}
	return "account"
}

// ── agregação ────────────────────────────────────────────────────────────────

// Summarize agrega no BANCO, não em memória: trazer as linhas do período para
// somar em Go seria mover milhões de registros pela rede para produzir seis
// números.
func (r *CostRepo) Summarize(ctx context.Context, accountID string, scope cost.Scope, scopeID string,
	since, until time.Time, recentLimit int) (*cost.Summary, error) {

	s := cost.Summary{Scope: scope, ScopeID: scopeID, Since: since, Until: until}
	var total int64
	demanda := demandFilter(scope, scopeID)

	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0), COUNT(*),
		       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0),
		       COALESCE(MIN(currency), 'USD')
		  FROM cost_usage
		 WHERE account_id = $1
		   AND occurred_at >= $2 AND occurred_at < $3
		   AND ($4::uuid IS NULL OR demand_id = $4::uuid)`,
		accountID, since, until, demanda,
	).Scan(&total, &s.Calls, &s.InputTokens, &s.OutputTokens,
		&s.CacheReadTokens, &s.CacheCreationTokens, &s.Currency); err != nil {
		return nil, Translate(err, "resumo de custo")
	}
	s.TotalMicros = cost.Micros(total)

	if recentLimit <= 0 {
		return &s, nil
	}

	rows, err := r.pool.Query(ctx, `
		SELECT `+usageCols+`
		  FROM cost_usage
		 WHERE account_id = $1
		   AND occurred_at >= $2 AND occurred_at < $3
		   AND ($4::uuid IS NULL OR demand_id = $4::uuid)
		 ORDER BY occurred_at DESC
		 LIMIT $5`, accountID, since, until, demanda, recentLimit)
	if err != nil {
		return nil, Translate(err, "resumo de custo")
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, Translate(err, "resumo de custo")
		}
		s.Recent = append(s.Recent, *u)
	}
	return &s, rows.Err()
}

var _ cost.Repository = (*CostRepo)(nil)
