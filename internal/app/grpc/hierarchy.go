package grpc

import (
	"context"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/hierarchy"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type HierarchyServer struct {
	dopv1.UnimplementedHierarchyServiceServer
	svc *hierarchy.Service
}

func NewHierarchyServer(svc *hierarchy.Service) *HierarchyServer {
	return &HierarchyServer{svc: svc}
}

func (s *HierarchyServer) ListWorkspaces(ctx context.Context, _ *dopv1.ListWorkspacesRequest) (*dopv1.ListWorkspacesResponse, error) {
	list, err := s.svc.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Workspace, 0, len(list))
	for i := range list {
		out = append(out, workspaceToProto(&list[i]))
	}
	return &dopv1.ListWorkspacesResponse{Workspaces: out}, nil
}

func (s *HierarchyServer) GetWorkspace(ctx context.Context, req *dopv1.GetWorkspaceRequest) (*dopv1.Workspace, error) {
	w, err := s.svc.GetWorkspace(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return workspaceToProto(w), nil
}

func (s *HierarchyServer) CreateWorkspace(ctx context.Context, req *dopv1.CreateWorkspaceRequest) (*dopv1.Workspace, error) {
	w, err := s.svc.CreateWorkspace(ctx, req.GetName(), req.GetKey(), req.GetDescription(), req.GetTags())
	if err != nil {
		return nil, err
	}
	return workspaceToProto(w), nil
}

func (s *HierarchyServer) UpdateWorkspace(ctx context.Context, req *dopv1.UpdateWorkspaceRequest) (*dopv1.Workspace, error) {
	w, err := s.svc.UpdateWorkspace(ctx, workspaceFromProto(req.GetWorkspace()))
	if err != nil {
		return nil, err
	}
	return workspaceToProto(w), nil
}

func (s *HierarchyServer) ListProjects(ctx context.Context, req *dopv1.ListProjectsRequest) (*dopv1.ListProjectsResponse, error) {
	list, err := s.svc.ListProjects(ctx, req.GetWorkspace().GetId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Project, 0, len(list))
	for i := range list {
		out = append(out, projectToProto(&list[i]))
	}
	return &dopv1.ListProjectsResponse{Projects: out}, nil
}

func (s *HierarchyServer) GetProject(ctx context.Context, req *dopv1.GetProjectRequest) (*dopv1.Project, error) {
	p, err := s.svc.GetProject(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return projectToProto(p), nil
}

func (s *HierarchyServer) CreateProject(ctx context.Context, req *dopv1.CreateProjectRequest) (*dopv1.Project, error) {
	p, err := s.svc.CreateProject(ctx, req.GetWorkspace().GetId(), req.GetName(), req.GetDescription())
	if err != nil {
		return nil, err
	}
	return projectToProto(p), nil
}

func (s *HierarchyServer) UpdateProject(ctx context.Context, req *dopv1.UpdateProjectRequest) (*dopv1.Project, error) {
	p, err := s.svc.UpdateProject(ctx, projectFromProto(req.GetProject()))
	if err != nil {
		return nil, err
	}
	return projectToProto(p), nil
}

// GetTree is a single call on purpose: the cockpit's tree must not cost one
// request per workspace.
func (s *HierarchyServer) GetTree(ctx context.Context, _ *dopv1.GetTreeRequest) (*dopv1.GetTreeResponse, error) {
	nodes, err := s.svc.GetTree(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.GetTreeResponse_Node, 0, len(nodes))
	for i := range nodes {
		projects := make([]*dopv1.Project, 0, len(nodes[i].Projects))
		for j := range nodes[i].Projects {
			projects = append(projects, projectToProto(&nodes[i].Projects[j]))
		}
		out = append(out, &dopv1.GetTreeResponse_Node{
			Workspace: workspaceToProto(&nodes[i].Workspace),
			Projects:  projects,
		})
	}
	return &dopv1.GetTreeResponse{Nodes: out}, nil
}

// ── conversions ──────────────────────────────────────────────────────────────

func workspaceToProto(w *hierarchy.Workspace) *dopv1.Workspace {
	if w == nil {
		return nil
	}
	return &dopv1.Workspace{
		Id:          w.ID,
		Account:     &dopv1.AccountRef{Id: w.AccountID},
		Name:        w.Name,
		Key:         w.Key,
		Description: w.Description,
		Tags:        w.Tags,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(w.CreatedAt),
			UpdatedAt: timestamppb.New(w.UpdatedAt),
		},
	}
}

// workspaceFromProto does NOT read the account from the proto: what rules is the
// context's active account. Accepting the body's account would let the caller
// choose the tenant.
func workspaceFromProto(w *dopv1.Workspace) hierarchy.Workspace {
	if w == nil {
		return hierarchy.Workspace{}
	}
	return hierarchy.Workspace{
		ID:          w.GetId(),
		Name:        w.GetName(),
		Key:         w.GetKey(),
		Description: w.GetDescription(),
		Tags:        w.GetTags(),
	}
}

func projectToProto(p *hierarchy.Project) *dopv1.Project {
	if p == nil {
		return nil
	}
	repos := make([]*dopv1.ProjectRepo, 0, len(p.Repos))
	for _, r := range p.Repos {
		repos = append(repos, &dopv1.ProjectRepo{
			Id:            r.ID,
			Integration:   &dopv1.ResourceRef{Id: r.IntegrationID},
			ExternalId:    r.ExternalID,
			Name:          r.Name,
			DefaultBranch: r.DefaultBranch,
			PrTargets:     r.PRTargets,
		})
	}
	resources := make([]*dopv1.ResourceRef, 0, len(p.Resources))
	for _, id := range p.Resources {
		resources = append(resources, &dopv1.ResourceRef{Id: id})
	}
	var tm *dopv1.ProjectTaskManager
	if p.TaskManager != nil {
		tm = &dopv1.ProjectTaskManager{
			Integration:       &dopv1.ResourceRef{Id: p.TaskManager.IntegrationID},
			ExternalSpaceId:   p.TaskManager.ExternalSpaceID,
			ExternalProjectId: p.TaskManager.ExternalProjectID,
			CardTypes:         p.TaskManager.CardTypes,
		}
	}
	return &dopv1.Project{
		Id:          p.ID,
		Workspace:   &dopv1.WorkspaceRef{Id: p.WorkspaceID},
		Name:        p.Name,
		Description: p.Description,
		Repos:       repos,
		TaskManager: tm,
		Resources:   resources,
		Rules:       p.Rules,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(p.CreatedAt),
			UpdatedAt: timestamppb.New(p.UpdatedAt),
		},
	}
}

func projectFromProto(p *dopv1.Project) hierarchy.Project {
	if p == nil {
		return hierarchy.Project{}
	}
	repos := make([]hierarchy.ProjectRepo, 0, len(p.GetRepos()))
	for _, r := range p.GetRepos() {
		repos = append(repos, hierarchy.ProjectRepo{
			ID:            r.GetId(),
			IntegrationID: r.GetIntegration().GetId(),
			ExternalID:    r.GetExternalId(),
			Name:          r.GetName(),
			DefaultBranch: r.GetDefaultBranch(),
			PRTargets:     r.GetPrTargets(),
		})
	}
	resources := make([]string, 0, len(p.GetResources()))
	for _, ref := range p.GetResources() {
		resources = append(resources, ref.GetId())
	}
	var tm *hierarchy.ProjectTaskManager
	if m := p.GetTaskManager(); m != nil {
		tm = &hierarchy.ProjectTaskManager{
			IntegrationID:     m.GetIntegration().GetId(),
			ExternalSpaceID:   m.GetExternalSpaceId(),
			ExternalProjectID: m.GetExternalProjectId(),
			CardTypes:         m.GetCardTypes(),
		}
	}
	return hierarchy.Project{
		ID:          p.GetId(),
		WorkspaceID: p.GetWorkspace().GetId(),
		Name:        p.GetName(),
		Description: p.GetDescription(),
		Repos:       repos,
		TaskManager: tm,
		Resources:   resources,
		Rules:       p.GetRules(),
	}
}
