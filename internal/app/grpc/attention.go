package grpc

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/attention"
)

type AttentionServer struct {
	dopv1.UnimplementedAttentionServiceServer
	svc *attention.Service
}

func NewAttentionServer(svc *attention.Service) *AttentionServer {
	return &AttentionServer{svc: svc}
}

func (s *AttentionServer) ListAttention(ctx context.Context, req *dopv1.ListAttentionRequest) (*dopv1.ListAttentionResponse, error) {
	itens, total, err := s.svc.List(ctx, req.GetDemand().GetId(),
		req.GetIncludeResolved(), int(req.GetPage().GetSize()))
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.AttentionItem, 0, len(itens))
	agora := timeNow()
	for _, it := range itens {
		out = append(out, attentionToProto(it, it.Priority(agora)))
	}
	return &dopv1.ListAttentionResponse{
		Items:     out,
		OpenTotal: int32(total),
	}, nil
}

func (s *AttentionServer) WatchAttention(req *dopv1.WatchAttentionRequest, stream dopv1.AttentionService_WatchAttentionServer) error {
	ctx := stream.Context() // contexto de chamada posto por StreamCallContext
	agora := timeNow()
	return s.svc.Watch(ctx, req.GetSinceEventId(), func(change, eventID string, it attention.Item) error {
		c := dopv1.AttentionUpdate_CHANGE_OPENED
		if change == "resolved" {
			c = dopv1.AttentionUpdate_CHANGE_RESOLVED
		}
		return stream.Send(&dopv1.AttentionUpdate{
			Change:  c,
			Item:    attentionToProto(it, it.Priority(agora)),
			EventId: eventID,
		})
	})
}

func attentionToProto(it attention.Item, prioridade int32) *dopv1.AttentionItem {
	msg := &dopv1.AttentionItem{
		Id:         it.ID,
		Account:    &dopv1.AccountRef{Id: it.AccountID},
		Kind:       attentionKindToProto(it.Kind),
		TargetKind: it.TargetKind,
		TargetId:   it.TargetID,
		Title:      it.Title,
		Summary:    it.Summary,
		Priority:   prioridade,
		OpenedAt:   timestamppb.New(it.OpenedAt),
	}
	// Absent ≠ zeroed: an account item (a broken integration) belongs to no
	// demand at all, and an empty DemandRef would make the cockpit group it
	// under a nonexistent demand.
	if it.DemandID != "" {
		msg.Demand = &dopv1.DemandRef{Id: it.DemandID}
	}
	if it.ResolvedAt != nil {
		msg.ResolvedAt = timestamppb.New(*it.ResolvedAt)
	}
	return msg
}

func attentionKindToProto(k attention.Kind) dopv1.AttentionItem_Kind {
	switch k {
	case attention.KindThreadBlocked:
		return dopv1.AttentionItem_KIND_THREAD_BLOCKED
	case attention.KindGatePending:
		return dopv1.AttentionItem_KIND_GATE_PENDING
	case attention.KindPRReview:
		return dopv1.AttentionItem_KIND_PR_REVIEW
	case attention.KindMergeConflict:
		return dopv1.AttentionItem_KIND_MERGE_CONFLICT
	case attention.KindDirective:
		return dopv1.AttentionItem_KIND_DIRECTIVE
	case attention.KindBudgetExceeded:
		return dopv1.AttentionItem_KIND_BUDGET_EXCEEDED
	case attention.KindIntegrationBroken:
		return dopv1.AttentionItem_KIND_INTEGRATION_BROKEN
	default:
		return dopv1.AttentionItem_KIND_UNSPECIFIED
	}
}

// timeNow exists so the priority is computed once per response, and not once
// per item: items of the same response ordered against different clocks could
// come out out of order.
func timeNow() time.Time { return time.Now().UTC() }
