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
	// It comes from the edge, verified: since ADR-0029 what reaches this struct
	// is what a signature proved, not what a header claimed.
	SessionID string
	// Caller is the COMPONENT that signed the call — "bff", "collector"
	// (ADR-0029). It is empty on a call proven only by the person's token,
	// because a token says who the person is and nothing about who relayed it.
	//
	// It exists for the authorizations that are about the component and not
	// about the person: only the collector writes metrics, and no person ever
	// does.
	Caller string
	// Verified is non-nil only when a bearer token was verified on this call. It
	// is INDEPENDENT of ActorID: a token proves the person, and the person may
	// not have a user yet. Anything that needs an actor keeps reading ActorID.
	Verified *VerifiedIdentity
}

// VerifiedIdentity is what the person's TOKEN proved, as opposed to what a
// request body claimed. It exists for exactly one caller: the bootstrap that
// creates a user, which runs before any user exists and therefore cannot be
// authorized by an actor.
//
// It restates ports.Principal instead of importing it. This package is platform
// and today depends on nothing in the domain; inverting that to reuse one struct
// buys nothing. The codebase already makes the same trade where reaction
// restates a workflow stage's action rather than importing the package.
type VerifiedIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	AvatarURL     string
	Providers     []string
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
