package grpc

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/cost"
)

// CostServer exposes the cost domain on the gRPC contract.
//
// A THIN layer: it converts types, calls the service, converts back. No rule
// here — not what an overrun is, not which model serves which work. If an `if`
// of policy shows up in this file, it is in the wrong place, and the right place
// is cost/router.go or cost/service.go.
type CostServer struct {
	dopv1.UnimplementedCostServiceServer
	svc *cost.Service
}

func NewCostServer(svc *cost.Service) *CostServer { return &CostServer{svc: svc} }

// RecordUsage propagates the idempotency key from the request's BODY, and not
// from the interceptor: here it is not merely retry protection, it is the
// guarantee that a budget does not count twice (ADR-0008). The domain requires
// it.
func (s *CostServer) RecordUsage(ctx context.Context, req *dopv1.RecordUsageRequest) (*dopv1.RecordUsageResponse, error) {
	out, err := s.svc.RecordUsage(ctx, usageFromProto(req.GetUsage()), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	// recorded=true on the repetition too: from the caller's point of view the
	// consumption IS recorded. Returning false would lead them to try again,
	// which is exactly the opposite of what idempotency solves.
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
		// The body's spent_micros is IGNORED on purpose: the accumulated total
		// belongs to the system, not to the client. Accepting it would allow
		// zeroing the spend by asking.
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
		// The justification goes to the client WHOLE. It is what allows auditing
		// ("why did this demand run on the expensive model?") and recalibrating
		// without reading the code — and it says, in every decision, that the
		// policy is still ADR-0008's draft (P-7).
		Reason: d.Reason,
	}, nil
}

// SummarizeCost uses the DEFAULT period (the current month).
//
// The contract has no period fields yet — SummarizeCostRequest carries only the
// scope. When it has them, the domain already takes since/until and only the
// conversion changes: that is why the service does not pin the month
// internally.
func (s *CostServer) SummarizeCost(ctx context.Context, req *dopv1.SummarizeCostRequest) (*dopv1.SummarizeCostResponse, error) {
	// A zeroed period = the domain's default (the current month).
	var defaultPeriod time.Time
	sum, err := s.svc.Summarize(ctx, cost.Scope(req.GetScope()), req.GetScopeId(),
		defaultPeriod, defaultPeriod, recentInSummary)
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

// recentInSummary: the screen shows the latest consumptions next to the total.
// Twenty fit without paging; the usage table is the one that grows the most and
// is not swept whole to draw a side list.
const recentInSummary = 20

// ── conversions ──────────────────────────────────────────────────────────────

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
	// An absent `at` does NOT become time's zero: the service fills it in from
	// the clock. Writing 0001-01-01 would send the record to a nonexistent
	// partition.
	if ts := u.GetAt(); ts != nil {
		out.At = ts.AsTime()
	}
	// account_id does not come from the body: the service takes it from the call context.
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
		// Micros with no currency is a number with no unit. The edge was having
		// to invent one or leave it blank — and a monetary value with no unit is
		// how dollars get summed with reais without anyone noticing.
		Currency: b.Currency,
	}
}

var _ dopv1.CostServiceServer = (*CostServer)(nil)
