package grpc

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/cost"
)

// CostServer expõe o domínio de custo no contrato gRPC.
//
// Camada FINA: converte tipos, chama o serviço, converte de volta. Nenhuma
// regra aqui — nem o que é estouro, nem qual modelo atende qual trabalho. Se
// um `if` de política aparecer neste arquivo, ele está no lugar errado, e o
// lugar certo é cost/router.go ou cost/service.go.
type CostServer struct {
	dopv1.UnimplementedCostServiceServer
	svc *cost.Service
}

func NewCostServer(svc *cost.Service) *CostServer { return &CostServer{svc: svc} }

// RecordUsage propaga a chave de idempotência do CORPO da requisição, e não do
// interceptador: aqui ela não é só proteção contra retentativa, é a garantia de
// que orçamento não conta duas vezes (ADR-0011). O domínio a exige.
func (s *CostServer) RecordUsage(ctx context.Context, req *dopv1.RecordUsageRequest) (*dopv1.RecordUsageResponse, error) {
	out, err := s.svc.RecordUsage(ctx, usageFromProto(req.GetUsage()), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	// recorded=true também na repetição: do ponto de vista do chamador o
	// consumo ESTÁ registrado. Devolver false o levaria a tentar de novo, que
	// é exatamente o contrário do que a idempotência resolve.
	return &dopv1.RecordUsageResponse{
		Recorded:       true,
		BudgetExceeded: out.BudgetExceeded,
	}, nil
}

func (s *CostServer) GetBudget(ctx context.Context, req *dopv1.GetBudgetRequest) (*dopv1.Budget, error) {
	b, err := s.svc.GetBudget(ctx, cost.Scope(req.GetScope()), req.GetScopeId())
	if err != nil {
		return nil, err
	}
	return budgetToProto(b), nil
}

func (s *CostServer) SetBudget(ctx context.Context, req *dopv1.SetBudgetRequest) (*dopv1.Budget, error) {
	in := req.GetBudget()
	b, err := s.svc.SetBudget(ctx, cost.Budget{
		Scope:       cost.Scope(in.GetScope()),
		ScopeID:     in.GetScopeId(),
		LimitMicros: cost.Micros(in.GetLimitMicros()),
		// spent_micros do corpo é IGNORADO de propósito: o acumulado é do
		// sistema, não do cliente. Aceitá-lo permitiria zerar o gasto pedindo.
	})
	if err != nil {
		return nil, err
	}
	return budgetToProto(b), nil
}

func (s *CostServer) RouteModel(ctx context.Context, req *dopv1.RouteModelRequest) (*dopv1.RoutingDecision, error) {
	d, err := s.svc.RouteModel(ctx, cost.TaskKind(req.GetTaskKind()), req.GetDemandId())
	if err != nil {
		return nil, err
	}
	return &dopv1.RoutingDecision{
		TaskKind: string(d.TaskKind),
		Model:    d.Model,
		Effort:   string(d.Effort),
		// A justificativa vai INTEIRA para o cliente. É o que permite auditar
		// ("por que esta demanda rodou no modelo caro?") e recalibrar sem ler
		// o código — e ela diz, em toda decisão, que a política ainda é
		// rascunho da ADR-0011 (P-7).
		Reason: d.Reason,
	}, nil
}

// SummarizeCost usa o período PADRÃO (mês corrente).
//
// O contrato ainda não tem campos de período — SummarizeCostRequest carrega só
// escopo. Quando tiver, o domínio já recebe since/until e só a conversão muda:
// é por isso que o serviço não fixa o mês por dentro.
func (s *CostServer) SummarizeCost(ctx context.Context, req *dopv1.SummarizeCostRequest) (*dopv1.SummarizeCostResponse, error) {
	// Período zerado = padrão do domínio (mês corrente).
	var padrão time.Time
	sum, err := s.svc.Summarize(ctx, cost.Scope(req.GetScope()), req.GetScopeId(),
		padrão, padrão, recentInSummary)
	if err != nil {
		return nil, err
	}

	recent := make([]*dopv1.UsageEvent, 0, len(sum.Recent))
	for i := range sum.Recent {
		recent = append(recent, usageToProto(&sum.Recent[i]))
	}
	return &dopv1.SummarizeCostResponse{
		Total:         &dopv1.Money{Currency: sum.Currency, AmountMicros: int64(sum.TotalMicros)},
		CacheHitRatio: sum.CacheHitRatio(),
		Recent:        recent,
	}, nil
}

// recentInSummary: a tela mostra os últimos consumos ao lado do total. Vinte
// cabem sem paginar; a tabela de uso é a que mais cresce e não se varre inteira
// para desenhar uma lista lateral.
const recentInSummary = 20

// ── conversões ───────────────────────────────────────────────────────────────

func usageFromProto(u *dopv1.UsageEvent) cost.UsageEvent {
	if u == nil {
		return cost.UsageEvent{}
	}
	out := cost.UsageEvent{
		DemandID:            u.GetDemand().GetId(),
		ThreadID:            u.GetThreadId(),
		Model:               u.GetModel(),
		InputTokens:         u.GetInputTokens(),
		OutputTokens:        u.GetOutputTokens(),
		CacheReadTokens:     u.GetCacheReadTokens(),
		CacheCreationTokens: u.GetCacheCreationTokens(),
		CostMicros:          cost.Micros(u.GetCost().GetAmountMicros()),
		Currency:            u.GetCost().GetCurrency(),
	}
	// `at` ausente NÃO vira o zero de time: o serviço preenche pelo relógio.
	// Gravar 0001-01-01 mandaria o registro para uma partição inexistente.
	if ts := u.GetAt(); ts != nil {
		out.At = ts.AsTime()
	}
	// account_id não vem do corpo: o serviço o toma do contexto de chamada.
	return out
}

func usageToProto(u *cost.UsageEvent) *dopv1.UsageEvent {
	if u == nil {
		return nil
	}
	out := &dopv1.UsageEvent{
		Id:                  u.ID,
		ThreadId:            u.ThreadID,
		Model:               u.Model,
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		Cost:                &dopv1.Money{Currency: u.Currency, AmountMicros: int64(u.CostMicros)},
		At:                  timestamppb.New(u.At),
	}
	if u.DemandID != "" {
		out.Demand = &dopv1.DemandRef{Id: u.DemandID}
	}
	return out
}

func budgetToProto(b *cost.Budget) *dopv1.Budget {
	if b == nil {
		return nil
	}
	return &dopv1.Budget{
		Scope:       string(b.Scope),
		ScopeId:     b.ScopeID,
		LimitMicros: int64(b.LimitMicros),
		SpentMicros: int64(b.SpentMicros),
		// Micros sem moeda é número sem unidade. A borda estava tendo que
		// inventar ou deixar em branco — e valor monetário sem unidade é como
		// se soma dólar com real sem ninguém perceber.
		Currency: b.Currency,
	}
}

var _ dopv1.CostServiceServer = (*CostServer)(nil)
