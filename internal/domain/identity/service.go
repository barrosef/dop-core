package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service concentra as regras de identidade. Recebe apenas PORTAS.
type Service struct {
	repo  Repository
	clock ports.Clock
}

// NewService exige um relógio. Aceitar nil era o que mantinha a porta de
// enfeite: o serviço caía em time.Now() por dentro, nenhum teste de expiração
// era determinístico, e ninguém percebia que a abstração não estava provada.
// Panic aqui é deliberado — é erro de montagem, detectado no boot, não em
// produção às três da manhã.
func NewService(repo Repository, clock ports.Clock) *Service {
	if clock == nil {
		panic("identity.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	return &Service{repo: repo, clock: clock}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// EnsureUser é chamada em TODO primeiro login e precisa ser idempotente.
//
// Aqui mora a decisão do account linking: o mesmo e-mail chegando por outro
// provedor resolve para o MESMO usuário. Sem isso, quem entrou por Google e
// depois por GitHub vira dois usuários — e duplicata em sistema multi-tenant
// não é incômodo cosmético, é confusão de acesso (spec SP-0 §1).
//
// Junto com o usuário nasce a conta pessoal: o usuário não precisa saber que
// isso aconteceu, ele só vê "minha conta" no seletor.
func (s *Service) EnsureUser(ctx context.Context, p ports.Principal) (*User, *Account, error) {
	if p.Subject == "" {
		return nil, nil, errs.Invalid("principal sem sujeito")
	}

	existing, err := s.repo.UserBySubject(ctx, p.Subject)
	if err != nil && errs.KindOf(err) != errs.KindNotFound {
		return nil, nil, err
	}

	u := &User{
		Subject:       p.Subject,
		Email:         strings.ToLower(strings.TrimSpace(p.Email)),
		EmailVerified: p.EmailVerified,
		Name:          p.Name,
		AvatarURL:     p.AvatarURL,
		Providers:     p.Providers,
	}
	if existing != nil {
		u.ID = existing.ID
		// Preserva o que o provedor novo não trouxe.
		if u.Name == "" {
			u.Name = existing.Name
		}
		if u.AvatarURL == "" {
			u.AvatarURL = existing.AvatarURL
		}
		u.Providers = mergeProviders(existing.Providers, p.Providers)
	}

	saved, err := s.repo.UpsertUser(ctx, u)
	if err != nil {
		return nil, nil, err
	}

	// Conta pessoal existente?
	accounts, _, err := s.repo.AccountsOfUser(ctx, saved.ID)
	if err != nil {
		return nil, nil, err
	}
	for i := range accounts {
		if accounts[i].Kind == AccountPersonal {
			return saved, &accounts[i], nil
		}
	}

	personal, err := s.createPersonalAccount(ctx, saved)
	if err != nil {
		return nil, nil, err
	}
	return saved, personal, nil
}

// createPersonalAccount deriva o handle do e-mail e resolve colisão por sufixo
// — o usuário pode trocar depois.
func (s *Service) createPersonalAccount(ctx context.Context, u *User) (*Account, error) {
	base := NormalizeHandle(u.Email)
	if base == "" {
		base = NormalizeHandle(u.Name)
	}
	if len(base) < handleMinLen {
		base = "dev-" + u.ID[:8]
	}

	handle := base
	for attempt := 0; attempt < 50; attempt++ {
		if attempt > 0 {
			handle = base + "-" + randomSuffix(4)
		}
		found, err := s.repo.AccountByHandle(ctx, handle)
		if err != nil && errs.KindOf(err) != errs.KindNotFound {
			return nil, err
		}
		if found != nil {
			continue
		}
		name := u.Name
		if name == "" {
			name = u.Email
		}
		return s.repo.CreateAccountWithOwner(ctx, &Account{
			Kind:        AccountPersonal,
			Handle:      handle,
			DisplayName: name,
		}, u.ID)
	}
	return nil, errs.Internal("não foi possível derivar um handle livre")
}

// CreateOrganization cria a conta PJ; quem criou vira owner.
// Sem espera e sem documento: a legitimidade vem da verificação de domínio,
// feita depois (ADR-0004).
func (s *Service) CreateOrganization(ctx context.Context, handle, displayName, legalID string) (*Account, error) {
	call, ok := ctxutil.From(ctx)
	if !ok || call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "ator não identificado")
	}
	handle = NormalizeHandle(handle)
	if err := ValidateHandle(handle); err != nil {
		return nil, err
	}
	if strings.TrimSpace(legalID) == "" {
		return nil, errs.Invalid("CNPJ é obrigatório para conta de organização")
	}
	if found, err := s.repo.AccountByHandle(ctx, handle); err != nil {
		if errs.KindOf(err) != errs.KindNotFound {
			return nil, err
		}
	} else if found != nil {
		return nil, errs.New(errs.KindAlreadyExists, "o identificador %q já está em uso", handle)
	}
	return s.repo.CreateAccountWithOwner(ctx, &Account{
		Kind:        AccountOrganization,
		Handle:      handle,
		DisplayName: displayName,
		LegalID:     legalID,
	}, call.ActorID)
}

// ListAccounts alimenta o seletor de conta ativa: a pessoal mais toda
// organização em que o usuário tenha vínculo.
func (s *Service) ListAccounts(ctx context.Context, userID string) ([]Account, []Membership, error) {
	if userID == "" {
		return nil, nil, errs.Invalid("usuário não informado")
	}
	return s.repo.AccountsOfUser(ctx, userID)
}

// Authorize resolve papel e concessões do ator na conta ativa.
// É o que o BFF consulta para preencher o AuthContext dos decorators.
func (s *Service) Authorize(ctx context.Context, userID, accountID string) (*Membership, error) {
	if accountID == "" {
		return nil, ctxutil.ErrNoAccount
	}
	m, err := s.repo.MembershipOf(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errs.Permission("sem vínculo com esta conta")
	}
	return m, nil
}

// CreateInvite compõe papel e concessões NO CONVITE — sem defaults.
func (s *Service) CreateInvite(ctx context.Context, email string, role Role, grants []GrantSpec) (*Invite, error) {
	call, _ := ctxutil.From(ctx)
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidRole(role) {
		return nil, errs.Invalid("papel desconhecido: %q", role)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, errs.Invalid("e-mail inválido")
	}
	for _, g := range grants {
		if g.Level != "use" && g.Level != "manage" {
			return nil, errs.Invalid("nível de concessão inválido: %q", g.Level)
		}
	}

	// Quem convida precisa poder gerir membros.
	actor, err := s.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !actor.Role.CanManageMembers() {
		return nil, errs.Permission("apenas owner ou admin podem convidar")
	}

	inv := &Invite{
		AccountID: accountID,
		Email:     email,
		Role:      role,
		Grants:    grants,
		Status:    InvitePending,
		InvitedBy: call.ActorID,
		ExpiresAt: s.now().Add(InviteTTL),
	}
	saved, err := s.repo.CreateInvite(ctx, inv)
	if err != nil {
		return nil, err
	}
	// NÃO existe mais token. O aceite confere o e-mail VERIFICADO da sessão
	// contra o do convite, então o link precisa apenas ENDEREÇAR o convite —
	// e um id que não concede nada pode viajar no e-mail, no evento e na
	// timeline sem virar credencial em repouso.
	return saved, nil
}

// AcceptInvite valida a expiração por TEMPO, não só por status: a varredura de
// expirados pode não ter passado ainda.
func (s *Service) AcceptInvite(ctx context.Context, inviteID, userID string) (*Membership, error) {
	if userID == "" {
		return nil, errs.New(errs.KindUnauthorized, "aceite exige sessão autenticada")
	}
	inv, err := s.repo.InviteByID(ctx, inviteID)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		return nil, errs.NotFound("convite")
	}
	if !inv.IsUsable(s.now()) {
		return nil, errs.Precondition("convite %s", inv.Status)
	}

	// O convite é para UMA pessoa, e agora ele exige que ela seja ela.
	//
	// Antes o aceite conferia só o token: qualquer usuário autenticado que
	// tivesse o link entrava na conta, com o papel concedido a outra pessoa.
	// Era credencial de PORTADOR, e por isso não podia aparecer em evento nem
	// em projeção — o que impedia o e-mail de carregar link de aceite.
	//
	// Exigindo o e-mail VERIFICADO da sessão, o link deixa de conceder
	// qualquer coisa a quem apenas o possui: é preciso SER o convidado. Foi
	// isso que liberou o `invite_id` para viajar em texto claro.
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, errs.New(errs.KindUnauthorized, "sessão sem usuário")
	}
	// Não verificado é recusa SEPARADA da divergência: "confirme seu e-mail" e
	// "este convite não é seu" mandam a pessoa fazer coisas diferentes, e um
	// erro só faria as duas parecerem a mesma parede.
	if !u.EmailVerified {
		return nil, errs.Precondition(
			"o aceite exige e-mail verificado: confirme %s antes de entrar na conta", u.Email)
	}
	if !strings.EqualFold(strings.TrimSpace(u.Email), strings.TrimSpace(inv.Email)) {
		// A mensagem NÃO diz para quem era o convite: isso transformaria o link
		// num oráculo de e-mail para quem o encontrasse.
		return nil, errs.New(errs.KindPermission, "este convite foi feito para outro e-mail")
	}

	return s.repo.AcceptInvite(ctx, inv.ID, userID)
}

func (s *Service) RevokeInvite(ctx context.Context, inviteID string) (*Invite, error) {
	call, _ := ctxutil.From(ctx)
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	actor, err := s.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !actor.Role.CanManageMembers() {
		return nil, errs.Permission("apenas owner ou admin podem revogar convites")
	}
	return s.repo.RevokeInvite(ctx, accountID, inviteID)
}

// UpdateMembershipRole altera o papel de um membro.
//
// A invariante "toda conta tem ao menos um owner ativo" é garantida por TRIGGER
// no banco — regra que nenhuma operação pode violar, nem por caminho que
// ninguém previu.
func (s *Service) UpdateMembershipRole(ctx context.Context, membershipID string, role Role) (*Membership, error) {
	call, _ := ctxutil.From(ctx)
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidRole(role) {
		return nil, errs.Invalid("papel desconhecido: %q", role)
	}
	actor, err := s.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !actor.Role.CanManageMembers() {
		return nil, errs.Permission("apenas owner ou admin podem alterar vínculos")
	}
	return s.repo.UpdateMembershipRole(ctx, membershipID, role)
}

func (s *Service) ListMemberships(ctx context.Context) ([]Membership, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.MembershipsOfAccount(ctx, accountID)
}

func (s *Service) GetUser(ctx context.Context, id string) (*User, error) {
	return s.repo.UserByID(ctx, id)
}

func (s *Service) GetAccount(ctx context.Context, id string) (*Account, error) {
	return s.repo.AccountByID(ctx, id)
}

// ── auxiliares ───────────────────────────────────────────────────────────────

func mergeProviders(existing, incoming []string) []string {
	seen := make(map[string]bool, len(existing)+len(incoming))
	out := make([]string, 0, len(existing)+len(incoming))
	for _, list := range [][]string{existing, incoming} {
		for _, p := range list {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func randomSuffix(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
