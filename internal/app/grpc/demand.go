package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/demand"
	"github.com/barrosef/dop-core/internal/domain/ports"
)

// DemandServer exposes the demand domain on the gRPC contract.
//
// A THIN layer: it translates types, calls the service, translates back. No rule
// about stage transitions, gates or thread conclusion appears here — they live
// in the domain, and they have to keep living there, or else they come to exist
// in two places that diverge with the first new client.
type DemandServer struct {
	dopv1.UnimplementedDemandServiceServer
	svc *demand.Service
}

func NewDemandServer(svc *demand.Service) *DemandServer { return &DemandServer{svc: svc} }

func (s *DemandServer) ListDemands(ctx context.Context, req *dopv1.ListDemandsRequest) (*dopv1.ListDemandsResponse, error) {
	list, next, err := s.svc.List(ctx, req.GetProject().GetId(),
		int(req.GetPage().GetSize()), req.GetPage().GetToken())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Demand, 0, len(list))
	for i := range list {
		out = append(out, demandToProto(&list[i]))
	}
	return &dopv1.ListDemandsResponse{
		Demands: out,
		Page:    &dopv1.PageResponse{NextToken: next, Total: int32(len(out))},
	}, nil
}

func (s *DemandServer) GetDemand(ctx context.Context, req *dopv1.GetDemandRequest) (*dopv1.Demand, error) {
	d, err := s.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return demandToProto(d), nil
}

func (s *DemandServer) StartDemand(ctx context.Context, req *dopv1.StartDemandRequest) (*dopv1.Demand, error) {
	d, err := s.svc.Start(ctx, req.GetProject().GetId(), req.GetExternalKey(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return demandToProto(d), nil
}

func (s *DemandServer) AdvanceStage(ctx context.Context, req *dopv1.AdvanceStageRequest) (*dopv1.DemandStage, error) {
	st, err := s.svc.AdvanceStage(ctx, req.GetDemandId(), req.GetStageKey(),
		stageStatusFromProto(req.GetStatus()), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return stageToProto(st), nil
}

func (s *DemandServer) DecideGate(ctx context.Context, req *dopv1.DecideGateRequest) (*dopv1.DemandStage, error) {
	st, err := s.svc.DecideGate(ctx, req.GetDemandId(), req.GetStageKey(),
		req.GetApproved(), req.GetComment(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return stageToProto(st), nil
}

func (s *DemandServer) ListThreads(ctx context.Context, req *dopv1.ListThreadsRequest) (*dopv1.ListThreadsResponse, error) {
	list, err := s.svc.ListThreads(ctx, req.GetDemandId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Thread, 0, len(list))
	for i := range list {
		out = append(out, threadToProto(&list[i]))
	}
	return &dopv1.ListThreadsResponse{Threads: out}, nil
}

func (s *DemandServer) CreateThread(ctx context.Context, req *dopv1.CreateThreadRequest) (*dopv1.Thread, error) {
	t, err := s.svc.CreateThread(ctx, req.GetDemandId(), req.GetKey(),
		cardFromProto(req.GetCard()), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return threadToProto(t), nil
}

func (s *DemandServer) PostMessage(ctx context.Context, req *dopv1.PostMessageRequest) (*dopv1.Message, error) {
	m, err := s.svc.PostMessage(ctx, req.GetThreadId(), req.GetText(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return messageToProto(m), nil
}

func (s *DemandServer) PublishFinding(ctx context.Context, req *dopv1.PublishFindingRequest) (*dopv1.Finding, error) {
	f, err := s.svc.PublishFinding(ctx, req.GetDemandId(), req.GetThreadId(),
		req.GetTitle(), req.GetPayload().AsMap(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return findingToProto(f), nil
}

func (s *DemandServer) ListFindings(ctx context.Context, req *dopv1.ListFindingsRequest) (*dopv1.ListFindingsResponse, error) {
	findings, err := s.svc.Findings(ctx, req.GetDemandId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Finding, 0, len(findings))
	for i := range findings {
		// The filter by thread lives HERE and not in the repository, on
		// purpose: the query is by demand, and the thread is a slice of the same
		// response. A second database query for a cheap filter does not pay for
		// itself.
		if t := req.GetThreadId(); t != "" && findings[i].ThreadID != t {
			continue
		}
		out = append(out, findingToProto(&findings[i]))
	}
	return &dopv1.ListFindingsResponse{Findings: out}, nil
}

// WatchDemand is pure server-side streaming: the BFF converts it into SSE
// (ADR-0017).
//
// The replay, per-account isolation and slow-consumer policies belong to the
// event service, behind the port — here we only send. Send returns an error when
// the client is gone; the error goes up and the domain tears the subscription
// down.
func (s *DemandServer) WatchDemand(req *dopv1.WatchDemandRequest, stream dopv1.DemandService_WatchDemandServer) error {
	ctx := stream.Context() // contexto de chamada posto por StreamCallContext
	return s.svc.Watch(ctx, req.GetDemandId(), func(e ports.Event) error {
		return stream.Send(&dopv1.DemandEvent{
			Type:      e.Type,
			Aggregate: e.Aggregate,
			Payload:   payloadToStruct(e.Payload),
			At:        timestamppb.New(e.OccurredAt),
		})
	})
}

// ── conversions ──────────────────────────────────────────────────────────────

func demandToProto(d *demand.Demand) *dopv1.Demand {
	if d == nil {
		return nil
	}
	stages := make([]*dopv1.DemandStage, 0, len(d.Stages))
	for i := range d.Stages {
		stages = append(stages, stageToProto(&d.Stages[i]))
	}
	return &dopv1.Demand{
		Id:             d.ID,
		Project:        &dopv1.ProjectRef{Id: d.ProjectID},
		ExternalKey:    d.ExternalKey,
		Title:          d.Title,
		CardType:       d.CardType,
		ProviderStatus: d.ProviderStatus,
		DopStatus:      dopStatusToProto(d.Status),
		FlowId:         d.Flow.FlowID,
		FlowVersion:    d.Flow.Version,
		Stages:         stages,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(d.CreatedAt),
			UpdatedAt: timestamppb.New(d.UpdatedAt),
			CreatedBy: &dopv1.ActorRef{Id: d.CreatedBy},
		},
	}
}

func stageToProto(s *demand.Stage) *dopv1.DemandStage {
	if s == nil {
		return nil
	}
	arts := make([]*dopv1.Artifact, 0, len(s.Artifacts))
	for _, a := range s.Artifacts {
		arts = append(arts, &dopv1.Artifact{
			Id: a.ID, Kind: artifactKindToProto(a.Kind), Name: a.Name,
			ObjectRef: a.ObjectRef, Version: a.Version,
			Audit: &dopv1.AuditStamp{CreatedAt: timestamppb.New(a.CreatedAt)},
		})
	}
	out := &dopv1.DemandStage{
		Key:       s.Key,
		Name:      s.Name,
		Type:      stageTypeToProto(s.Type),
		Status:    stageStatusToProto(s.Status),
		Gate:      gateToProto(s.Gate),
		Artifacts: arts,
	}
	if s.StartedAt != nil {
		out.StartedAt = timestamppb.New(*s.StartedAt)
	}
	if s.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*s.FinishedAt)
	}
	return out
}

func threadToProto(t *demand.Thread) *dopv1.Thread {
	if t == nil {
		return nil
	}
	return &dopv1.Thread{
		Id:     t.ID,
		Demand: &dopv1.DemandRef{Id: t.DemandID},
		Key:    t.Key,
		Card: &dopv1.AgentCard{
			Purpose: t.Card.Purpose, Tools: t.Card.Tools, Model: t.Card.Model,
			Effort: t.Card.Effort, BudgetMicros: t.Card.BudgetMicros,
		},
		// The screen reads `blocked` for the attention box; the thread's full
		// state (open/active/blocked/concluded) has no field in the contract yet
		// — when it has one, it comes out of here without touching the domain.
		Blocked: t.Blocked(),
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(t.CreatedAt),
			UpdatedAt: timestamppb.New(t.UpdatedAt),
			CreatedBy: &dopv1.ActorRef{Id: t.CreatedBy},
		},
	}
}

func cardFromProto(c *dopv1.AgentCard) demand.AgentCard {
	if c == nil {
		return demand.AgentCard{}
	}
	return demand.AgentCard{
		Purpose: c.GetPurpose(), Tools: c.GetTools(), Model: c.GetModel(),
		Effort: c.GetEffort(), BudgetMicros: c.GetBudgetMicros(),
	}
}

func messageToProto(m *demand.Message) *dopv1.Message {
	if m == nil {
		return nil
	}
	return &dopv1.Message{
		Id:       m.ID,
		ThreadId: m.ThreadID,
		Author:   &dopv1.ActorRef{Kind: actorKindToProto(m.AuthorKind), Id: m.AuthorID, Name: m.AuthorName},
		Text:     m.Text,
		At:       timestamppb.New(m.At),
	}
}

func findingToProto(f *demand.Finding) *dopv1.Finding {
	if f == nil {
		return nil
	}
	payload, err := structpb.NewStruct(f.Payload)
	if err != nil {
		// A finding whose payload does not become a Struct (NaN, an exotic type)
		// does not bring the response down: the title and the authorship are
		// worth something on their own, and losing the whole finding would be
		// worse than losing the detail.
		payload = nil
	}
	return &dopv1.Finding{
		Id:       f.ID,
		Demand:   &dopv1.DemandRef{Id: f.DemandID},
		ThreadId: f.ThreadID,
		Title:    f.Title,
		Payload:  payload,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(f.CreatedAt),
			CreatedBy: &dopv1.ActorRef{Id: f.CreatedBy},
		},
	}
}

// ── enums ────────────────────────────────────────────────────────────────────
//
// An explicit translation, with no generic table: the contract's vocabulary and
// the domain's evolve at different rates, and a `switch` that does not compile
// when somebody adds a type is exactly the warning we want.

func dopStatusToProto(s demand.DopStatus) dopv1.DopStatus {
	switch s {
	case demand.StatusNew:
		return dopv1.DopStatus_DOP_STATUS_NEW
	case demand.StatusDoing:
		return dopv1.DopStatus_DOP_STATUS_DOING
	case demand.StatusDone:
		return dopv1.DopStatus_DOP_STATUS_DONE
	case demand.StatusDelivered:
		return dopv1.DopStatus_DOP_STATUS_DELIVERED
	}
	return dopv1.DopStatus_DOP_STATUS_UNSPECIFIED
}

func stageStatusToProto(s demand.StageStatus) dopv1.StageStatus {
	switch s {
	case demand.StagePending:
		return dopv1.StageStatus_STAGE_STATUS_PENDING
	case demand.StageRunning:
		return dopv1.StageStatus_STAGE_STATUS_RUNNING
	case demand.StageBlocked:
		return dopv1.StageStatus_STAGE_STATUS_BLOCKED
	case demand.StageDone:
		return dopv1.StageStatus_STAGE_STATUS_DONE
	}
	return dopv1.StageStatus_STAGE_STATUS_UNSPECIFIED
}

// stageStatusFromProto returns an empty string for UNSPECIFIED on purpose: the
// domain refuses an unknown status with a message that says what arrived, and
// choosing a default here would be the edge deciding a stage transition.
func stageStatusFromProto(s dopv1.StageStatus) demand.StageStatus {
	switch s {
	case dopv1.StageStatus_STAGE_STATUS_PENDING:
		return demand.StagePending
	case dopv1.StageStatus_STAGE_STATUS_RUNNING:
		return demand.StageRunning
	case dopv1.StageStatus_STAGE_STATUS_BLOCKED:
		return demand.StageBlocked
	case dopv1.StageStatus_STAGE_STATUS_DONE:
		return demand.StageDone
	}
	return ""
}

func stageTypeToProto(t demand.StageType) dopv1.StageType {
	switch t {
	case demand.TypeContext:
		return dopv1.StageType_STAGE_TYPE_CONTEXT
	case demand.TypeSpec:
		return dopv1.StageType_STAGE_TYPE_SPEC
	case demand.TypePlan:
		return dopv1.StageType_STAGE_TYPE_PLAN
	case demand.TypeImplementation:
		return dopv1.StageType_STAGE_TYPE_IMPLEMENTATION
	case demand.TypeTest:
		return dopv1.StageType_STAGE_TYPE_TEST
	case demand.TypeHumanValidation:
		return dopv1.StageType_STAGE_TYPE_HUMAN_VALIDATION
	case demand.TypeFinalization:
		return dopv1.StageType_STAGE_TYPE_FINALIZATION
	case demand.TypeGeneric:
		return dopv1.StageType_STAGE_TYPE_GENERIC
	}
	return dopv1.StageType_STAGE_TYPE_UNSPECIFIED
}

func gateToProto(g demand.Gate) dopv1.Gate {
	switch g {
	case demand.GateNone:
		return dopv1.Gate_GATE_NONE
	case demand.GateHuman:
		return dopv1.Gate_GATE_HUMAN
	}
	return dopv1.Gate_GATE_UNSPECIFIED
}

func artifactKindToProto(k demand.ArtifactKind) dopv1.ArtifactKind {
	switch k {
	case demand.ArtifactDocument:
		return dopv1.ArtifactKind_ARTIFACT_KIND_DOCUMENT
	case demand.ArtifactSpec:
		return dopv1.ArtifactKind_ARTIFACT_KIND_SPEC
	case demand.ArtifactPlan:
		return dopv1.ArtifactKind_ARTIFACT_KIND_PLAN
	case demand.ArtifactTestPlan:
		return dopv1.ArtifactKind_ARTIFACT_KIND_TEST_PLAN
	case demand.ArtifactDiagram:
		return dopv1.ArtifactKind_ARTIFACT_KIND_DIAGRAM
	case demand.ArtifactReport:
		return dopv1.ArtifactKind_ARTIFACT_KIND_REPORT
	}
	return dopv1.ArtifactKind_ARTIFACT_KIND_UNSPECIFIED
}

func actorKindToProto(k string) dopv1.ActorRef_Kind {
	switch k {
	case "user":
		return dopv1.ActorRef_KIND_USER
	case "agent":
		return dopv1.ActorRef_KIND_AGENT
	case "subagent":
		return dopv1.ActorRef_KIND_SUBAGENT
	case "system":
		return dopv1.ActorRef_KIND_SYSTEM
	}
	return dopv1.ActorRef_KIND_UNSPECIFIED
}
