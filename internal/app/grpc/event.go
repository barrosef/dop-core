package grpc

import (
	"encoding/json"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

type EventServer struct {
	dopv1.UnimplementedEventServiceServer
	svc *event.Service
}

func NewEventServer(svc *event.Service) *EventServer { return &EventServer{svc: svc} }

// WatchEvents is the service's only method: pure server-side streaming.
//
// The layer stays thin — it translates the request, hands over the Emitter and
// returns whatever the domain decides. The replay, isolation and slow-consumer
// policies live in the domain, not here.
func (s *EventServer) WatchEvents(req *dopv1.WatchEventsRequest, stream dopv1.EventService_WatchEventsServer) error {
	ctx := stream.Context() // contexto de chamada posto por StreamCallContext

	f := event.Filter{Aggregates: req.GetAggregate(), Types: req.GetTypes()}
	return s.svc.Watch(ctx, req.GetSinceEventId(), f, func(e ports.Event) error {
		// Send returns an error when the client is gone; the error goes up and
		// the domain tears the subscription down in its defer. That is how the
		// goroutine dies along with it.
		return stream.Send(eventToProto(e))
	})
}

func eventToProto(e ports.Event) *dopv1.EventEnvelope {
	return &dopv1.EventEnvelope{
		Id:          e.ID,
		Account:     &dopv1.AccountRef{Id: e.AccountID},
		Aggregate:   e.Aggregate,
		AggregateId: e.AggregateID,
		Type:        e.Type,
		Payload:     payloadToStruct(e.Payload),
		OccurredAt:  timestamppb.New(e.OccurredAt),
	}
}

// payloadToStruct converts the jsonb payload into a Struct.
//
// A payload that is unreadable or is not a JSON object does not bring the stream
// down: the event is worth something on its own (id, type, aggregate, instant)
// and the cockpit already knows how to react to it. Losing the whole stream over
// a crooked payload would be worse.
func payloadToStruct(raw []byte) *structpb.Struct {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	st := &structpb.Struct{}
	if err := protojson.Unmarshal(raw, st); err != nil {
		return nil
	}
	return st
}
