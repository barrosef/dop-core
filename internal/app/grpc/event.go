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

// WatchEvents é o único método do serviço: server-side streaming puro.
//
// A camada continua fina — traduz o pedido, entrega o Emitter e devolve o que o
// domínio decidir. A política de replay, de isolamento e de consumidor lento
// mora no domínio, não aqui.
func (s *EventServer) WatchEvents(req *dopv1.WatchEventsRequest, stream dopv1.EventService_WatchEventsServer) error {
	ctx := stream.Context() // contexto de chamada posto por StreamCallContext

	f := event.Filter{Aggregates: req.GetAggregate(), Types: req.GetTypes()}
	return s.svc.Watch(ctx, req.GetSinceEventId(), f, func(e ports.Event) error {
		// Send devolve erro quando o cliente sumiu; o erro sobe e o domínio
		// desmonta a assinatura no defer. É assim que a goroutine morre junto.
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

// payloadToStruct converte o payload jsonb em Struct.
//
// Payload ilegível ou que não seja objeto JSON não derruba o fluxo: o evento
// vale por si (id, tipo, agregado, instante) e o cockpit já sabe reagir a ele.
// Perder o stream inteiro por causa de um payload torto seria pior.
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
