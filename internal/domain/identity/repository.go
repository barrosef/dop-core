package identity

import "context"

// Repository é a PORTA de persistência do domínio de identidade.
//
// Declarada aqui, em linguagem de domínio; implementada em
// internal/adapter/postgres. O domínio nunca vê SQL.
type Repository interface {
	// Usuários
	UserBySubject(ctx context.Context, subject string) (*User, error)
	UserByID(ctx context.Context, id string) (*User, error)
	UpsertUser(ctx context.Context, u *User) (*User, error)

	// Contas e vínculos
	AccountByID(ctx context.Context, id string) (*Account, error)
	AccountByHandle(ctx context.Context, handle string) (*Account, error)
	CreateAccountWithOwner(ctx context.Context, a *Account, ownerUserID string) (*Account, error)
	AccountsOfUser(ctx context.Context, userID string) ([]Account, []Membership, error)

	MembershipsOfAccount(ctx context.Context, accountID string) ([]Membership, error)
	MembershipOf(ctx context.Context, userID, accountID string) (*Membership, error)
	UpdateMembershipRole(ctx context.Context, membershipID string, role Role) (*Membership, error)

	// Convites
	CreateInvite(ctx context.Context, inv *Invite) (*Invite, error)
	// InviteByID busca pelo id da linha — que NÃO concede nada por si só.
	//
	// Substituiu a busca por hash de token. O token era credencial de
	// PORTADOR: quem tivesse o valor entrava, e por isso ele não podia
	// aparecer em evento, log nem projeção. Com o aceite exigindo que o
	// e-mail VERIFICADO da sessão bata com o do convite, o id vira suficiente
	// como endereço e insuficiente como permissão — que é a propriedade que
	// permite ele viajar no link, no evento e na timeline sem risco.
	InviteByID(ctx context.Context, id string) (*Invite, error)
	AcceptInvite(ctx context.Context, inviteID, userID string) (*Membership, error)
	RevokeInvite(ctx context.Context, accountID, inviteID string) (*Invite, error)
}
