package grpc

import (
	"context"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

// AgentServer expõe o runtime de agente no contrato gRPC.
//
// Camada FINA: converte tipos, chama o serviço, converte de volta. Nenhuma regra
// aqui — nem qual modelo atende qual trabalho, nem o que é conclusão válida, nem
// quem assina a resposta. Se um `if` de política aparecer neste arquivo, ele está
// no lugar errado, e o lugar certo é internal/domain/agent.
//
// E, sobretudo: a CREDENCIAL não passa por aqui em direção nenhuma. Ela é lida do
// cofre pelo composition root e usada no mesmo processo (ADR-0023) — é por isso
// que `RunTurnRequest` tem `resource_id` e não tem chave.
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
			// Os dois booleanos viajam SEMPRE: sem eles, um zero em custo ou em
			// criação de cache é indistinguível de "foi de graça" e "nada foi
			// escrito no cache" — que são afirmações que ninguém pode fazer.
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
