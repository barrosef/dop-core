package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/resource"
)

// ResourceServer exposes the resource domain on the gRPC contract.
//
// A THIN layer: it converts types, calls the service, converts back. Note that
// no access decision appears here — who may see, use or manage is the domain's
// answer, and it has to stay that way, or else the rule comes to exist in two
// places that diverge over time.
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
	// The list arrives already filtered by what the actor may use — the total
	// reflects what they see, not what exists in the account.
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

// CreateResource ignores idempotency_key on purpose: the protection against
// repetition belongs to the idempotency interceptor (ADR-0017), and the last
// backstop is the database's UNIQUE (account_id, kind, name), which returns a
// conflict instead of creating a duplicate resource.
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

func (s *ResourceServer) ListMemberGrants(ctx context.Context, req *dopv1.ListMemberGrantsRequest) (*dopv1.ListMemberGrantsResponse, error) {
	grants, err := s.svc.GrantsOfMember(ctx, req.GetUserId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.ResourceGrant, 0, len(grants))
	for i := range grants {
		out = append(out, grantToProto(&grants[i]))
	}
	return &dopv1.ListMemberGrantsResponse{Grants: out}, nil
}

// SetCredential returns the REFERENCE, never the value. The secret goes in
// through this RPC and comes out through none: there is no GetCredential in the
// contract, and that is how it has to be (ADR-0001).
func (s *ResourceServer) SetCredential(ctx context.Context, req *dopv1.SetCredentialRequest) (*dopv1.SetCredentialResponse, error) {
	ref, err := s.svc.SetCredential(ctx, req.GetResourceId(), req.GetSecret())
	if err != nil {
		return nil, err
	}
	return &dopv1.SetCredentialResponse{CredentialRef: ref}, nil
}

// ── conversions ──────────────────────────────────────────────────────────────

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
		// An opaque pointer: it says the integration HAS a credential, not which one.
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

// resourceConfigToProto: a config that does not become a Struct is a config that
// did not come from JSON — impossible through the database's path, so the nil
// here is a defence, not a case.
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

// resourceKindFromProto returns "" for KIND_UNSPECIFIED: in ListResources that
// means "every kind"; in CreateResource the domain refuses, because creating a
// resource with no kind is not a reasonable omission, it is an incomplete
// request.
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
