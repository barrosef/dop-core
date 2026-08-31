// Package ctxutil carrega o contexto de chamada — quem, em qual conta — por
// todas as camadas, sem que o domínio precise recebê-lo como parâmetro.
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

// Call é o contexto obrigatório de toda operação de domínio.
type Call struct {
	RequestID string
	AccountID string
	ActorID   string
	ActorKind ActorKind
	ActorName string
}

// ErrNoAccount sinaliza requisição sem conta ativa — inválida por definição
// (regra do SP-0, materializada aqui e no @account_scoped do BFF).
var ErrNoAccount = errors.New("requisição sem conta ativa")

// Requisição sem conta ativa é erro do CLIENTE, não falha interna: precisa
// chegar como 400, não como 500.
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

// MustAccount devolve a conta ativa ou ErrNoAccount. Todo repositório filtra
// por ela — isolamento multi-tenant é constraint, não convenção.
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
