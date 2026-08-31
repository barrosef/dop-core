package resource

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
)

// Repository é a PORTA de persistência do domínio de recurso.
//
// Declarada aqui, em linguagem de domínio; implementada em
// internal/adapter/postgres. O domínio nunca vê SQL.
//
// Toda operação recebe accountID explicitamente: isolamento multi-tenant é
// parâmetro obrigatório da porta, não algo que o adaptador possa esquecer.
type Repository interface {
	// Recursos
	List(ctx context.Context, accountID string, kind Kind) ([]Resource, error)
	ByID(ctx context.Context, accountID, id string) (*Resource, error)
	Create(ctx context.Context, r *Resource) (*Resource, error)
	// Update grava a configuração nova. bumpVersion=true incrementa a versão
	// em vez de sobrescrever — é o que separa conteúdo de credencial.
	Update(ctx context.Context, accountID, id string, config map[string]any, bumpVersion bool) (*Resource, error)
	Delete(ctx context.Context, accountID, id string) error
	// SetCredentialRef grava APENAS o ponteiro opaco. O valor do segredo não
	// passa por esta porta em momento algum.
	SetCredentialRef(ctx context.Context, accountID, id, ref string) (*Resource, error)

	// Concessões
	GrantsOfUser(ctx context.Context, accountID, userID string) ([]Grant, error)
	GrantOf(ctx context.Context, accountID, resourceID, userID string) (*Grant, error)
	GrantByID(ctx context.Context, accountID, grantID string) (*Grant, error)
	// Grant é upsert: conceder de novo com outro nível AJUSTA a concessão, não
	// duplica linha (a tabela tem UNIQUE (resource_id, user_id)).
	Grant(ctx context.Context, accountID string, g *Grant) (*Grant, error)
	RevokeGrant(ctx context.Context, accountID, grantID string) error
}

// Access é a porta ESTREITA para o domínio de identidade: recurso precisa
// saber duas coisas sobre quem chama — o papel na conta ativa (para o manage
// implícito de owner e admin) e a natureza da conta (para o default de acesso
// por natureza). Nada além disso.
//
// A superfície foi escolhida para que *identity.Service a satisfaça como está:
// o composition root apenas liga, sem adaptador de cola e sem duplicar a regra
// de papéis em dois lugares.
type Access interface {
	Authorize(ctx context.Context, userID, accountID string) (*identity.Membership, error)
	GetAccount(ctx context.Context, id string) (*identity.Account, error)
}
