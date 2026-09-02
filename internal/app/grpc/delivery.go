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

// DeliveryServer exposes the delivery domain on the gRPC contract.
//
// A THIN layer: it converts types, calls the service, converts back. No delivery
// rule appears here — not the refusal for want of green, not the queue's order,
// not what a directive may instruct. If any of them appeared in this layer, it
// would come to exist in two places that diverge over time, and the BFF could
// work around it by calling another path.
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

// GetMergeQueue returns the repository's queue ALREADY ordered and numbered by
// the domain — the server does not reorder, not even for presentation's
// convenience.
func (s *DeliveryServer) GetMergeQueue(ctx context.Context, req *dopv1.GetMergeQueueRequest) (*dopv1.GetMergeQueueResponse, error) {
	queue, err := s.svc.GetMergeQueue(ctx, req.GetRepoId())
	if err != nil {
		return nil, err
	}
	out := make([]*dopv1.MergeQueueEntry, 0, len(queue))
	for i := range queue {
		out = append(out, mergeQueueEntryToProto(&queue[i]))
	}
	return &dopv1.GetMergeQueueResponse{Entries: out}, nil
}

// EnqueueMerge is the RPC ADR-0007 protects: with no green evidence for the PR's
// current commit, the answer is FailedPrecondition saying exactly what is
// missing. The request's idempotency key goes all the way down to the repository
// — repeating the call returns the SAME entry, not a second position in the
// queue.
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

// DecideDirective takes the decision as a Struct — the contract leaves the
// format open on purpose, and it is the domain that requires the two mandatory
// keys (`option` and `rationale`). Validating here would duplicate the rule.
func (s *DeliveryServer) DecideDirective(ctx context.Context, req *dopv1.DecideDirectiveRequest) (*dopv1.Directive, error) {
	d, err := s.svc.DecideDirective(ctx,
		req.GetDirectiveId(), req.GetDecision().AsMap(), req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return directiveToProto(d), nil
}

// ── conversions ──────────────────────────────────────────────────────────────

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

// directiveToProto fits into the `payload` what the contract's message has no
// field of its own to carry: the statement, the options, the recommendation, the
// status and the decision taken.
//
// The contract is the source of truth (ADR-0017) — no field is invented here.
// And it is precisely what the message has a google.protobuf.Struct for: the
// attention box needs the options and the recommendation in order to render a
// decision item instead of a raw alarm, and they arrive through here.
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

// directiveOptionsPayload writes the options in snake_case, with the SAME keys
// the database stores (`key`, `summary`, `instructions`, `demand_id`, `action`,
// `when`). Whoever reads the directive through the event and whoever reads it
// through the RPC has to see the same shape — different names on the two paths
// is how the cockpit and the attention box come to disagree.
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

// deliveryActorKindToProto carries a prefix because the grpc package already has
// an actor converter with a different signature: two domains arrived at the same
// name, and renaming here is cheaper than tying this file to the shape the other
// one chose.
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

// deliveryStruct converts through JSON because structpb.NewStruct only accepts
// the Struct's primitive types: a round trip through JSON normalizes struct
// slices and numeric types at once. A structure that does not serialize becomes
// an empty Struct instead of bringing the response down — the payload is
// informative, it is not the real data.
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
