package grpc

import (
	"context"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/agent"
)

// AgentServer exposes the agent runtime on the gRPC contract.
//
// A THIN layer: it converts types, calls the service, converts back. No rule
// here — not which model serves which work, not what a valid conclusion is, not
// who signs the reply. If an `if` of policy shows up in this file, it is in the
// wrong place, and the right place is internal/domain/agent.
//
// And, above all: the CREDENTIAL does not pass through here in any direction. It
// is read from the vault by the composition root and used in the same process
// (ADR-0016) — which is why `RunTurnRequest` has a `resource_id` and no key.
type AgentServer struct {
	dopv1.UnimplementedAgentServiceServer
	svc *agent.Service
}

func NewAgentServer(svc *agent.Service) *AgentServer { return &AgentServer{svc: svc} }

func (s *AgentServer) RunTurn(ctx context.Context, req *dopv1.RunTurnRequest) (*dopv1.TurnOutcome, error) {
	out, err := s.svc.RunTurn(ctx, agent.TurnRequest{
		DemandID:        req.GetDemandId(),
		ThreadID:        req.GetThreadId(),
		Text:            req.GetText(),
		TaskKind:        req.GetTaskKind(),
		ResourceID:      req.GetResourceId(),
		OperatorNote:    req.GetOperatorNote(),
		MaxOutputTokens: int(req.GetMaxOutputTokens()),
	}, req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return turnOutcomeToProto(out), nil
}

func turnOutcomeToProto(o *agent.TurnOutcome) *dopv1.TurnOutcome {
	if o == nil {
		return nil
	}
	out := &dopv1.TurnOutcome{
		Demand:   &dopv1.DemandRef{Id: o.DemandID},
		ThreadId: o.ThreadID,
		Provider: o.Provider,
		Routing: &dopv1.TurnRouting{
			TaskKind:      o.Routing.TaskKind,
			ModelClass:    string(o.Routing.Class),
			Model:         o.Routing.Model,
			Effort:        string(o.Routing.Effort),
			EffortApplied: string(o.Routing.EffortApplied),
			Reason:        o.Routing.Reason,
			FromAgentCard: o.Routing.FromAgentCard,
		},
		Reply:      o.Reply,
		MessageIds: o.MessageIDs,
		Concluded:  o.Concluded,
		Usage: &dopv1.TurnUsage{
			InputTokens:         o.Usage.InputTokens,
			OutputTokens:        o.Usage.OutputTokens,
			CacheReadTokens:     o.Usage.CacheReadTokens,
			CacheCreationTokens: o.Usage.CacheCreationTokens,
			Cost: &dopv1.Money{
				Currency:     o.Usage.Currency,
				AmountMicros: int64(o.Usage.CostMicros),
			},
			// Both booleans travel ALWAYS: without them, a zero in cost or in
			// cache creation is indistinguishable from "it was free" and
			// "nothing was written to the cache" — assertions nobody can
			// make.
			CacheCreationKnown: o.Usage.CacheCreationKnown,
			CostKnown:          o.Usage.CostKnown,
		},
		ContextTruncated: o.ContextTruncated,
		Paused:           o.Paused,
		Notice:           o.Notice,
		Warnings:         o.Warnings,
	}
	if o.Finding != nil {
		out.Finding = &dopv1.TurnFinding{Id: o.Finding.ID, Title: o.Finding.Title}
	}
	for _, b := range o.Budgets {
		out.Budgets = append(out.Budgets, &dopv1.Budget{
			Scope:       b.Scope,
			ScopeId:     b.ScopeID,
			LimitMicros: int64(b.LimitMicros),
			SpentMicros: int64(b.SpentMicros),
			Currency:    b.Currency,
		})
	}
	return out
}

var _ dopv1.AgentServiceServer = (*AgentServer)(nil)
