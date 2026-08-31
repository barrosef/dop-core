package resource

import (
	"context"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service concentra as regras de recurso. Recebe apenas PORTAS.
//
// Repare no que NÃO existe aqui: nenhum método devolve o valor de uma
// credencial. O segredo entra por SetCredential e some — quem precisa dele é o
// executor, que resolve a SecretRef pelo próprio SecretStore. Não há caminho
// de leitura pelo qual um segredo volte por uma RPC (ADR-0001).
type Service struct {
	repo    Repository
	access  Access
	secrets ports.SecretStore
}

func NewService(repo Repository, access Access, secrets ports.SecretStore) *Service {
	return &Service{repo: repo, access: access, secrets: secrets}
}

// actor é o ator resolvido na conta ativa: papel e natureza da conta, que é
// exatamente o que EffectiveLevel consome.
type actor struct {
	accountID   string
	userID      string
	role        identity.Role
	accountKind identity.AccountKind
}

// who resolve quem está chamando, na conta ativa.
//
// Requisição sem conta ativa é inválida por definição (SP-0) — e a resolução
// do papel acontece UMA vez por operação, não uma vez por recurso.
func (s *Service) who(ctx context.Context) (actor, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return actor{}, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return actor{}, errs.New(errs.KindUnauthorized, "ator não identificado")
	}
	m, err := s.access.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return actor{}, err
	}
	acct, err := s.access.GetAccount(ctx, accountID)
	if err != nil {
		return actor{}, err
	}
	return actor{
		accountID:   accountID,
		userID:      call.ActorID,
		role:        m.Role,
		accountKind: acct.Kind,
	}, nil
}

// authorize carrega o recurso e verifica o nível EXIGIDO, numa operação só.
// Separar "carregar" de "autorizar" convidaria a esquecer a segunda parte.
func (s *Service) authorize(ctx context.Context, a actor, resourceID string, need Level) (*Resource, error) {
	if strings.TrimSpace(resourceID) == "" {
		return nil, errs.Invalid("recurso não informado")
	}
	r, err := s.repo.ByID(ctx, a.accountID, resourceID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errs.NotFound("recurso")
	}
	g, err := s.repo.GrantOf(ctx, a.accountID, r.ID, a.userID)
	if err != nil {
		return nil, err
	}
	if lvl := EffectiveLevel(*r, a.role, a.accountKind, g); !lvl.AtLeast(need) {
		return nil, errs.Permission("sem concessão de %s sobre o recurso %q", need, r.Name)
	}
	return r, nil
}

// List devolve o que o ator PODE ver, não tudo que existe na conta.
//
// Filtrar na leitura é o que faz o default por natureza ser real: uma
// integração fechada não aparece na lista de quem não recebeu concessão. As
// concessões do ator são buscadas de uma vez — não há uma consulta por linha.
func (s *Service) List(ctx context.Context, kind Kind) ([]Resource, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	if kind != "" && !ValidKind(kind) {
		return nil, errs.Invalid("tipo de recurso desconhecido: %q", kind)
	}

	all, err := s.repo.List(ctx, a.accountID, kind)
	if err != nil {
		return nil, err
	}
	grants, err := s.repo.GrantsOfUser(ctx, a.accountID, a.userID)
	if err != nil {
		return nil, err
	}
	byResource := make(map[string]*Grant, len(grants))
	for i := range grants {
		byResource[grants[i].ResourceID] = &grants[i]
	}

	out := make([]Resource, 0, len(all))
	for i := range all {
		if EffectiveLevel(all[i], a.role, a.accountKind, byResource[all[i].ID]).AtLeast(LevelUse) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Resource, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	return s.authorize(ctx, a, id, LevelUse)
}

// Create registra um recurso na conta ativa.
//
// Duas decisões moram aqui:
//
//   - integração tem seu config VALIDADO na escrita (categoria e provedor):
//     descobrir que a integração não sabe quem é na hora do deploy é tarde;
//   - integração nasce FECHADA, então quem a criou recebe manage explícito.
//     Sem isso, um developer conecta o GitHub e perde o acesso à própria
//     integração no instante seguinte — o default por natureza viraria uma
//     armadilha em vez de uma proteção.
func (s *Service) Create(ctx context.Context, kind Kind, name string, config map[string]any) (*Resource, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidKind(kind) {
		return nil, errs.Invalid("tipo de recurso desconhecido: %q", kind)
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	// Viewer é papel de leitura: não cria recurso na conta.
	if a.role == identity.RoleViewer {
		return nil, errs.Permission("viewer não cria recurso")
	}
	if config == nil {
		config = map[string]any{}
	}
	if kind.HasCredential() {
		if _, err := ParseIntegration(config); err != nil {
			return nil, err
		}
	}

	saved, err := s.repo.Create(ctx, &Resource{
		AccountID: a.accountID,
		Kind:      kind,
		Name:      strings.TrimSpace(name),
		Version:   1,
		Config:    config,
		Status:    "active",
		CreatedBy: a.userID,
	})
	if err != nil {
		return nil, err
	}

	if saved.HasCredential() && !a.role.HasImplicitManage() {
		if _, err := s.repo.Grant(ctx, a.accountID, &Grant{
			ResourceID: saved.ID,
			UserID:     a.userID,
			Level:      LevelManage,
			GrantedBy:  a.userID,
		}); err != nil {
			return nil, err
		}
	}
	return saved, nil
}

// Update altera a configuração. Conteúdo é VERSIONADO — a versão nova não
// apaga a anterior, porque alguém vai precisar saber com qual skill aquela
// execução rodou. Integração é sobrescrita: base_url de ontem não é história,
// é lixo.
func (s *Service) Update(ctx context.Context, id string, config map[string]any) (*Resource, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	r, err := s.authorize(ctx, a, id, LevelManage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		config = map[string]any{}
	}
	if r.HasCredential() {
		if _, err := ParseIntegration(config); err != nil {
			return nil, err
		}
	}
	return s.repo.Update(ctx, a.accountID, r.ID, config, r.IsVersioned())
}

// Delete apaga o recurso e, antes dele, a credencial.
//
// A ordem é deliberada: o cofre primeiro, a linha depois. Delete no SecretStore
// é idempotente por contrato, então uma falha na remoção da linha deixa um
// retry limpo. A ordem inversa deixaria segredo órfão no cofre — sem nenhuma
// linha apontando para ele, ninguém jamais o encontraria para apagar.
func (s *Service) Delete(ctx context.Context, id string) error {
	a, err := s.who(ctx)
	if err != nil {
		return err
	}
	r, err := s.authorize(ctx, a, id, LevelManage)
	if err != nil {
		return err
	}
	if r.CredentialRef != "" {
		if err := s.secrets.Delete(ctx, SecretRefFor(a.accountID, r.ID)); err != nil {
			return errs.Wrap(errs.KindUnavailable, err,
				"falha ao remover a credencial do recurso %s", r.ID)
		}
	}
	return s.repo.Delete(ctx, a.accountID, r.ID)
}

// Grant concede acesso a um membro da conta.
//
// Quem concede precisa ter manage sobre o RECURSO — não basta ser membro, e
// não basta ter use. E só se concede a quem já é membro da conta: acesso a
// recurso não é porta de entrada na conta, é composição sobre um vínculo que
// já existe (ADR-0013).
//
// Consequência que vale registrar: numa conta pessoal não existe segundo
// membro, então nenhuma concessão é possível — recurso de conta pessoal nunca
// é compartilhável, e isso sai da cardinalidade, sem regra especial.
func (s *Service) Grant(ctx context.Context, resourceID, userID string, level Level) (*Grant, error) {
	a, err := s.who(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidLevel(level) {
		return nil, errs.Invalid("nível de concessão inválido: %q (use use ou manage)", level)
	}
	if strings.TrimSpace(userID) == "" {
		return nil, errs.Invalid("usuário da concessão não informado")
	}
	r, err := s.authorize(ctx, a, resourceID, LevelManage)
	if err != nil {
		return nil, err
	}
	if _, err := s.access.Authorize(ctx, userID, a.accountID); err != nil {
		switch errs.KindOf(err) {
		case errs.KindPermission, errs.KindNotFound:
			return nil, errs.Invalid("não é possível conceder acesso a quem não é membro desta conta")
		}
		return nil, err
	}
	return s.repo.Grant(ctx, a.accountID, &Grant{
		ResourceID: r.ID,
		UserID:     userID,
		Level:      level,
		GrantedBy:  a.userID,
	})
}

// RevokeGrant remove uma concessão. A autorização é sobre o RECURSO da
// concessão — quem gerencia o recurso decide quem o acessa.
//
// Revogar não fecha a porta para owner e admin: o manage deles é implícito e
// não passa por esta tabela. É proposital.
func (s *Service) RevokeGrant(ctx context.Context, grantID string) error {
	a, err := s.who(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(grantID) == "" {
		return errs.Invalid("concessão não informada")
	}
	g, err := s.repo.GrantByID(ctx, a.accountID, grantID)
	if err != nil {
		return err
	}
	if g == nil {
		return errs.NotFound("concessão")
	}
	if _, err := s.authorize(ctx, a, g.ResourceID, LevelManage); err != nil {
		return err
	}
	return s.repo.RevokeGrant(ctx, a.accountID, grantID)
}

// SetCredential guarda o segredo no SecretStore e persiste APENAS a referência
// opaca na linha do recurso.
//
// O valor não é gravado no banco, não volta em nenhuma leitura, não entra em
// evento e não entra em mensagem de erro — nem como "cause". A ordem é cofre
// primeiro, linha depois: se o banco falhar, sobra um segredo sem ponteiro
// (inerte, e sobrescrito na próxima tentativa); a ordem inversa deixaria a
// linha afirmando ter credencial que não existe, e a execução falharia longe
// daqui, sem explicação.
func (s *Service) SetCredential(ctx context.Context, resourceID string, secret []byte) (string, error) {
	a, err := s.who(ctx)
	if err != nil {
		return "", err
	}
	if len(secret) == 0 {
		return "", errs.Invalid("credencial vazia")
	}
	r, err := s.authorize(ctx, a, resourceID, LevelManage)
	if err != nil {
		return "", err
	}
	if !r.HasCredential() {
		return "", errs.Precondition("recurso do tipo %q não tem credencial", r.Kind)
	}

	if err := s.secrets.Put(ctx, SecretRefFor(a.accountID, r.ID), ports.SecretValue(secret)); err != nil {
		return "", errs.Wrap(errs.KindUnavailable, err,
			"falha ao guardar a credencial do recurso %s", r.ID)
	}
	saved, err := s.repo.SetCredentialRef(ctx, a.accountID, r.ID, CredentialRef(a.accountID, r.ID))
	if err != nil {
		return "", err
	}
	return saved.CredentialRef, nil
}
