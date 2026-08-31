package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
)

// WorkflowServer é a BORDA do domínio de fluxo: traduz proto para domínio e
// devolve. Nenhuma regra mora aqui — o que decide é o Service.
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

// UpdateFlow GERA VERSÃO NOVA. O `version` que vem no corpo é a versão sobre a
// qual o autor editou — é o que permite recusar uma gravação feita em cima de
// um documento desatualizado em vez de apagar o trabalho de quem gravou antes.
func (s *WorkflowServer) UpdateFlow(ctx context.Context, req *dopv1.UpdateFlowRequest) (*dopv1.Flow, error) {
	f, err := s.svc.Update(ctx, flowFromProto(req.GetFlow()))
	if err != nil {
		return nil, err
	}
	return flowToProto(f), nil
}

// ValidateFlow devolve o relatório, não um erro: o cliente pediu um ensaio, e a
// tela precisa da lista de problemas para marcar as etapas — não de uma
// mensagem de falha para exibir num toast.
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
	// A procedência sai ESTRUTURADA, além da frase. A frase continua para log
	// e mensagem de erro; a estrutura existe para o consumidor não ter que
	// interpretar texto — contrato que obriga parsing quebra no dia em que
	// alguém melhora a redação.
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

// ── conversões ───────────────────────────────────────────────────────────────

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

// flowFromProto NÃO lê a conta do proto — Flow nem a carrega, e é assim que
// deve ser: quem manda é a conta ativa do contexto. Também ignora o carimbo de
// auditoria: quem escreve created_at é o servidor, não o cliente.
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

// As tabelas abaixo são a fronteira do vocabulário: o enum é do CONTRATO, a
// string é do domínio. Um valor que o contrato não conhece vira o zero do enum
// (UNSPECIFIED) e volta ao cliente como tipo desconhecido — que é a verdade,
// e não um valor plausível inventado na borda.
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

// Portão não declarado vira "nenhum": é o mesmo default do domínio, e ter os
// dois lados concordando evita que o silêncio signifique coisas diferentes na
// borda e no núcleo.
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

var _ dopv1.WorkflowServiceServer = (*WorkflowServer)(nil)
