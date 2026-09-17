// Package notifier connects the EVENT SPINE to the communication trigger.
//
// It plays the same role — and takes the same shape — as
// adapter/postgres/projection/attention: unwrap the envelope that travelled the
// wire and hand the domain an event in domain vocabulary. The decision of what
// to notify lives in internal/domain/notification; only the translation happens
// here.
//
// The separation is not ceremony: the envelope is JSON because the bus is JSON,
// and a domain that knew that could not be tested without inventing bytes.
package notifier

import (
	"context"
	"encoding/json"
	"time"

	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
)

// Consumer is the subscriber. Idempotent by construction — the real
// idempotency lives in the service's claim, and JetStream delivery is
// at-least-once (ADR-0014).
type Consumer struct{ svc *notification.Service }

func NewConsumer(svc *notification.Service) *Consumer {
	if svc == nil {
		panic("notifier.NewConsumer: service is required")
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
		// An unreadable event never improves with a retry — and a poison
		// message must not block the queue (EventBus guarantee 8).
		return nil
	}
	// An event with no account (`user.ensured`, migration 0003) belongs to no
	// notification: there are no members to warn and no account to warn on
	// behalf of.
	if env.AccountID == "" {
		return nil
	}

	// The consumer runs as SYSTEM, under the event's account — the same Call
	// the interceptor would build for a user request. That is what keeps the
	// per-account filter in force inside the worker, with no exception carved
	// out for it.
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
