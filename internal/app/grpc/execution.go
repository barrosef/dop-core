package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/execution"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// ExecutionServer expõe o substrato no contrato gRPC.
//
// Camada FINA: converte tipos, chama o serviço, converte de volta. Repare no
// que NÃO acontece aqui: min_tier UNSPECIFIED não vira um default. Ele desce
// como está e o domínio recusa — porque a regra "declarado, nunca presumido"
// morreria se a borda gentilmente preenchesse o vazio antes de perguntar.
type ExecutionServer struct {
	dopv1.UnimplementedExecutionServiceServer
	svc *execution.Service
}

func NewExecutionServer(svc *execution.Service) *ExecutionServer {
	return &ExecutionServer{svc: svc}
}

func (s *ExecutionServer) ProvisionSandbox(ctx context.Context, req *dopv1.ProvisionSandboxRequest) (*dopv1.Sandbox, error) {
	sb, err := s.svc.Provision(ctx, req.GetDemandId(),
		tierFromProto(req.GetMinTier()), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return sandboxToProto(sb), nil
}

func (s *ExecutionServer) SuspendSandbox(ctx context.Context, req *dopv1.SuspendSandboxRequest) (*dopv1.Sandbox, error) {
	sb, err := s.svc.Suspend(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return sandboxToProto(sb), nil
}

func (s *ExecutionServer) ResumeSandbox(ctx context.Context, req *dopv1.ResumeSandboxRequest) (*dopv1.Sandbox, error) {
	sb, err := s.svc.Resume(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return sandboxToProto(sb), nil
}

func (s *ExecutionServer) DestroySandbox(ctx context.Context, req *dopv1.DestroySandboxRequest) (*dopv1.DestroySandboxResponse, error) {
	ok, err := s.svc.Destroy(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return &dopv1.DestroySandboxResponse{Destroyed: ok}, nil
}

func (s *ExecutionServer) DescribeSandbox(ctx context.Context, req *dopv1.DescribeSandboxRequest) (*dopv1.Sandbox, error) {
	sb, err := s.svc.Describe(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return sandboxToProto(sb), nil
}

// StreamLogs é server-side streaming puro, no mesmo desenho de WatchEvents.
//
// O contexto de chamada JÁ chega preenchido: quem faz isso é o interceptador
// StreamCallContext. Sem ele, o isolamento multi-tenant do fluxo passaria a
// depender do CallContext declarado no corpo do pedido — campo que o servidor
// NÃO lê (ADR-0017, conv. 5).
func (s *ExecutionServer) StreamLogs(req *dopv1.StreamLogsRequest, stream dopv1.ExecutionService_StreamLogsServer) error {
	ctx := stream.Context()

	f := execution.LogFilter{
		Source:   execution.Source(req.GetSource()),
		Service:  req.GetService(),
		TestType: execution.TestType(req.GetTestType()),
	}
	return s.svc.StreamLogs(ctx, req.GetSandboxId(), f, func(l execution.LogLine) error {
		// Send devolve erro quando o cliente sumiu; o erro sobe pelo emit, o
		// domínio interrompe o Tail e o adaptador fecha o corpo da resposta no
		// defer. É assim que a goroutine morre junto, sem vazar.
		return stream.Send(&dopv1.LogLine{
			Source:  string(l.Source),
			Service: l.Service,
			Line:    l.Text,
			At:      timestamppb.New(l.At),
		})
	})
}

// ── conversões ───────────────────────────────────────────────────────────────

func sandboxToProto(s *execution.Sandbox) *dopv1.Sandbox {
	if s == nil {
		return nil
	}
	out := &dopv1.Sandbox{
		Id:        s.ID,
		Demand:    &dopv1.DemandRef{Id: s.DemandID},
		State:     stateToProto(s.State),
		Tier:      tierToProto(s.Tier),
		Namespace: s.Namespace,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(s.CreatedAt),
			UpdatedAt: timestamppb.New(s.UpdatedAt),
			CreatedBy: &dopv1.ActorRef{Kind: dopv1.ActorRef_KIND_USER, Id: s.CreatedBy},
		},
	}
	if !s.LastActiveAt.IsZero() {
		out.LastActiveAt = timestamppb.New(s.LastActiveAt)
	}
	for _, e := range s.Endpoints {
		out.Endpoints = append(out.Endpoints, &dopv1.SandboxEndpoint{
			Name: e.Name, Url: e.URL, Port: e.Port, State: e.State,
		})
	}
	return out
}

func stateToProto(s execution.State) dopv1.Sandbox_State {
	switch s {
	case execution.StateProvisioning:
		return dopv1.Sandbox_STATE_PROVISIONING
	case execution.StateActive:
		return dopv1.Sandbox_STATE_ACTIVE
	case execution.StateSuspended:
		return dopv1.Sandbox_STATE_SUSPENDED
	case execution.StateDestroyed:
		return dopv1.Sandbox_STATE_DESTROYED
	}
	return dopv1.Sandbox_STATE_UNSPECIFIED
}

// tierFromProto NÃO tem default. UNSPECIFIED atravessa como não declarado, e é
// o domínio que recusa — com a mensagem que diz o que fazer.
func tierFromProto(t dopv1.IsolationTier) ports.IsolationTier {
	switch t {
	case dopv1.IsolationTier_ISOLATION_TIER_HARDWARE:
		return ports.TierHardware
	case dopv1.IsolationTier_ISOLATION_TIER_KERNEL_EMULATED:
		return ports.TierKernelEmulated
	case dopv1.IsolationTier_ISOLATION_TIER_NAMESPACE:
		return ports.TierNamespace
	}
	return ports.TierUnspecified
}

func tierToProto(t ports.IsolationTier) dopv1.IsolationTier {
	switch t {
	case ports.TierHardware:
		return dopv1.IsolationTier_ISOLATION_TIER_HARDWARE
	case ports.TierKernelEmulated:
		return dopv1.IsolationTier_ISOLATION_TIER_KERNEL_EMULATED
	case ports.TierNamespace:
		return dopv1.IsolationTier_ISOLATION_TIER_NAMESPACE
	}
	return dopv1.IsolationTier_ISOLATION_TIER_UNSPECIFIED
}

var _ dopv1.ExecutionServiceServer = (*ExecutionServer)(nil)
