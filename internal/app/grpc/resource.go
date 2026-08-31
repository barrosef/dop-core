package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/resource"
)

// ResourceServer expõe o domínio de recurso no contrato gRPC.
//
// Camada FINA: converte tipos, chama o serviço, converte de volta. Repare que
// nenhuma decisão de acesso aparece aqui — quem pode ver, usar ou gerenciar é
// resposta do domínio, e precisa continuar sendo, senão a regra passa a existir
// em dois lugares que divergem com o tempo.
type ResourceServer struct {
	dopv1.UnimplementedResourceServiceServer
	svc *resource.Service
}

func NewResourceServer(svc *resource.Service) *ResourceServer {
	return &ResourceServer{svc: svc}
}

func (s *ResourceServer) ListResources(ctx context.Context, req *dopv1.ListResourcesRequest) (*dopv1.ListResourcesResponse, error) {
	list, err := s.svc.List(ctx, resourceKindFromProto(req.GetKind()))
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Resource, 0, len(list))
	for i := range list {
		out = append(out, resourceToProto(&list[i]))
	}
	// A lista já vem filtrada pelo que o ator pode usar — o total reflete o
	// que ele vê, não o que existe na conta.
	return &dopv1.ListResourcesResponse{
		Resources: out,
		Page:      &dopv1.PageResponse{Total: int32(len(out))},
	}, nil
}

func (s *ResourceServer) GetResource(ctx context.Context, req *dopv1.GetResourceRequest) (*dopv1.Resource, error) {
	r, err := s.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return resourceToProto(r), nil
}

// CreateResource ignora idempotency_key de propósito: a proteção contra
// repetição é do interceptador de idempotência (ADR-0017), e o último anteparo
// é a UNIQUE (account_id, kind, name) do banco, que devolve conflito em vez de
// criar recurso duplicado.
func (s *ResourceServer) CreateResource(ctx context.Context, req *dopv1.CreateResourceRequest) (*dopv1.Resource, error) {
	r, err := s.svc.Create(ctx,
		resourceKindFromProto(req.GetKind()), req.GetName(), req.GetConfig().AsMap())
	if err != nil {
		return nil, err
	}
	return resourceToProto(r), nil
}

func (s *ResourceServer) UpdateResource(ctx context.Context, req *dopv1.UpdateResourceRequest) (*dopv1.Resource, error) {
	r, err := s.svc.Update(ctx, req.GetId(), req.GetConfig().AsMap())
	if err != nil {
		return nil, err
	}
	return resourceToProto(r), nil
}

func (s *ResourceServer) DeleteResource(ctx context.Context, req *dopv1.DeleteResourceRequest) (*dopv1.DeleteResourceResponse, error) {
	if err := s.svc.Delete(ctx, req.GetId()); err != nil {
		return nil, err
	}
	return &dopv1.DeleteResourceResponse{Deleted: true}, nil
}

func (s *ResourceServer) GrantResource(ctx context.Context, req *dopv1.GrantResourceRequest) (*dopv1.ResourceGrant, error) {
	g, err := s.svc.Grant(ctx, req.GetResourceId(), req.GetUserId(), resource.Level(req.GetLevel()))
	if err != nil {
		return nil, err
	}
	return grantToProto(g), nil
}

func (s *ResourceServer) RevokeGrant(ctx context.Context, req *dopv1.RevokeGrantRequest) (*dopv1.RevokeGrantResponse, error) {
	if err := s.svc.RevokeGrant(ctx, req.GetGrantId()); err != nil {
		return nil, err
	}
	return &dopv1.RevokeGrantResponse{Revoked: true}, nil
}

// SetCredential devolve a REFERÊNCIA, nunca o valor. O segredo entra por esta
// RPC e não sai por nenhuma: não existe GetCredential no contrato, e é assim
// que tem de ser (ADR-0001).
func (s *ResourceServer) SetCredential(ctx context.Context, req *dopv1.SetCredentialRequest) (*dopv1.SetCredentialResponse, error) {
	ref, err := s.svc.SetCredential(ctx, req.GetResourceId(), req.GetSecret())
	if err != nil {
		return nil, err
	}
	return &dopv1.SetCredentialResponse{CredentialRef: ref}, nil
}

// ── conversões ───────────────────────────────────────────────────────────────

func resourceToProto(r *resource.Resource) *dopv1.Resource {
	if r == nil {
		return nil
	}
	return &dopv1.Resource{
		Id:      r.ID,
		Account: &dopv1.AccountRef{Id: r.AccountID},
		Kind:    resourceKindToProto(r.Kind),
		Name:    r.Name,
		Version: r.Version,
		Config:  resourceConfigToProto(r.Config),
		// Ponteiro opaco: diz que a integração TEM credencial, não qual é.
		CredentialRef: r.CredentialRef,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(r.CreatedAt),
			UpdatedAt: timestamppb.New(r.UpdatedAt),
			CreatedBy: &dopv1.ActorRef{Kind: dopv1.ActorRef_KIND_USER, Id: r.CreatedBy},
		},
	}
}

func grantToProto(g *resource.Grant) *dopv1.ResourceGrant {
	if g == nil {
		return nil
	}
	return &dopv1.ResourceGrant{
		Id:       g.ID,
		Resource: &dopv1.ResourceRef{Id: g.ResourceID},
		User:     &dopv1.UserRef{Id: g.UserID},
		Level:    string(g.Level),
	}
}

// resourceConfigToProto: config que não vira Struct é config que não veio de
// JSON — impossível pelo caminho do banco, então o nil aqui é defesa, não caso.
func resourceConfigToProto(cfg map[string]any) *structpb.Struct {
	if len(cfg) == 0 {
		return nil
	}
	s, err := structpb.NewStruct(cfg)
	if err != nil {
		return nil
	}
	return s
}

func resourceKindToProto(k resource.Kind) dopv1.Resource_Kind {
	switch k {
	case resource.KindIntegration:
		return dopv1.Resource_KIND_INTEGRATION
	case resource.KindSkill:
		return dopv1.Resource_KIND_SKILL
	case resource.KindWorkflow:
		return dopv1.Resource_KIND_WORKFLOW
	case resource.KindGitFlow:
		return dopv1.Resource_KIND_GIT_FLOW
	}
	return dopv1.Resource_KIND_UNSPECIFIED
}

// resourceKindFromProto devolve "" para KIND_UNSPECIFIED: em ListResources isso
// significa "todos os tipos"; em CreateResource o domínio recusa, porque criar
// recurso sem tipo não é omissão razoável, é requisição incompleta.
func resourceKindFromProto(k dopv1.Resource_Kind) resource.Kind {
	switch k {
	case dopv1.Resource_KIND_INTEGRATION:
		return resource.KindIntegration
	case dopv1.Resource_KIND_SKILL:
		return resource.KindSkill
	case dopv1.Resource_KIND_WORKFLOW:
		return resource.KindWorkflow
	case dopv1.Resource_KIND_GIT_FLOW:
		return resource.KindGitFlow
	}
	return ""
}

var _ dopv1.ResourceServiceServer = (*ResourceServer)(nil)
