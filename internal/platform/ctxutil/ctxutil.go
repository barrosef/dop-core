// Package ctxutil carries the call context — who, in which account — across
// every layer, without the domain having to take it as a parameter.
package ctxutil

import (
	"context"
	"errors"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

type ActorKind string

const (
	ActorUser     ActorKind = "user"
	ActorAgent    ActorKind = "agent"
	ActorSubagent ActorKind = "subagent"
	ActorSystem   ActorKind = "system"
)

// Call is the mandatory context of every domain operation.
type Call struct {
	RequestID string
	AccountID string
	ActorID   string
	ActorKind ActorKind
	ActorName string
	// SessionID identifies the CALLER'S SESSION, and it exists for the second
	// factor: the step-up is per (user, session), because two open sessions are
	// two doors and one of them answering must not open the other (ADR-0027 §5).
	//
	// It comes from the edge, like every other field here — with the limit P-18
	// describes, which this feature makes load-bearing: the core trusts the
	// metadata, and the NetworkPolicy is what makes the assumption hold.
	SessionID string
}

// ErrNoAccount signals a request with no active account — invalid by
// definition (the SP-0 rule, materialized here and in the BFF's
// @account_scoped).
var ErrNoAccount = errors.New("request without an active account")

// A request with no active account is a CLIENT error, not an internal failure:
// it must surface as 400, never as 500.
func init() {
	errs.RegisterClassifier(func(err error) (errs.Kind, bool) {
		if errors.Is(err, ErrNoAccount) {
			return errs.KindInvalid, true
		}
		return "", false
	})
}

type ctxKey struct{}

func Into(ctx context.Context, c Call) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

func From(ctx context.Context) (Call, bool) {
	c, ok := ctx.Value(ctxKey{}).(Call)
	return c, ok
}

// MustAccount returns the active account or ErrNoAccount. Every repository
// filters by it — multi-tenant isolation is a constraint, not a convention.
func MustAccount(ctx context.Context) (string, error) {
	c, ok := From(ctx)
	if !ok || c.AccountID == "" {
		return "", ErrNoAccount
	}
	return c.AccountID, nil
}

func System(requestID string) Call {
	return Call{RequestID: requestID, ActorKind: ActorSystem, ActorName: "dop-core"}
}
