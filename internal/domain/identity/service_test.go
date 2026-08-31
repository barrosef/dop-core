package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM banco: o repositório é porta, e aqui entra um duplo
// em memória. É o retorno prático da arquitetura hexagonal.
func TestNormalizeHandle(t *testing.T) {
	casos := map[string]string{
		"dev@dop.local":     "dev",
		"Ed Barros":         "ed-barros",
		"  UPPER@x.com  ":   "upper",
		"a..b__c":           "a-b-c",
		"---trim---":        "trim",
		"maria.silva@x.com": "maria-silva",
	}
	for entrada, esperado := range casos {
		if got := identity.NormalizeHandle(entrada); got != esperado {
			t.Errorf("NormalizeHandle(%q) = %q, esperado %q", entrada, got, esperado)
		}
	}
}

func TestValidateHandle(t *testing.T) {
	if err := identity.ValidateHandle("ed"); err != nil {
		t.Errorf("handle mínimo válido recusado: %v", err)
	}
	if err := identity.ValidateHandle("a"); err == nil {
		t.Error("handle de 1 caractere deveria ser recusado")
	}
	if err := identity.ValidateHandle("Ed_Barros"); err == nil {
		t.Error("maiúscula e underscore deveriam ser recusados")
	}
}

func TestPapeis(t *testing.T) {
	if !identity.RoleOwner.CanManageMembers() || !identity.RoleAdmin.CanManageMembers() {
		t.Error("owner e admin devem poder gerir membros")
	}
	if identity.RoleDeveloper.CanManageMembers() || identity.RoleViewer.CanManageMembers() {
		t.Error("developer e viewer NÃO devem gerir membros")
	}
	// Sem manage implícito, ninguém conserta integração quebrada.
	if !identity.RoleOwner.HasImplicitManage() || !identity.RoleAdmin.HasImplicitManage() {
		t.Error("owner e admin devem ter manage implícito")
	}
	if identity.RoleDeveloper.HasImplicitManage() {
		t.Error("developer NÃO tem manage implícito")
	}
	if identity.ValidRole("superuser") {
		t.Error("papel fora do vocabulário deveria ser recusado")
	}
}

func TestConviteExpiraPorTempo(t *testing.T) {
	agora := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	inv := identity.Invite{Status: identity.InvitePending, ExpiresAt: agora.Add(time.Hour)}
	if !inv.IsUsable(agora) {
		t.Error("convite pendente e dentro do prazo deveria ser usável")
	}
	// Expiração é por TEMPO, não só por status: a varredura pode não ter passado.
	if inv.IsUsable(agora.Add(2 * time.Hour)) {
		t.Error("convite vencido deveria ser recusado mesmo com status pendente")
	}
	revogado := identity.Invite{Status: identity.InviteRevoked, ExpiresAt: agora.Add(time.Hour)}
	if revogado.IsUsable(agora) {
		t.Error("convite revogado nunca é usável")
	}
}

// ── duplo em memória ────────────────────────────────────────────────────────

// relogioFixo é o duplo do Clock. Mora aqui, e não em internal/adapter/clock,
// porque o teste de arquitetura reprova QUALQUER import de adaptador sob
// internal/domain — inclusive em arquivo _test.go. A suíte de contrato
// (test/contract/clock.go) é quem garante que este duplo e o relógio de
// verdade cumprem as mesmas garantias.
type relogioFixo struct{ t time.Time }

func (r relogioFixo) Now() time.Time { return r.t }

// instante base dos testes: fixo, para que expiração de convite (14 dias) seja
// verificável por igualdade em vez de por tolerância.
var agora = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

type fakeRepo struct {
	users    map[string]*identity.User // por subject
	byID     map[string]*identity.User
	accounts map[string]*identity.Account
	byHandle map[string]*identity.Account
	members  []identity.Membership
	nextID   int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		users: map[string]*identity.User{}, byID: map[string]*identity.User{},
		accounts: map[string]*identity.Account{}, byHandle: map[string]*identity.Account{},
	}
}

func (f *fakeRepo) id(prefix string) string {
	f.nextID++
	return prefix + "-" + string(rune('a'+f.nextID))
}

func (f *fakeRepo) UserBySubject(_ context.Context, s string) (*identity.User, error) {
	if u, ok := f.users[s]; ok {
		return u, nil
	}
	return nil, errs.NotFound("usuário")
}
func (f *fakeRepo) UserByID(_ context.Context, id string) (*identity.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, errs.NotFound("usuário")
}
func (f *fakeRepo) UpsertUser(_ context.Context, u *identity.User) (*identity.User, error) {
	if u.ID == "" {
		u.ID = f.id("usr")
	}
	cp := *u
	f.users[u.Subject] = &cp
	f.byID[u.ID] = &cp
	return &cp, nil
}
func (f *fakeRepo) AccountByID(_ context.Context, id string) (*identity.Account, error) {
	if a, ok := f.accounts[id]; ok {
		return a, nil
	}
	return nil, errs.NotFound("conta")
}
func (f *fakeRepo) AccountByHandle(_ context.Context, h string) (*identity.Account, error) {
	if a, ok := f.byHandle[h]; ok {
		return a, nil
	}
	return nil, errs.NotFound("conta")
}
func (f *fakeRepo) CreateAccountWithOwner(_ context.Context, a *identity.Account, owner string) (*identity.Account, error) {
	a.ID = f.id("acct")
	cp := *a
	f.accounts[a.ID] = &cp
	f.byHandle[a.Handle] = &cp
	f.members = append(f.members, identity.Membership{
		ID: f.id("mem"), UserID: owner, AccountID: a.ID, Role: identity.RoleOwner,
	})
	return &cp, nil
}
func (f *fakeRepo) AccountsOfUser(_ context.Context, uid string) ([]identity.Account, []identity.Membership, error) {
	var accs []identity.Account
	var mems []identity.Membership
	for _, m := range f.members {
		if m.UserID == uid {
			if a, ok := f.accounts[m.AccountID]; ok {
				accs = append(accs, *a)
				mems = append(mems, m)
			}
		}
	}
	return accs, mems, nil
}
func (f *fakeRepo) MembershipsOfAccount(_ context.Context, aid string) ([]identity.Membership, error) {
	var out []identity.Membership
	for _, m := range f.members {
		if m.AccountID == aid {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fakeRepo) MembershipOf(_ context.Context, uid, aid string) (*identity.Membership, error) {
	for i := range f.members {
		if f.members[i].UserID == uid && f.members[i].AccountID == aid {
			return &f.members[i], nil
		}
	}
	return nil, nil
}
func (f *fakeRepo) UpdateMembershipRole(_ context.Context, id string, r identity.Role) (*identity.Membership, error) {
	for i := range f.members {
		if f.members[i].ID == id {
			f.members[i].Role = r
			return &f.members[i], nil
		}
	}
	return nil, errs.NotFound("vínculo")
}
func (f *fakeRepo) CreateInvite(_ context.Context, inv *identity.Invite, _ string) (*identity.Invite, error) {
	inv.ID = f.id("inv")
	return inv, nil
}
func (f *fakeRepo) InviteByTokenHash(context.Context, string) (*identity.Invite, error) {
	return nil, nil
}
func (f *fakeRepo) AcceptInvite(context.Context, string, string) (*identity.Membership, error) {
	return nil, nil
}
func (f *fakeRepo) RevokeInvite(context.Context, string, string) (*identity.Invite, error) {
	return nil, nil
}

// ── testes do serviço ───────────────────────────────────────────────────────

func TestEnsureUserCriaContaPessoal(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, relogioFixo{agora})

	u, acct, err := svc.EnsureUser(context.Background(), ports.Principal{
		Subject: "sub-1", Email: "dev@dop.local", Name: "Dev", Providers: []string{"password"},
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if acct == nil || acct.Kind != identity.AccountPersonal {
		t.Fatal("a conta pessoal deveria nascer junto com o usuário")
	}
	if acct.Handle != "dev" {
		t.Errorf("handle deveria derivar do e-mail: %q", acct.Handle)
	}
	// O criador é owner — e a conta pessoal tem exatamente um vínculo.
	mems, _ := repo.MembershipsOfAccount(context.Background(), acct.ID)
	if len(mems) != 1 || mems[0].Role != identity.RoleOwner || mems[0].UserID != u.ID {
		t.Errorf("vínculo owner ausente ou incorreto: %+v", mems)
	}
}

func TestEnsureUserEIdempotente(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, relogioFixo{agora})
	ctx := context.Background()
	p := ports.Principal{Subject: "sub-1", Email: "dev@dop.local", Providers: []string{"password"}}

	u1, a1, _ := svc.EnsureUser(ctx, p)
	u2, a2, err := svc.EnsureUser(ctx, p)
	if err != nil {
		t.Fatalf("segunda chamada: %v", err)
	}
	if u1.ID != u2.ID {
		t.Error("EnsureUser criou usuário duplicado")
	}
	if a1.ID != a2.ID {
		t.Error("EnsureUser criou conta pessoal duplicada")
	}
}

func TestAccountLinkingAcumulaProvedores(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, relogioFixo{agora})
	ctx := context.Background()

	svc.EnsureUser(ctx, ports.Principal{Subject: "sub-1", Email: "dev@dop.local", Providers: []string{"password"}})
	u, _, err := svc.EnsureUser(ctx, ports.Principal{
		Subject: "sub-1", Email: "dev@dop.local", Providers: []string{"google.com"},
	})
	if err != nil {
		t.Fatalf("segundo provedor: %v", err)
	}
	if len(u.Providers) != 2 {
		t.Errorf("os dois provedores deveriam somar, veio %v", u.Providers)
	}
}

func TestCreateInviteExigePermissao(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, relogioFixo{agora})
	ctx := context.Background()

	u, acct, _ := svc.EnsureUser(ctx, ports.Principal{Subject: "s1", Email: "dono@x.com"})
	// developer não convida
	repo.members[0].Role = identity.RoleDeveloper
	ctx = ctxutil.Into(ctx, ctxutil.Call{AccountID: acct.ID, ActorID: u.ID, ActorKind: ctxutil.ActorUser})

	_, _, err := svc.CreateInvite(ctx, "novo@x.com", identity.RoleDeveloper, nil)
	if err == nil || errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("developer não deveria convidar; erro: %v", err)
	}

	repo.members[0].Role = identity.RoleAdmin
	inv, token, err := svc.CreateInvite(ctx, "novo@x.com", identity.RoleDeveloper,
		[]identity.GrantSpec{{ResourceID: "res-1", Level: "use"}})
	if err != nil {
		t.Fatalf("admin deveria convidar: %v", err)
	}
	if token == "" {
		t.Error("o token do convite deveria ser devolvido uma única vez")
	}
	if len(inv.Grants) != 1 {
		t.Error("as concessões compostas no convite deveriam ser preservadas")
	}
}

func TestCreateInviteRecusaNivelInvalido(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, relogioFixo{agora})
	ctx := context.Background()
	u, acct, _ := svc.EnsureUser(ctx, ports.Principal{Subject: "s1", Email: "dono@x.com"})
	ctx = ctxutil.Into(ctx, ctxutil.Call{AccountID: acct.ID, ActorID: u.ID})

	if _, _, err := svc.CreateInvite(ctx, "n@x.com", identity.RoleDeveloper,
		[]identity.GrantSpec{{ResourceID: "r", Level: "admin"}}); err == nil {
		t.Error("nível de concessão fora de use|manage deveria ser recusado")
	}
}

func TestOperacaoSemContaAtivaERecusada(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), relogioFixo{agora})
	// Sem AccountID: regra do SP-0 — requisição sem conta ativa é inválida.
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "u1"})
	if _, _, err := svc.CreateInvite(ctx, "a@b.com", identity.RoleViewer, nil); err == nil {
		t.Error("operação sem conta ativa deveria ser recusada")
	}
}

// A expiração do convite era, até aqui, verificável só por tolerância — o
// serviço lia o relógio de parede por dentro. Com a porta injetada dá para
// afirmar o instante exato, e para atravessar a fronteira dos 14 dias sem
// dormir.
func TestConviteExpiraExatamenteEmQuatorzeDias(t *testing.T) {
	repo := newFakeRepo()
	svc := identity.NewService(repo, relogioFixo{agora})
	ctx := context.Background()

	u, acct, _ := svc.EnsureUser(ctx, ports.Principal{Subject: "s1", Email: "dono@x.com"})
	ctx = ctxutil.Into(ctx, ctxutil.Call{AccountID: acct.ID, ActorID: u.ID, ActorKind: ctxutil.ActorUser})

	convite, _, err := svc.CreateInvite(ctx, "novo@dop.dev", identity.RoleDeveloper, nil)
	if err != nil {
		t.Fatalf("criar convite: %v", err)
	}

	if quer := agora.Add(identity.InviteTTL); !convite.ExpiresAt.Equal(quer) {
		t.Fatalf("expiração em %v, esperada %v", convite.ExpiresAt, quer)
	}

	// Um instante ANTES do vencimento ainda serve; no vencimento, não. A
	// fronteira é fechada em cima: `now.Before(ExpiresAt)`.
	if !convite.IsUsable(convite.ExpiresAt.Add(-time.Nanosecond)) {
		t.Error("convite deveria valer no último instante antes de expirar")
	}
	if convite.IsUsable(convite.ExpiresAt) {
		t.Error("convite não pode valer no exato instante da expiração")
	}
}
