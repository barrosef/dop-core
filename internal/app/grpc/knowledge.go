package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/knowledge"
)

// KnowledgeServer exposes the knowledge domain on the gRPC contract.
//
// A THIN layer: it converts types, calls the service, converts back. No curation
// decision appears here — what goes into the context package is the domain's
// answer, and it has to stay that way: the selection rule duplicated at the edge
// is the rule that diverges on the first budget adjustment.
type KnowledgeServer struct {
	dopv1.UnimplementedKnowledgeServiceServer
	svc *knowledge.Service
}

func NewKnowledgeServer(svc *knowledge.Service) *KnowledgeServer {
	return &KnowledgeServer{svc: svc}
}

// BuildContextPackage passes a zeroed budget: the ceiling is the service's,
// chosen in the wiring. The contract has no budget field on purpose — the caller
// is the sandbox provisioning, and letting it choose its own ceiling would turn
// the cost governance (ADR-0011) into a suggestion.
func (s *KnowledgeServer) BuildContextPackage(ctx context.Context, req *dopv1.BuildContextPackageRequest) (*dopv1.ContextPackage, error) {
	pkg, err := s.svc.BuildContextPackage(ctx, req.GetDemandId(), knowledge.Budget{})
	if err != nil {
		return nil, err
	}
	return packageToProto(pkg), nil
}

func (s *KnowledgeServer) SearchMemory(ctx context.Context, req *dopv1.SearchMemoryRequest) (*dopv1.SearchMemoryResponse, error) {
	hits, err := s.svc.SearchMemory(ctx, req.GetProject().GetId(), req.GetQuery(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := &dopv1.SearchMemoryResponse{
		Artifacts: make([]*dopv1.KnowledgeArtifact, 0, len(hits)),
		Scores:    make([]float32, 0, len(hits)),
	}
	// Artifacts and scores travel in PARALLEL lists (it is what the contract
	// asks for): the two loops have to produce the same order, so they are the
	// same loop.
	for i := range hits {
		out.Artifacts = append(out.Artifacts, artifactToProto(&hits[i].Artifact))
		out.Scores = append(out.Scores, hits[i].Score)
	}
	return out, nil
}

func (s *KnowledgeServer) ReadIndex(ctx context.Context, req *dopv1.ReadIndexRequest) (*dopv1.KnowledgeArtifact, error) {
	a, err := s.svc.ReadIndex(ctx, req.GetProject().GetId(), req.GetRepo())
	if err != nil {
		return nil, err
	}
	return artifactToProto(a), nil
}

// PutArtifact carries the idempotency key down to the domain, and from there to
// the transaction. Unlike other writes, here it is NOT redundant with a UNIQUE:
// rewriting the same name in the same scope is a legitimate operation (it bumps
// the version), so without the key a network retry would become a silent new
// version.
func (s *KnowledgeServer) PutArtifact(ctx context.Context, req *dopv1.PutArtifactRequest) (*dopv1.KnowledgeArtifact, error) {
	in := req.GetArtifact()
	a, err := s.svc.PutArtifact(ctx, knowledge.PutInput{
		Kind:           knowledgeKindFromProto(in.GetKind()),
		ProjectID:      in.GetProject().GetId(),
		Name:           in.GetName(),
		Content:        req.GetContent(),
		Meta:           in.GetMeta().AsMap(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, err
	}
	return artifactToProto(a), nil
}

func (s *KnowledgeServer) ListRules(ctx context.Context, req *dopv1.ListRulesRequest) (*dopv1.ListRulesResponse, error) {
	rules, err := s.svc.ListRules(ctx, req.GetProject().GetId())
	if err != nil {
		return nil, err
	}
	return &dopv1.ListRulesResponse{Rules: rules}, nil
}

// ── conversions ──────────────────────────────────────────────────────────────

// metaBodyKey and metaScopeKey are OUR keys inside the artifact's meta.
//
// The KnowledgeArtifact contract has an object_ref and no content field — which
// is right for the large artifact, which the sandbox reads straight from
// storage. But the small artifact lives in the row, and returning it without the
// text would force the caller into a second RPC that does not exist. The "dop."
// prefix avoids colliding with the meta the artifact's author wrote.
const (
	metaBodyKey  = "dop.body"
	metaScopeKey = "dop.scope"
)

func artifactToProto(a *knowledge.Artifact) *dopv1.KnowledgeArtifact {
	if a == nil {
		return nil
	}
	meta := map[string]any{}
	for k, v := range a.Meta {
		meta[k] = v
	}
	meta[metaScopeKey] = string(a.Scope.Level)
	if !a.Externalized() {
		meta[metaBodyKey] = a.Body
	}

	out := &dopv1.KnowledgeArtifact{
		Id:        a.ID,
		Kind:      knowledgeKindToProto(a.Kind),
		Name:      a.Name,
		Version:   a.Version,
		ObjectRef: a.ObjectRef,
		Meta:      knowledgeMetaToProto(meta),
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(a.CreatedAt),
			UpdatedAt: timestamppb.New(a.UpdatedAt),
			CreatedBy: &dopv1.ActorRef{Kind: dopv1.ActorRef_KIND_USER, Id: a.CreatedBy},
		},
	}
	// An artifact in the account or workspace scope has no project — and the
	// field stays empty instead of lying with some id.
	if a.Scope.ProjectID != "" {
		out.Project = &dopv1.ProjectRef{Id: a.Scope.ProjectID}
	}
	return out
}

// packageToProto invents no timestamp and no volatile id: the package enters the
// prompt's cached prefix, and a byte that changes on every assembly burns the
// cache discount in silence (ADR-0012 §1).
func packageToProto(p *knowledge.Package) *dopv1.ContextPackage {
	if p == nil {
		return nil
	}
	out := &dopv1.ContextPackage{
		Demand:          &dopv1.DemandRef{Id: p.DemandID},
		Rules:           p.Rules,
		Index:           make([]*dopv1.KnowledgeArtifact, 0, len(p.Index)),
		Memories:        make([]*dopv1.KnowledgeArtifact, 0, len(p.Memories)),
		Findings:        make([]*dopv1.Finding, 0, len(p.Findings)),
		EstimatedTokens: int32(p.EstimatedTokens),
		// What was dropped per layer travels ALWAYS, zeroed included: "nothing
		// was dropped" and "I cannot say" are different facts, and the empty map
		// already means the second. Without this the screen has no way to warn
		// that the context was truncated, and pretends everything fitted.
		Dropped: map[string]int32{
			"rules":    int32(p.Dropped.Rules),
			"findings": int32(p.Dropped.Findings),
			"index":    int32(p.Dropped.Index),
			"memories": int32(p.Dropped.Memories),
		},
	}
	for i := range p.Index {
		out.Index = append(out.Index, artifactToProto(&p.Index[i]))
	}
	for i := range p.Memories {
		out.Memories = append(out.Memories, artifactToProto(&p.Memories[i]))
	}
	for _, f := range p.Findings {
		out.Findings = append(out.Findings, &dopv1.Finding{
			Id:       f.ID,
			Demand:   &dopv1.DemandRef{Id: p.DemandID},
			ThreadId: f.ThreadID,
			Title:    f.Title,
			Payload:  knowledgeMetaToProto(map[string]any{"summary": f.Summary}),
		})
	}
	return out
}

// knowledgeMetaToProto: meta that does not become a Struct is meta that did not
// come from JSON — impossible through the database's path, so the nil here is a
// defence, not a case.
func knowledgeMetaToProto(m map[string]any) *structpb.Struct {
	if len(m) == 0 {
		return nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

func knowledgeKindToProto(k knowledge.Kind) dopv1.KnowledgeArtifact_Kind {
	switch k {
	case knowledge.KindRule:
		return dopv1.KnowledgeArtifact_KIND_RULE
	case knowledge.KindIndex:
		return dopv1.KnowledgeArtifact_KIND_INDEX
	case knowledge.KindMemory:
		return dopv1.KnowledgeArtifact_KIND_MEMORY
	}
	return dopv1.KnowledgeArtifact_KIND_UNSPECIFIED
}

// knowledgeKindFromProto returns "" for KIND_UNSPECIFIED, and the domain
// refuses: writing knowledge without saying whether it is a rule, an index or a
// memory is not a reasonable omission — it is the difference between what the
// agent obeys and what it consults.
func knowledgeKindFromProto(k dopv1.KnowledgeArtifact_Kind) knowledge.Kind {
	switch k {
	case dopv1.KnowledgeArtifact_KIND_RULE:
		return knowledge.KindRule
	case dopv1.KnowledgeArtifact_KIND_INDEX:
		return knowledge.KindIndex
	case dopv1.KnowledgeArtifact_KIND_MEMORY:
		return knowledge.KindMemory
	}
	return ""
}

var _ dopv1.KnowledgeServiceServer = (*KnowledgeServer)(nil)
