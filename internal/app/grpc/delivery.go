package grpc

import (
	"context"
	"encoding/json"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
)

// DeliveryServer expõe o domínio de entrega no contrato gRPC.
//
// Camada FINA: converte tipos, chama o serviço, converte de volta. Nenhuma
// regra de entrega aparece aqui — nem a recusa por falta de verde, nem a ordem
// da fila, nem o que uma diretriz pode instruir. Se qualquer uma delas
// aparecesse nesta camada, passaria a existir em dois lugares que divergem com
// o tempo, e o BFF poderia contorná-la chamando outro caminho.
type DeliveryServer struct {
	dopv1.UnimplementedDeliveryServiceServer
	svc *delivery.Service
}

func NewDeliveryServer(svc *delivery.Service) *DeliveryServer {
	return &DeliveryServer{svc: svc}
}

func (s *DeliveryServer) ListPullRequests(ctx context.Context, req *dopv1.ListPullRequestsRequest) (*dopv1.ListPullRequestsResponse, error) {
	list, err := s.svc.ListPullRequests(ctx, delivery.PRFilter{
		DemandID:  req.GetDemandId(),
		ProjectID: req.GetProject().GetId(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.PullRequest, 0, len(list))
	for i := range list {
		out = append(out, pullRequestToProto(&list[i]))
	}
	return &dopv1.ListPullRequestsResponse{PullRequests: out}, nil
}

// GetMergeQueue devolve a fila do repositório JÁ ordenada e numerada pelo
// domínio — o servidor não reordena, nem por conveniência de apresentação.
func (s *DeliveryServer) GetMergeQueue(ctx context.Context, req *dopv1.GetMergeQueueRequest) (*dopv1.GetMergeQueueResponse, error) {
	fila, err := s.svc.GetMergeQueue(ctx, req.GetRepoId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.MergeQueueEntry, 0, len(fila))
	for i := range fila {
		out = append(out, mergeQueueEntryToProto(&fila[i]))
	}
	return &dopv1.GetMergeQueueResponse{Entries: out}, nil
}

// EnqueueMerge é a RPC que a ADR-0007 protege: sem evidência de verde do commit
// atual do PR, a resposta é FailedPrecondition dizendo exatamente o que falta.
// A chave de idempotência do pedido desce até o repositório — repetir a chamada
// devolve a MESMA entrada, não uma segunda posição na fila.
func (s *DeliveryServer) EnqueueMerge(ctx context.Context, req *dopv1.EnqueueMergeRequest) (*dopv1.MergeQueueEntry, error) {
	e, err := s.svc.EnqueueMerge(ctx, req.GetRepoId(), req.GetDemandId(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return mergeQueueEntryToProto(e), nil
}

func (s *DeliveryServer) ListDirectives(ctx context.Context, req *dopv1.ListDirectivesRequest) (*dopv1.ListDirectivesResponse, error) {
	list, err := s.svc.ListDirectives(ctx, req.GetProject().GetId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.Directive, 0, len(list))
	for i := range list {
		out = append(out, directiveToProto(&list[i]))
	}
	return &dopv1.ListDirectivesResponse{Directives: out}, nil
}

// DecideDirective recebe a decisão como Struct — o contrato deixa o formato
// aberto de propósito, e é o domínio que exige as duas chaves obrigatórias
// (`option` e `rationale`). Validar aqui seria duplicar a regra.
func (s *DeliveryServer) DecideDirective(ctx context.Context, req *dopv1.DecideDirectiveRequest) (*dopv1.Directive, error) {
	d, err := s.svc.DecideDirective(ctx,
		req.GetDirectiveId(), req.GetDecision().AsMap(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return directiveToProto(d), nil
}

// ── conversões ───────────────────────────────────────────────────────────────

func pullRequestToProto(pr *delivery.PullRequest) *dopv1.PullRequest {
	if pr == nil {
		return nil
	}
	revs := make([]*dopv1.Reviewer, 0, len(pr.Reviewers))
	for _, r := range pr.Reviewers {
		revs = append(revs, &dopv1.Reviewer{Name: r.Name, Initials: r.Initials, Status: r.Status})
	}
	return &dopv1.PullRequest{
		Id:           pr.ID,
		Demand:       &dopv1.DemandRef{Id: pr.DemandID},
		Repo:         pr.Repo,
		SourceBranch: pr.SourceBranch,
		TargetBranch: pr.TargetBranch,
		Url:          pr.URL,
		Merged:       pr.Merged,
		HasConflict:  pr.HasConflict,
		Reviewers:    revs,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(pr.CreatedAt),
			UpdatedAt: timestamppb.New(pr.UpdatedAt),
		},
	}
}

func mergeQueueEntryToProto(e *delivery.MergeQueueEntry) *dopv1.MergeQueueEntry {
	if e == nil {
		return nil
	}
	return &dopv1.MergeQueueEntry{
		Id:               e.ID,
		RepoId:           e.RepoID,
		Demand:           &dopv1.DemandRef{Id: e.DemandID},
		Position:         e.Position,
		State:            queueStateToProto(e.State),
		OverlappingFiles: e.OverlappingFiles,
	}
}

func queueStateToProto(s delivery.QueueState) dopv1.MergeQueueEntry_State {
	switch s {
	case delivery.StateQueued:
		return dopv1.MergeQueueEntry_STATE_QUEUED
	case delivery.StateRebasing:
		return dopv1.MergeQueueEntry_STATE_REBASING
	case delivery.StateVerifying:
		return dopv1.MergeQueueEntry_STATE_VERIFYING
	case delivery.StateMerged:
		return dopv1.MergeQueueEntry_STATE_MERGED
	case delivery.StateConflict:
		return dopv1.MergeQueueEntry_STATE_CONFLICT
	}
	return dopv1.MergeQueueEntry_STATE_UNSPECIFIED
}

// directiveToProto acomoda no `payload` o que a mensagem do contrato não tem
// campo próprio para carregar: enunciado, opções, recomendação, status e a
// decisão tomada.
//
// O contrato é a fonte da verdade (ADR-0017) — não se inventa campo aqui. E é
// justamente para isso que a mensagem tem um google.protobuf.Struct: a caixa de
// atenção precisa das opções e da recomendação para renderizar um item de
// decisão em vez de um alarme cru, e elas chegam por aqui.
func directiveToProto(d *delivery.Directive) *dopv1.Directive {
	if d == nil {
		return nil
	}
	payload := map[string]any{
		"summary":          d.Summary,
		"status":           string(d.Status),
		"recommended":      d.Recommended,
		"options":          directiveOptionsPayload(d.Options),
		"affected_demands": d.AffectedDemands,
		"signals":          d.Payload,
	}
	var decidedBy *dopv1.ActorRef
	if d.Decision != nil {
		payload["decision"] = map[string]any{
			"option":     d.Decision.Option,
			"rationale":  d.Decision.Rationale,
			"decided_at": d.Decision.DecidedAt,
		}
		decidedBy = &dopv1.ActorRef{
			Kind: deliveryActorKindToProto(ctxutil.ActorKind(d.Decision.ActorKind)),
			Id:   d.Decision.DecidedBy,
		}
	}
	return &dopv1.Directive{
		Id:        d.ID,
		Project:   &dopv1.ProjectRef{Id: d.ProjectID},
		Kind:      directiveKindToProto(d.Kind),
		Payload:   deliveryStruct(payload),
		DecidedBy: decidedBy,
		Audit: &dopv1.AuditStamp{
			CreatedAt: timestamppb.New(d.CreatedAt),
			UpdatedAt: timestamppb.New(d.UpdatedAt),
		},
	}
}

// directiveOptionsPayload escreve as opções em snake_case, com as MESMAS
// chaves que o banco guarda (`key`, `summary`, `instructions`, `demand_id`,
// `action`, `when`). Quem lê a diretriz pelo evento e quem a lê pela RPC
// precisa enxergar a mesma forma — nomes diferentes nos dois caminhos é como o
// cockpit e a caixa de atenção passam a discordar.
func directiveOptionsPayload(opts []delivery.DirectiveOption) []any {
	out := make([]any, 0, len(opts))
	for _, o := range opts {
		ins := make([]any, 0, len(o.Instructions))
		for _, i := range o.Instructions {
			ins = append(ins, map[string]any{
				"demand_id": i.DemandID,
				"action":    string(i.Action),
				"when":      i.When,
				"payload":   i.Payload,
			})
		}
		out = append(out, map[string]any{
			"key": o.Key, "summary": o.Summary, "instructions": ins,
		})
	}
	return out
}

func directiveKindToProto(k delivery.DirectiveKind) dopv1.Directive_Kind {
	switch k {
	case delivery.DirectiveCherryPick:
		return dopv1.Directive_KIND_CHERRY_PICK
	case delivery.DirectiveMergeOrder:
		return dopv1.Directive_KIND_MERGE_ORDER
	case delivery.DirectiveFilePartition:
		return dopv1.Directive_KIND_FILE_PARTITION
	case delivery.DirectiveCrossVerify:
		return dopv1.Directive_KIND_CROSS_VERIFY
	}
	return dopv1.Directive_KIND_UNSPECIFIED
}

// deliveryActorKindToProto tem prefixo porque o pacote grpc já tem um
// conversor de ator com outra assinatura: dois domínios chegaram ao mesmo
// nome, e renomear aqui é mais barato do que amarrar este arquivo à forma que
// o outro escolheu.
func deliveryActorKindToProto(k ctxutil.ActorKind) dopv1.ActorRef_Kind {
	switch k {
	case ctxutil.ActorUser:
		return dopv1.ActorRef_KIND_USER
	case ctxutil.ActorAgent:
		return dopv1.ActorRef_KIND_AGENT
	case ctxutil.ActorSubagent:
		return dopv1.ActorRef_KIND_SUBAGENT
	case ctxutil.ActorSystem:
		return dopv1.ActorRef_KIND_SYSTEM
	}
	return dopv1.ActorRef_KIND_UNSPECIFIED
}

// deliveryStruct converte via JSON porque structpb.NewStruct só aceita os tipos
// primitivos do Struct: uma volta por JSON normaliza slices de struct e tipos
// numéricos de uma vez. Estrutura que não serializa vira Struct vazio em vez de
// derrubar a resposta — o payload é informativo, não é o dado de verdade.
func deliveryStruct(v map[string]any) *structpb.Struct {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

var _ dopv1.DeliveryServiceServer = (*DeliveryServer)(nil)
