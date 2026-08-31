package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
)

// KnowledgeServer expõe o domínio de conhecimento no contrato gRPC.
//
// Camada FINA: converte tipos, chama o serviço, converte de volta. Nenhuma
// decisão de curadoria aparece aqui — o que entra no pacote de contexto é
// resposta do domínio, e precisa continuar sendo: a regra de seleção duplicada
// na borda é a regra que diverge no primeiro ajuste de orçamento.
type KnowledgeServer struct {
	dopv1.UnimplementedKnowledgeServiceServer
	svc *knowledge.Service
}

func NewKnowledgeServer(svc *knowledge.Service) *KnowledgeServer {
	return &KnowledgeServer{svc: svc}
}

// BuildContextPackage passa orçamento zerado: o teto é o do serviço, escolhido
// na fiação. O contrato não tem campo de orçamento de propósito — quem chama é
// o provisionamento do sandbox, e deixá-lo escolher o próprio teto tornaria a
// governança de custo (ADR-0011) uma sugestão.
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
	// Artefatos e scores viajam em listas PARALELAS (é o que o contrato pede):
	// os dois laços precisam produzir a mesma ordem, então são o mesmo laço.
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

// PutArtifact carrega a chave de idempotência até o domínio, e de lá até a
// transação. Diferente de outras escritas, aqui ela NÃO é redundante com uma
// UNIQUE: regravar o mesmo nome no mesmo escopo é operação legítima (bumpa a
// versão), então sem a chave um retry de rede viraria versão nova silenciosa.
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

// ── conversões ───────────────────────────────────────────────────────────────

// metaBodyKey e metaScopeKey são chaves NOSSAS dentro do meta do artefato.
//
// O contrato do KnowledgeArtifact tem object_ref e não tem campo de conteúdo —
// o que está certo para o artefato grande, que o sandbox lê direto do storage.
// Mas o artefato pequeno mora na linha, e devolvê-lo sem o texto obrigaria o
// chamador a uma segunda RPC que não existe. O prefixo "dop." evita colisão com
// o meta que o autor do artefato escreveu.
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
	// Artefato de escopo de conta ou de workspace não tem projeto — e o campo
	// fica vazio em vez de mentir com um id qualquer.
	if a.Scope.ProjectID != "" {
		out.Project = &dopv1.ProjectRef{Id: a.Scope.ProjectID}
	}
	return out
}

// packageToProto não inventa timestamp nem id volátil: o pacote entra no
// prefixo cacheado do prompt, e um byte que muda a cada montagem queima o
// desconto de cache em silêncio (ADR-0012 §1).
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

// knowledgeMetaToProto: meta que não vira Struct é meta que não veio de JSON —
// impossível pelo caminho do banco, então o nil aqui é defesa, não caso.
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

// knowledgeKindFromProto devolve "" para KIND_UNSPECIFIED, e o domínio recusa:
// gravar conhecimento sem dizer se é regra, índice ou memória não é omissão
// razoável — é a diferença entre o que o agente obedece e o que ele consulta.
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
