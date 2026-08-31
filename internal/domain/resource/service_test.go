package resource_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/resource"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM banco e SEM cofre real: repositório e SecretStore
// são portas, e aqui entram duplos em memória. É o retorno prático da
// arquitetura hexagonal.
//
// O cofre usado nos testes é um duplo local, não o adaptador
// internal/adapter/secretstore: o teste de arquitetura varre TODO .go sob
// internal/domain, inclusive os _test.go, e importar adaptador daqui quebraria
// a fronteira que ele protege. O duplo satisfaz ports.SecretStore — a mesma
// porta, o mesmo contrato.

// ── natureza do recurso ──────────────────────────────────────────────────────

func TestNaturezaDoRecurso(t *testing.T) {
	if !resource.KindIntegration.HasCredential() {
		t.Error("integração é o único tipo com credencial")
	}
	for _, k := range []resource.Kind{resource.KindSkill, resource.KindWorkflow, resource.KindGitFlow} {
		if k.HasCredential() {
			t.Errorf("%s não deveria ter credencial", k)
		}
		if !k.IsContent() {
			t.Errorf("%s é recurso de conteúdo", k)
		}
	}
	if resource.ValidKind("database") {
		t.Error("tipo fora do vocabulário deveria ser recusado")
	}
	// Conteúdo é versionado; credencial não.
	if !(resource.Resource{Kind: resource.KindSkill}).IsVersioned() {
		t.Error("skill deveria ser versionada")
	}
	if (resource.Resource{Kind: resource.KindIntegration}).IsVersioned() {
		t.Error("integração não é versionada — config é estado corrente, não história")
	}
}

func TestNivelManageIncluiUse(t *testing.T) {
	if !resource.LevelManage.AtLeast(resource.LevelUse) {
		t.Error("quem gerencia também usa")
	}
	if resource.LevelUse.AtLeast(resource.LevelManage) {
		t.Error("use NÃO dá manage")
	}
	if resource.LevelNone.AtLeast(resource.LevelUse) {
		t.Error("ausência de concessão não dá uso")
	}
	if resource.ValidLevel("admin") {
		t.Error("nível fora de use|manage deveria ser recusado")
	}
}

// TestDefaultDeAcessoPorNatureza é o teste central deste domínio: credencial é
// risco, conhecimento é conhecimento (ADR-0014 §6).
func TestDefaultDeAcessoPorNatureza(t *testing.T) {
	integracao := resource.Resource{ID: "r1", Kind: resource.KindIntegration}
	skill := resource.Resource{ID: "r2", Kind: resource.KindSkill}
	org := identity.AccountOrganization

	// Recurso COM credencial nasce FECHADO.
	if lvl := resource.EffectiveLevel(integracao, identity.RoleDeveloper, org, nil); lvl != resource.LevelNone {
		t.Errorf("integração sem concessão deveria ser inacessível, veio %q", lvl)
	}
	// Recurso de CONTEÚDO nasce ABERTO dentro da conta de organização.
	if lvl := resource.EffectiveLevel(skill, identity.RoleDeveloper, org, nil); lvl != resource.LevelUse {
		t.Errorf("skill deveria ser usável por membro da organização, veio %q", lvl)
	}
	// ...mas aberto é USE, não MANAGE: alterar conhecimento do time exige
	// concessão explícita.
	if resource.EffectiveLevel(skill, identity.RoleDeveloper, org, nil).AtLeast(resource.LevelManage) {
		t.Error("o padrão aberto não pode conceder manage")
	}
	// Owner e admin gerenciam TODO recurso — sem isso ninguém conserta uma
	// integração quebrada.
	for _, papel := range []identity.Role{identity.RoleOwner, identity.RoleAdmin} {
		if lvl := resource.EffectiveLevel(integracao, papel, org, nil); lvl != resource.LevelManage {
			t.Errorf("%s deveria ter manage implícito, veio %q", papel, lvl)
		}
	}
	// Concessão explícita destrava a integração fechada.
	g := &resource.Grant{ResourceID: "r1", UserID: "u2", Level: resource.LevelUse}
	if lvl := resource.EffectiveLevel(integracao, identity.RoleDeveloper, org, g); lvl != resource.LevelUse {
		t.Errorf("concessão explícita deveria valer, veio %q", lvl)
	}
	// E restringe o conteúdo aberto: viewer com manage explícito gerencia.
	gm := &resource.Grant{ResourceID: "r2", UserID: "u3", Level: resource.LevelManage}
	if lvl := resource.EffectiveLevel(skill, identity.RoleViewer, org, gm); lvl != resource.LevelManage {
		t.Errorf("concessão explícita de manage deveria valer para viewer, veio %q", lvl)
	}
}

func TestIntegracaoPrecisaDeCategoriaEProvedor(t *testing.T) {
	if _, err := resource.ParseIntegration(map[string]any{"provider": "github"}); err == nil {
		t.Error("integração sem categoria deveria ser recusada")
	}
	if _, err := resource.ParseIntegration(map[string]any{"category": "banco", "provider": "x"}); err == nil {
		t.Error("categoria fora de git|task_manager|agent deveria ser recusada")
	}
	if _, err := resource.ParseIntegration(map[string]any{"category": "git"}); err == nil {
		t.Error("integração sem provedor deveria ser recusada")
	}
	for _, c := range []string{"git", "task_manager", "agent"} {
		if _, err := resource.ParseIntegration(map[string]any{"category": c, "provider": "p"}); err != nil {
			t.Errorf("categoria %q deveria ser aceita: %v", c, err)
		}
	}
}

// ── duplos em memória ────────────────────────────────────────────────────────

// cofreFake é o SecretStore dos testes: as quatro operações da porta e nada
// além. Espelha o adaptador em memória de internal/adapter/secretstore, com as
// mesmas garantias que importam aqui — leitura-após-escrita, Get de referência
// inexistente devolvendo (nil, nil), Delete idempotente, Put substituindo, e
// isolamento por conta (a chave inclui AccountID).
type cofreFake struct {
	mu   sync.RWMutex
	data map[string]ports.SecretValue
}

func novoCofre() *cofreFake { return &cofreFake{data: map[string]ports.SecretValue{}} }

func chave(r ports.SecretRef) string { return r.AccountID + "/" + r.Kind + "/" + r.OwnerID }

func (c *cofreFake) Put(_ context.Context, ref ports.SecretRef, v ports.SecretValue) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make(ports.SecretValue, len(v))
	copy(cp, v)
	c.data[chave(ref)] = cp
	return nil
}

func (c *cofreFake) Get(_ context.Context, ref ports.SecretRef) (ports.SecretValue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[chave(ref)]
	if !ok {
		return nil, nil
	}
	cp := make(ports.SecretValue, len(v))
	copy(cp, v)
	return cp, nil
}

func (c *cofreFake) Delete(_ context.Context, ref ports.SecretRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, chave(ref))
	return nil
}

func (c *cofreFake) Exists(ctx context.Context, ref ports.SecretRef) (bool, error) {
	v, err := c.Get(ctx, ref)
	return v != nil, err
}

var _ ports.SecretStore = (*cofreFake)(nil)

type fakeRepo struct {
	res    map[string]*resource.Resource
	grants map[string]*resource.Grant
	seq    int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{res: map[string]*resource.Resource{}, grants: map[string]*resource.Grant{}}
}

func (f *fakeRepo) id(p string) string {
	f.seq++
	return p + "-" + string(rune('a'+f.seq))
}

func (f *fakeRepo) List(_ context.Context, accountID string, kind resource.Kind) ([]resource.Resource, error) {
	var out []resource.Resource
	for _, r := range f.res {
		if r.AccountID == accountID && (kind == "" || r.Kind == kind) {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeRepo) ByID(_ context.Context, accountID, id string) (*resource.Resource, error) {
	if r, ok := f.res[id]; ok && r.AccountID == accountID {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeRepo) Create(_ context.Context, r *resource.Resource) (*resource.Resource, error) {
	for _, ex := range f.res {
		if ex.AccountID == r.AccountID && ex.Kind == r.Kind && ex.Name == r.Name {
			return nil, errs.New(errs.KindAlreadyExists, "recurso já existe")
		}
	}
	r.ID = f.id("res")
	cp := *r
	f.res[r.ID] = &cp
	out := cp
	return &out, nil
}

func (f *fakeRepo) Update(_ context.Context, accountID, id string, config map[string]any, bump bool) (*resource.Resource, error) {
	r, ok := f.res[id]
	if !ok || r.AccountID != accountID {
		return nil, errs.NotFound("recurso")
	}
	r.Config = config
	if bump {
		r.Version++
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRepo) Delete(_ context.Context, accountID, id string) error {
	r, ok := f.res[id]
	if !ok || r.AccountID != accountID {
		return errs.NotFound("recurso")
	}
	delete(f.res, id)
	return nil
}

func (f *fakeRepo) SetCredentialRef(_ context.Context, accountID, id, ref string) (*resource.Resource, error) {
	r, ok := f.res[id]
	if !ok || r.AccountID != accountID {
		return nil, errs.NotFound("recurso")
	}
	r.CredentialRef = ref
	cp := *r
	return &cp, nil
}

func (f *fakeRepo) GrantsOfUser(_ context.Context, accountID, userID string) ([]resource.Grant, error) {
	var out []resource.Grant
	for _, g := range f.grants {
		if r, ok := f.res[g.ResourceID]; ok && r.AccountID == accountID && g.UserID == userID {
			out = append(out, *g)
		}
	}
	return out, nil
}

func (f *fakeRepo) GrantOf(_ context.Context, accountID, resourceID, userID string) (*resource.Grant, error) {
	for _, g := range f.grants {
		if g.ResourceID == resourceID && g.UserID == userID {
			if r, ok := f.res[resourceID]; ok && r.AccountID == accountID {
				cp := *g
				return &cp, nil
			}
		}
	}
	return nil, nil
}

func (f *fakeRepo) GrantByID(_ context.Context, accountID, grantID string) (*resource.Grant, error) {
	g, ok := f.grants[grantID]
	if !ok {
		return nil, nil
	}
	if r, ok := f.res[g.ResourceID]; !ok || r.AccountID != accountID {
		return nil, nil
	}
	cp := *g
	return &cp, nil
}

func (f *fakeRepo) Grant(_ context.Context, accountID string, g *resource.Grant) (*resource.Grant, error) {
	r, ok := f.res[g.ResourceID]
	if !ok || r.AccountID != accountID {
		return nil, errs.NotFound("recurso")
	}
	for _, ex := range f.grants { // upsert por (recurso, usuário)
		if ex.ResourceID == g.ResourceID && ex.UserID == g.UserID {
			ex.Level = g.Level
			cp := *ex
			return &cp, nil
		}
	}
	g.ID = f.id("grant")
	cp := *g
	f.grants[g.ID] = &cp
	out := cp
	return &out, nil
}

func (f *fakeRepo) RevokeGrant(_ context.Context, accountID, grantID string) error {
	g, ok := f.grants[grantID]
	if !ok {
		return errs.NotFound("concessão")
	}
	if r, ok := f.res[g.ResourceID]; !ok || r.AccountID != accountID {
		return errs.NotFound("concessão")
	}
	delete(f.grants, grantID)
	return nil
}

var _ resource.Repository = (*fakeRepo)(nil)

// fakeAccess é a porta estreita de identidade: papel na conta e natureza da
// conta, nada mais.
type fakeAccess struct {
	account identity.Account
	members map[string]identity.Role
}

func (f *fakeAccess) Authorize(_ context.Context, userID, accountID string) (*identity.Membership, error) {
	if accountID != f.account.ID {
		return nil, errs.Permission("sem vínculo com esta conta")
	}
	role, ok := f.members[userID]
	if !ok {
		return nil, errs.Permission("sem vínculo com esta conta")
	}
	return &identity.Membership{UserID: userID, AccountID: accountID, Role: role}, nil
}

func (f *fakeAccess) GetAccount(_ context.Context, id string) (*identity.Account, error) {
	if id != f.account.ID {
		return nil, errs.NotFound("conta")
	}
	a := f.account
	return &a, nil
}

var _ resource.Access = (*fakeAccess)(nil)

// ── cenário padrão: uma organização com owner, developer e viewer ────────────

const acctID = "acct-1"

func cenario() (*resource.Service, *fakeRepo, *fakeAccess, *cofreFake) {
	repo := newFakeRepo()
	acc := &fakeAccess{
		account: identity.Account{ID: acctID, Kind: identity.AccountOrganization, Handle: "acme"},
		members: map[string]identity.Role{
			"dono": identity.RoleOwner,
			"dev":  identity.RoleDeveloper,
			"dev2": identity.RoleDeveloper,
			"leit": identity.RoleViewer,
		},
	}
	cofre := novoCofre()
	return resource.NewService(repo, acc, cofre), repo, acc, cofre
}

func comoAtor(user string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: acctID, ActorID: user, ActorKind: ctxutil.ActorUser,
	})
}

func integracaoConfig() map[string]any {
	return map[string]any{"category": "git", "provider": "github"}
}

// ── testes do serviço ────────────────────────────────────────────────────────

func TestOperacaoSemContaAtivaERecusada(t *testing.T) {
	svc, _, _, _ := cenario()
	// Sem AccountID: regra do SP-0 — requisição sem conta ativa é inválida.
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "dono"})
	if _, err := svc.List(ctx, ""); err == nil {
		t.Error("operação sem conta ativa deveria ser recusada")
	}
}

func TestCreateValidaTipoNomeECategoria(t *testing.T) {
	svc, _, _, _ := cenario()
	ctx := comoAtor("dono")

	if _, err := svc.Create(ctx, "database", "x", nil); err == nil {
		t.Error("tipo desconhecido deveria ser recusado")
	}
	if _, err := svc.Create(ctx, resource.KindSkill, "  ", nil); err == nil {
		t.Error("nome vazio deveria ser recusado")
	}
	if _, err := svc.Create(ctx, resource.KindIntegration, "gh", map[string]any{"provider": "github"}); err == nil {
		t.Error("integração sem categoria deveria ser recusada")
	}
	if _, err := svc.Create(ctx, resource.KindIntegration, "gh", integracaoConfig()); err != nil {
		t.Errorf("integração bem formada deveria ser aceita: %v", err)
	}
	// Viewer é papel de leitura.
	if _, err := svc.Create(comoAtor("leit"), resource.KindSkill, "revisão", nil); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("viewer não deveria criar recurso; erro: %v", err)
	}
}

// Sem manage explícito ao criador, quem conecta a integração perde o acesso a
// ela no instante seguinte — o default fechado viraria armadilha.
func TestCriadorDeIntegracaoRecebeManage(t *testing.T) {
	svc, _, _, _ := cenario()
	ctx := comoAtor("dev")

	r, err := svc.Create(ctx, resource.KindIntegration, "github", integracaoConfig())
	if err != nil {
		t.Fatalf("criação: %v", err)
	}
	if _, err := svc.Update(ctx, r.ID, integracaoConfig()); err != nil {
		t.Errorf("o criador deveria gerenciar a própria integração: %v", err)
	}
	// E o outro developer continua de fora: fechada é fechada.
	if _, err := svc.Get(comoAtor("dev2"), r.ID); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("integração alheia não deveria ser acessível; erro: %v", err)
	}
}

func TestListFiltraPeloQueOAtorPodeUsar(t *testing.T) {
	svc, _, _, _ := cenario()
	dono := comoAtor("dono")

	if _, err := svc.Create(dono, resource.KindIntegration, "github", integracaoConfig()); err != nil {
		t.Fatal(err)
	}
	skill, err := svc.Create(dono, resource.KindSkill, "revisar-pr", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Owner vê tudo — manage implícito.
	todos, err := svc.List(dono, "")
	if err != nil || len(todos) != 2 {
		t.Fatalf("owner deveria ver os dois recursos, veio %d (%v)", len(todos), err)
	}

	// Developer vê a skill (conteúdo é aberto na organização) e NÃO vê a
	// integração criada pelo owner (credencial nasce fechada).
	visiveis, err := svc.List(comoAtor("dev2"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(visiveis) != 1 || visiveis[0].ID != skill.ID {
		t.Fatalf("developer deveria ver só a skill, veio %+v", visiveis)
	}
}

func TestUpdateVersionaConteudoMasNaoIntegracao(t *testing.T) {
	svc, _, _, _ := cenario()
	ctx := comoAtor("dono")

	skill, _ := svc.Create(ctx, resource.KindSkill, "revisar-pr", map[string]any{"body": "v1"})
	atualizada, err := svc.Update(ctx, skill.ID, map[string]any{"body": "v2"})
	if err != nil {
		t.Fatalf("update da skill: %v", err)
	}
	if atualizada.Version != skill.Version+1 {
		t.Errorf("conteúdo deveria ser versionado: %d → %d", skill.Version, atualizada.Version)
	}

	inte, _ := svc.Create(ctx, resource.KindIntegration, "github", integracaoConfig())
	cfg := integracaoConfig()
	cfg["base_url"] = "https://git.acme.dev"
	depois, err := svc.Update(ctx, inte.ID, cfg)
	if err != nil {
		t.Fatalf("update da integração: %v", err)
	}
	if depois.Version != inte.Version {
		t.Errorf("integração não é versionada: %d → %d", inte.Version, depois.Version)
	}
}

// Developer só tem USE sobre a skill aberta — alterar exige manage.
func TestUpdateDeConteudoExigeManage(t *testing.T) {
	svc, _, _, _ := cenario()
	skill, _ := svc.Create(comoAtor("dono"), resource.KindSkill, "revisar-pr", nil)

	if _, err := svc.Get(comoAtor("dev"), skill.ID); err != nil {
		t.Errorf("developer deveria poder LER a skill aberta: %v", err)
	}
	if _, err := svc.Update(comoAtor("dev"), skill.ID, map[string]any{"body": "x"}); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("developer sem manage não deveria alterar a skill; erro: %v", err)
	}
}

func TestGrantExigeManageEMembroDaConta(t *testing.T) {
	svc, _, _, _ := cenario()
	inte, _ := svc.Create(comoAtor("dono"), resource.KindIntegration, "github", integracaoConfig())

	// Quem não gerencia o recurso não concede.
	if _, err := svc.Grant(comoAtor("dev"), inte.ID, "dev2", resource.LevelUse); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("developer sem manage não deveria conceder; erro: %v", err)
	}
	// Nível fora do vocabulário.
	if _, err := svc.Grant(comoAtor("dono"), inte.ID, "dev", "admin"); err == nil {
		t.Error("nível de concessão inválido deveria ser recusado")
	}
	// Concessão não é porta de entrada na conta.
	if _, err := svc.Grant(comoAtor("dono"), inte.ID, "estranho", resource.LevelUse); err == nil ||
		errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("não-membro não deveria receber concessão; erro: %v", err)
	}

	g, err := svc.Grant(comoAtor("dono"), inte.ID, "dev", resource.LevelUse)
	if err != nil {
		t.Fatalf("owner deveria conceder: %v", err)
	}
	if _, err := svc.Get(comoAtor("dev"), inte.ID); err != nil {
		t.Errorf("concessão deveria destravar o acesso: %v", err)
	}

	// Revogar fecha de novo.
	if err := svc.RevokeGrant(comoAtor("dono"), g.ID); err != nil {
		t.Fatalf("revogação: %v", err)
	}
	if _, err := svc.Get(comoAtor("dev"), inte.ID); err == nil {
		t.Error("depois da revogação o acesso deveria fechar")
	}
	// ...mas nunca para o owner: o manage dele não passa por esta tabela.
	if _, err := svc.Get(comoAtor("dono"), inte.ID); err != nil {
		t.Errorf("owner não perde acesso por revogação: %v", err)
	}
}

// TestSetCredential é o outro teste central: o segredo vai para o SecretStore e
// a linha guarda APENAS o ponteiro opaco.
func TestSetCredentialGuardaSegredoForaDoBanco(t *testing.T) {
	svc, repo, _, cofre := cenario()
	ctx := comoAtor("dono")
	inte, _ := svc.Create(ctx, resource.KindIntegration, "github", integracaoConfig())

	segredo := []byte("ghp_token_super_secreto")
	ref, err := svc.SetCredential(ctx, inte.ID, segredo)
	if err != nil {
		t.Fatalf("SetCredential: %v", err)
	}

	// A referência é opaca: não carrega o segredo.
	if ref == "" || strings.Contains(ref, string(segredo)) {
		t.Fatalf("a referência não pode conter o segredo: %q", ref)
	}
	// A linha guarda o ponteiro, e só ele.
	linha, _ := repo.ByID(ctx, acctID, inte.ID)
	if linha.CredentialRef != ref {
		t.Errorf("a linha deveria guardar a referência, veio %q", linha.CredentialRef)
	}
	for _, v := range linha.Config {
		if s, ok := v.(string); ok && strings.Contains(s, string(segredo)) {
			t.Fatal("o segredo vazou para o config do recurso")
		}
	}
	// O valor está no cofre, resolvido pela referência LÓGICA.
	guardado, err := cofre.Get(ctx, resource.SecretRefFor(acctID, inte.ID))
	if err != nil {
		t.Fatalf("leitura no cofre: %v", err)
	}
	if !bytes.Equal(guardado, segredo) {
		t.Error("o cofre deveria guardar exatamente o valor enviado")
	}
	// E o valor nunca se imprime — nem em log, nem em erro.
	if got := ports.SecretValue(segredo).String(); got != "***" {
		t.Errorf("SecretValue não pode se imprimir: %q", got)
	}

	// Rotação: gravar de novo substitui.
	if _, err := svc.SetCredential(ctx, inte.ID, []byte("ghp_novo")); err != nil {
		t.Fatalf("rotação: %v", err)
	}
	novo, _ := cofre.Get(ctx, resource.SecretRefFor(acctID, inte.ID))
	if string(novo) != "ghp_novo" {
		t.Error("a rotação deveria substituir o valor guardado")
	}
}

func TestSetCredentialSoValeParaIntegracao(t *testing.T) {
	svc, _, _, cofre := cenario()
	ctx := comoAtor("dono")

	skill, _ := svc.Create(ctx, resource.KindSkill, "revisar-pr", nil)
	if _, err := svc.SetCredential(ctx, skill.ID, []byte("x")); err == nil ||
		errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("recurso de conteúdo não tem credencial; erro: %v", err)
	}
	if existe, _ := cofre.Exists(ctx, resource.SecretRefFor(acctID, skill.ID)); existe {
		t.Error("nada deveria ter sido escrito no cofre")
	}

	inte, _ := svc.Create(ctx, resource.KindIntegration, "github", integracaoConfig())
	if _, err := svc.SetCredential(ctx, inte.ID, nil); err == nil ||
		errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("credencial vazia deveria ser recusada; erro: %v", err)
	}
	// Quem não gerencia a integração não troca a credencial dela.
	if _, err := svc.SetCredential(comoAtor("dev"), inte.ID, []byte("x")); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("developer sem manage não deveria gravar credencial; erro: %v", err)
	}
}

// A credencial não sobrevive ao recurso: cofre primeiro, linha depois.
func TestDeleteRemoveACredencialDoCofre(t *testing.T) {
	svc, _, _, cofre := cenario()
	ctx := comoAtor("dono")

	inte, _ := svc.Create(ctx, resource.KindIntegration, "github", integracaoConfig())
	if _, err := svc.SetCredential(ctx, inte.ID, []byte("segredo")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, inte.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if existe, _ := cofre.Exists(ctx, resource.SecretRefFor(acctID, inte.ID)); existe {
		t.Error("o segredo não pode sobreviver ao recurso apagado")
	}
	if _, err := svc.Get(ctx, inte.ID); err == nil || errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("o recurso apagado não deveria ser encontrado; erro: %v", err)
	}
}

// Numa conta pessoal não há segundo membro: o recurso nunca é compartilhável, e
// isso sai da cardinalidade, sem regra especial.
func TestRecursoDeContaPessoalNaoECompartilhavel(t *testing.T) {
	repo := newFakeRepo()
	acc := &fakeAccess{
		account: identity.Account{ID: acctID, Kind: identity.AccountPersonal, Handle: "ed"},
		members: map[string]identity.Role{"ed": identity.RoleOwner},
	}
	svc := resource.NewService(repo, acc, novoCofre())
	ctx := comoAtor("ed")

	skill, err := svc.Create(ctx, resource.KindSkill, "revisar-pr", nil)
	if err != nil {
		t.Fatalf("criação: %v", err)
	}
	if _, err := svc.Grant(ctx, skill.ID, "qualquer-outro", resource.LevelUse); err == nil ||
		errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("não há a quem conceder numa conta pessoal; erro: %v", err)
	}
	// E o único membro é owner: gerencia tudo, por manage implícito.
	if _, err := svc.Update(ctx, skill.ID, map[string]any{"body": "v2"}); err != nil {
		t.Errorf("o dono da conta pessoal gerencia os próprios recursos: %v", err)
	}
}
