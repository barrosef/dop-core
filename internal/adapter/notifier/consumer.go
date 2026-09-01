// Package notifier liga a ESPINHA DE EVENTOS ao gatilho de comunicação.
//
// É o mesmo papel — e a mesma forma — de adapter/postgres/projection/attention:
// desembrulhar o envelope que trafegou no fio e entregar ao domínio um evento
// em vocabulário de domínio. A decisão do que notificar mora em
// internal/domain/notification; aqui só acontece a tradução.
//
// A separação não é cerimônia: o envelope é JSON porque o barramento é JSON, e
// um domínio que soubesse disso não conseguiria ser testado sem inventar bytes.
package notifier

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
)

// Consumer é o assinante. Idempotente por construção — a idempotência real
// mora na reserva do serviço, e a entrega do JetStream é ao-menos-uma-vez
// (ADR-0019).
type Consumer struct{ svc *notification.Service }

func NewConsumer(svc *notification.Service) *Consumer {
	if svc == nil {
		panic("notifier.NewConsumer: serviço obrigatório")
	}
	return &Consumer{svc: svc}
}

func (c *Consumer) Handle(ctx context.Context, e ports.Event) error {
	var env struct {
		ID          string         `json:"id"`
		AccountID   string         `json:"account_id"`
		Aggregate   string         `json:"aggregate"`
		AggregateID string         `json:"aggregate_id"`
		Type        string         `json:"type"`
		Payload     map[string]any `json:"payload"`
		OccurredAt  time.Time      `json:"occurred_at"`
	}
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		// Evento ilegível nunca melhora com retry — e mensagem venenosa não
		// pode travar a fila (garantia 8 do EventBus).
		return nil
	}
	// Evento sem conta (`user.ensured`, migração 0003) não pertence a
	// notificação nenhuma: não há membros a avisar nem conta em nome de quem
	// avisar.
	if env.AccountID == "" {
		return nil
	}

	// O consumidor roda como SISTEMA, com a conta do evento — o mesmo Call que
	// o interceptor montaria numa chamada de usuário. É isso que faz o filtro
	// por conta continuar valendo dentro do worker, sem abrir exceção.
	ctx = ctxutil.Into(ctx, ctxutil.Call{
		AccountID: env.AccountID,
		ActorID:   "notifier",
		ActorKind: ctxutil.ActorSystem,
		ActorName: "notifier",
	})

	return c.svc.HandleEvent(ctx, notification.Event{
		ID: env.ID, AccountID: env.AccountID, Aggregate: env.Aggregate,
		AggregateID: env.AggregateID, Type: env.Type,
		OccurredAt: env.OccurredAt, Payload: env.Payload,
	})
}
