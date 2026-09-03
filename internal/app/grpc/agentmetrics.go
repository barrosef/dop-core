package grpc

import (
	"context"
	"encoding/json"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/agentmetrics"
)

// AgentMetricsServer is the surface the COLLECTOR pushes to — a container beside
// the agent in the sandbox's pod, with no person behind it (ADR-0029).
type AgentMetricsServer struct {
	dopv1.UnimplementedAgentMetricsServiceServer
	svc *agentmetrics.Service
}

func NewAgentMetricsServer(svc *agentmetrics.Service) *AgentMetricsServer {
	return &AgentMetricsServer{svc: svc}
}

func (s *AgentMetricsServer) RecordTurns(ctx context.Context, req *dopv1.RecordTurnsRequest) (*dopv1.RecordTurnsResponse, error) {
	turns := make([]agentmetrics.Turn, 0, len(req.GetTurns()))
	for _, t := range req.GetTurns() {
		turns = append(turns, turnFromProto(t))
	}
	n, err := s.svc.Ingest(ctx, agentmetrics.Session{
		ExternalID:  req.GetSession(),
		DemandID:    req.GetDemandId(),
		ProjectID:   req.GetProjectId(),
		CWD:         req.GetCwd(),
		GitBranch:   req.GetGitBranch(),
		ToolVersion: req.GetToolVersion(),
		ByteOffset:  req.GetByteOffset(),
		StartedAt:   firstTurnAt(turns),
		EndedAt:     lastTurnAt(turns),
		Auth: agentmetrics.SessionAuth{
			Method:       req.GetAuth().GetMethod(),
			Provider:     req.GetAuth().GetProvider(),
			Subscription: req.GetAuth().GetSubscription(),
			KeySource:    req.GetAuth().GetKeySource(),
		},
	}, turns)
	if err != nil {
		return nil, err
	}
	return &dopv1.RecordTurnsResponse{Recorded: int32(n)}, nil
}

func (s *AgentMetricsServer) GetDemandConsumption(ctx context.Context, req *dopv1.GetDemandConsumptionRequest) (*dopv1.DemandConsumption, error) {
	c, err := s.svc.Consumption(ctx, req.GetDemandId())
	if err != nil {
		return nil, err
	}
	out := &dopv1.DemandConsumption{
		DemandId: c.DemandID, Sessions: int32(c.Sessions), Turns: int32(c.Turns),
		InputTokens: c.InputTokens, OutputTokens: c.OutputTokens,
		CacheCreationTokens: c.CacheCreationTokens, CacheReadTokens: c.CacheReadTokens,
		CacheRatio:    c.CacheRatio(),
		TokensByModel: c.TokensByModel,
		CallsByTool:   map[string]int32{},
	}
	for tool, n := range c.CallsByTool {
		out.CallsByTool[tool] = int32(n)
	}
	if !c.FirstTurnAt.IsZero() {
		out.FirstTurnAt = timestamppb.New(c.FirstTurnAt)
	}
	if !c.LastTurnAt.IsZero() {
		out.LastTurnAt = timestamppb.New(c.LastTurnAt)
	}
	return out, nil
}

func turnFromProto(t *dopv1.AgentTurn) agentmetrics.Turn {
	out := agentmetrics.Turn{
		UUID: t.GetUuid(), ParentUUID: t.GetParentUuid(),
		Model: t.GetModel(), ServiceTier: t.GetServiceTier(),
		StopReason: t.GetStopReason(), Sidechain: t.GetSidechain(),
		InputTokens: t.GetInputTokens(), OutputTokens: t.GetOutputTokens(),
		CacheCreationTokens: t.GetCacheCreationTokens(),
		CacheReadTokens:     t.GetCacheReadTokens(),
		ToolUses:            int(t.GetToolUses()),
		Thinking:            int(t.GetThinking()),
		Texts:               int(t.GetTexts()),
		Tools:               t.GetTools(),
		RawUsage:            map[string]any{},
	}
	if out.Tools == nil {
		out.Tools = []string{}
	}
	if t.GetOccurredAt() != nil {
		out.OccurredAt = t.GetOccurredAt().AsTime()
	}
	// The rest of `usage` travels as JSON: what has no field yet is kept whole
	// rather than dropped, and a payload we cannot read is not worth failing a
	// batch over.
	if raw := t.GetRawUsage(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &out.RawUsage)
	}
	return out
}

// The batch's window. The collector pushes as the file grows, so a session's
// start and end are refreshed by whatever batch happens to carry them.
func firstTurnAt(turns []agentmetrics.Turn) time.Time {
	var at time.Time
	for _, t := range turns {
		if t.OccurredAt.IsZero() {
			continue
		}
		if at.IsZero() || t.OccurredAt.Before(at) {
			at = t.OccurredAt
		}
	}
	return at
}

func lastTurnAt(turns []agentmetrics.Turn) time.Time {
	var at time.Time
	for _, t := range turns {
		if t.OccurredAt.After(at) {
			at = t.OccurredAt
		}
	}
	return at
}
