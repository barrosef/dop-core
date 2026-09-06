package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
)

// WorkflowServer is the flow domain's EDGE: it translates proto into domain and
// back. No rule lives here — what decides is the Service.
type WorkflowServer struct {
	dopv1.UnimplementedWorkflowServiceServer
	svc *workflow.Service
}

func NewWorkflowServer(svc *workflow.Service) *WorkflowServer {
	return &WorkflowServer{svc: svc}
}

func (s *WorkflowServer) ListFlows(ctx context.Context, req *dopv1.ListFlowsRequest) (*dopv1.ListFlowsResponse, error) {
	list, err := s.svc.List(ctx, workflow.Scope(req.GetOwnerScope()), req.GetOwnerId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Flow, 0, len(list))
	for i := range list {
		out = append(out, flowToProto(&list[i]))
	}
	return &dopv1.ListFlowsResponse{Flows: out}, nil
}

func (s *WorkflowServer) GetFlow(ctx context.Context, req *dopv1.GetFlowRequest) (*dopv1.Flow, error) {
	f, err := s.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return flowToProto(f), nil
}

func (s *WorkflowServer) CreateFlow(ctx context.Context, req *dopv1.CreateFlowRequest) (*dopv1.Flow, error) {
	f, err := s.svc.Create(ctx, flowFromProto(req.GetFlow()), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return flowToProto(f), nil
}

// UpdateFlow GENERATES A NEW VERSION. The `version` coming in the body is the
// version the author edited against — it is what allows refusing a write made on
// top of an outdated document instead of erasing the work of whoever wrote
// before.
func (s *WorkflowServer) UpdateFlow(ctx context.Context, req *dopv1.UpdateFlowRequest) (*dopv1.Flow, error) {
	f, err := s.svc.Update(ctx, flowFromProto(req.GetFlow()))
	if err != nil {
		return nil, err
	}
	return flowToProto(f), nil
}

// ValidateFlow returns the report, not an error: the client asked for a dry run,
// and the screen needs the list of problems to mark the stages — not a failure
// message to show in a toast.
func (s *WorkflowServer) ValidateFlow(ctx context.Context, req *dopv1.ValidateFlowRequest) (*dopv1.ValidateFlowResponse, error) {
	rep, err := s.svc.Validate(ctx, flowFromProto(req.GetFlow()))
	if err != nil {
		return nil, err
	}
	return &dopv1.ValidateFlowResponse{
		Valid:    rep.Valid(),
		Errors:   rep.Errors,
		Warnings: rep.Warnings,
	}, nil
}

func (s *WorkflowServer) ResolveFlow(ctx context.Context, req *dopv1.ResolveFlowRequest) (*dopv1.EffectiveFlow, error) {
	eff, err := s.svc.Resolve(ctx, workflow.Scope(req.GetScope()), req.GetScopeId())
	if err != nil {
		return nil, err
	}
	// The provenance goes out STRUCTURED, besides the sentence. The sentence
	// stays for logs and error messages; the structure exists so the consumer
	// does not have to interpret text — a contract that forces parsing breaks
	// the day somebody improves the wording.
	contribuintes := make([]*dopv1.ScopeRef, 0, len(eff.Contributors))
	for _, c := range eff.Contributors {
		contribuintes = append(contribuintes, &dopv1.ScopeRef{Scope: string(c.Scope), Id: c.ID})
	}
	origens := make([]*dopv1.StageOrigin, 0, len(eff.Origins))
	for _, o := range eff.Origins {
		origens = append(origens, &dopv1.StageOrigin{
			Key: o.Key, Scope: string(o.From.Scope), ScopeId: o.From.ID,
		})
	}
	return &dopv1.EffectiveFlow{
		Flow:         flowToProto(&eff.Flow),
		ResolvedFrom: eff.ResolvedFrom,
		Contributors: contribuintes,
		Origins:      origens,
	}, nil
}

func (s *WorkflowServer) PromoteFlow(ctx context.Context, req *dopv1.PromoteFlowRequest) (*dopv1.Flow, error) {
	f, err := s.svc.Promote(ctx, req.GetFlowId(),
		workflow.Scope(req.GetTargetScope()), req.GetTargetId())
	if err != nil {
		return nil, err
	}
	return flowToProto(f), nil
}

// ── sharing ──────────────────────────────────────────────────────────────────

// PublishFlow freezes the flow's current version under @handle/slug. The
// reference in the response is RENDERED HERE, from the caller's own handle
// (Publish always publishes to the caller's own account) plus the slug and
// version the domain just decided — never assembled by the client, which
// would be three places for the format to drift instead of one.
//
// Publish already wrote the publication durably by the time HandleOf runs, so
// a failure here (the handle lookup, not the write) reports an error for a
// write that succeeded. Accepted trade-off: PublishFlowRequest carries an
// idempotency_key, so the caller's retry replays onto the same row instead of
// creating a second one — it does not lose the publication, only the first
// response.
func (s *WorkflowServer) PublishFlow(ctx context.Context, req *dopv1.PublishFlowRequest) (*dopv1.FlowPublication, error) {
	p, err := s.svc.Publish(ctx, req.GetFlowId(), req.GetSlug(), req.GetNotes(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	handle, err := s.svc.HandleOf(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	return publicationToProto(p, handle), nil
}

func (s *WorkflowServer) WithdrawFlow(ctx context.Context, req *dopv1.WithdrawFlowRequest) (*emptypb.Empty, error) {
	if err := s.svc.Withdraw(ctx, req.GetPublicationId()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *WorkflowServer) GrantFlow(ctx context.Context, req *dopv1.GrantFlowRequest) (*dopv1.FlowGrant, error) {
	sh, err := s.svc.Grant(ctx, req.GetPublicationId(), req.GetToAccount().GetId(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return shareToProto(sh), nil
}

func (s *WorkflowServer) RevokeFlowGrant(ctx context.Context, req *dopv1.RevokeFlowGrantRequest) (*emptypb.Empty, error) {
	if err := s.svc.Revoke(ctx, req.GetShareId()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// DeriveFlow adopts a published flow into the caller's own account. The
// reference the caller typed is parsed by the DOMAIN (workflow.ParseRef, via
// Service.Derive) — its refusals already name what is wrong with the string
// (a missing @, an unusable handle, a malformed version), and returning them
// unchanged is what lets a person act on them instead of a generic parse
// error.
func (s *WorkflowServer) DeriveFlow(ctx context.Context, req *dopv1.DeriveFlowRequest) (*dopv1.Flow, error) {
	target := workflow.ScopeRef{
		Scope: workflow.Scope(req.GetTarget().GetScope()),
		ID:    req.GetTarget().GetId(),
	}
	f, err := s.svc.Derive(ctx, req.GetReference(), target, req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return flowToProto(f), nil
}

func (s *WorkflowServer) BumpFlowPin(ctx context.Context, req *dopv1.BumpFlowPinRequest) (*emptypb.Empty, error) {
	if err := s.svc.BumpPin(ctx, req.GetFlowId(), req.GetVersion()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *WorkflowServer) ListFlowShares(ctx context.Context, req *dopv1.ListFlowSharesRequest) (*dopv1.ListFlowSharesResponse, error) {
	shares, err := s.svc.SharesOf(ctx, req.GetPublicationId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.FlowGrant, 0, len(shares))
	for i := range shares {
		out = append(out, shareToProto(&shares[i]))
	}
	return &dopv1.ListFlowSharesResponse{Shares: out}, nil
}

func (s *WorkflowServer) ListFlowAdoptions(ctx context.Context, req *dopv1.ListFlowAdoptionsRequest) (*dopv1.ListFlowAdoptionsResponse, error) {
	adoptions, err := s.svc.AdoptionsOf(ctx, req.GetPublicationId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.FlowAdoption, 0, len(adoptions))
	for i := range adoptions {
		out = append(out, adoptionToProto(&adoptions[i]))
	}
	return &dopv1.ListFlowAdoptionsResponse{Adoptions: out}, nil
}

// ── conversions ──────────────────────────────────────────────────────────────

func flowToProto(f *workflow.Flow) *dopv1.Flow {
	if f == nil {
		return nil
	}
	stages := make([]*dopv1.StageSpec, 0, len(f.Stages))
	for _, st := range f.Stages {
		artifacts := make([]dopv1.ArtifactKind, 0, len(st.Artifacts))
		for _, a := range st.Artifacts {
			artifacts = append(artifacts, flowArtifactToProto(a))
		}
		stages = append(stages, &dopv1.StageSpec{
			Key:       st.Key,
			Name:      st.Name,
			Type:      flowStageTypeToProto(st.Type),
			Artifacts: artifacts,
			Gate:      flowGateToProto(st.Gate),
			Subtypes:  st.Subtypes,
		})
	}
	out := &dopv1.Flow{
		Id:          f.ID,
		Name:        f.Name,
		Description: f.Description,
		Version:     f.Version,
		OwnerScope:  string(f.OwnerScope),
		OwnerId:     f.OwnerID,
		Stages:      stages,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(f.CreatedAt),
			UpdatedAt: timestamppb.New(f.UpdatedAt),
		},
	}
	if f.CreatedBy != "" {
		out.Audit.CreatedBy = &dopv1.ActorRef{Kind: dopv1.ActorRef_KIND_USER, Id: f.CreatedBy}
	}
	return out
}

// flowFromProto does NOT read the account from the proto — Flow does not even
// carry it, and that is how it should be: what rules is the context's active
// account. It also ignores the audit stamp: the one who writes created_at is the
// server, not the client.
func flowFromProto(f *dopv1.Flow) workflow.Flow {
	if f == nil {
		return workflow.Flow{}
	}
	stages := make([]workflow.StageSpec, 0, len(f.GetStages()))
	for _, st := range f.GetStages() {
		artifacts := make([]workflow.ArtifactKind, 0, len(st.GetArtifacts()))
		for _, a := range st.GetArtifacts() {
			artifacts = append(artifacts, flowArtifactFromProto(a))
		}
		stages = append(stages, workflow.StageSpec{
			Key:       st.GetKey(),
			Name:      st.GetName(),
			Type:      flowStageTypeFromProto(st.GetType()),
			Artifacts: artifacts,
			Gate:      flowGateFromProto(st.GetGate()),
			Subtypes:  st.GetSubtypes(),
		})
	}
	return workflow.Flow{
		ID:          f.GetId(),
		Name:        f.GetName(),
		Description: f.GetDescription(),
		Version:     f.GetVersion(),
		OwnerScope:  workflow.Scope(f.GetOwnerScope()),
		OwnerID:     f.GetOwnerId(),
		Stages:      stages,
	}
}

// The tables below are the vocabulary's frontier: the enum belongs to the
// CONTRACT, the string to the domain. A value the contract does not know becomes
// the enum's zero (UNSPECIFIED) and goes back to the client as an unknown type —
// which is the truth, and not a plausible value invented at the edge.
var flowStageTypes = map[workflow.StageType]dopv1.StageType{
	workflow.TypeContext:         dopv1.StageType_STAGE_TYPE_CONTEXT,
	workflow.TypeSpec:            dopv1.StageType_STAGE_TYPE_SPEC,
	workflow.TypePlan:            dopv1.StageType_STAGE_TYPE_PLAN,
	workflow.TypeImplementation:  dopv1.StageType_STAGE_TYPE_IMPLEMENTATION,
	workflow.TypeTest:            dopv1.StageType_STAGE_TYPE_TEST,
	workflow.TypeHumanValidation: dopv1.StageType_STAGE_TYPE_HUMAN_VALIDATION,
	workflow.TypeFinalization:    dopv1.StageType_STAGE_TYPE_FINALIZATION,
	workflow.TypeGeneric:         dopv1.StageType_STAGE_TYPE_GENERIC,
}

func flowStageTypeToProto(t workflow.StageType) dopv1.StageType {
	return flowStageTypes[t]
}

func flowStageTypeFromProto(t dopv1.StageType) workflow.StageType {
	for domain, proto := range flowStageTypes {
		if proto == t {
			return domain
		}
	}
	return ""
}

var flowArtifactKinds = map[workflow.ArtifactKind]dopv1.ArtifactKind{
	workflow.ArtifactDocument: dopv1.ArtifactKind_ARTIFACT_KIND_DOCUMENT,
	workflow.ArtifactSpec:     dopv1.ArtifactKind_ARTIFACT_KIND_SPEC,
	workflow.ArtifactPlan:     dopv1.ArtifactKind_ARTIFACT_KIND_PLAN,
	workflow.ArtifactTestPlan: dopv1.ArtifactKind_ARTIFACT_KIND_TEST_PLAN,
	workflow.ArtifactDiagram:  dopv1.ArtifactKind_ARTIFACT_KIND_DIAGRAM,
	workflow.ArtifactReport:   dopv1.ArtifactKind_ARTIFACT_KIND_REPORT,
}

func flowArtifactToProto(a workflow.ArtifactKind) dopv1.ArtifactKind {
	return flowArtifactKinds[a]
}

func flowArtifactFromProto(a dopv1.ArtifactKind) workflow.ArtifactKind {
	for domain, proto := range flowArtifactKinds {
		if proto == a {
			return domain
		}
	}
	return ""
}

// An undeclared gate becomes "none": it is the domain's same default, and having
// both sides agree stops silence from meaning different things at the edge and
// in the core.
func flowGateToProto(g workflow.GateKind) dopv1.Gate {
	if g == workflow.GateHuman {
		return dopv1.Gate_GATE_HUMAN
	}
	return dopv1.Gate_GATE_NONE
}

func flowGateFromProto(g dopv1.Gate) workflow.GateKind {
	switch g {
	case dopv1.Gate_GATE_HUMAN:
		return workflow.GateHuman
	case dopv1.Gate_GATE_NONE, dopv1.Gate_GATE_UNSPECIFIED:
		return workflow.GateNone
	}
	return workflow.GateKind("")
}

// publicationToProto renders `reference` from the caller's handle plus the
// publication's own slug and version — the ONE place this format is built,
// via PublicationRef.String, the same formatter Derive's provenance uses.
func publicationToProto(p *workflow.Publication, handle string) *dopv1.FlowPublication {
	ref := workflow.PublicationRef{Handle: handle, Slug: p.Slug, Version: p.Version}
	out := &dopv1.FlowPublication{
		Id:          p.ID,
		FlowId:      p.FlowID,
		Reference:   ref.String(),
		Version:     p.Version,
		Notes:       p.Notes,
		PublishedAt: timestamppb.New(p.PublishedAt),
	}
	if !p.WithdrawnAt.IsZero() {
		out.WithdrawnAt = timestamppb.New(p.WithdrawnAt)
	}
	return out
}

func shareToProto(s *workflow.Share) *dopv1.FlowGrant {
	out := &dopv1.FlowGrant{
		Id:               s.ID,
		PublicationId:    s.PublicationID,
		ToAccount:        &dopv1.AccountRef{Id: s.ToAccountID},
		RevocationPolicy: string(s.RevocationPolicy),
		GrantedAt:        timestamppb.New(s.GrantedAt),
	}
	if !s.RevokedAt.IsZero() {
		out.RevokedAt = timestamppb.New(s.RevokedAt)
	}
	return out
}

func adoptionToProto(a *workflow.Adoption) *dopv1.FlowAdoption {
	out := &dopv1.FlowAdoption{
		Id:            a.ID,
		PublicationId: a.PublicationID,
		Version:       a.Version,
		ByAccount:     &dopv1.AccountRef{Id: a.ByAccountID},
		FlowId:        a.FlowID,
		DerivedAt:     timestamppb.New(a.DerivedAt),
	}
	if !a.RevokedAt.IsZero() {
		out.RevokedAt = timestamppb.New(a.RevokedAt)
	}
	return out
}

var _ dopv1.WorkflowServiceServer = (*WorkflowServer)(nil)
