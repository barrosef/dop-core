package workflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM banco: repositório, linhagem, identidade e relógio
// são portas, e aqui entram duplos em memória. É o retorno prático da
// arquitetura hexagonal — e o motivo de os duplos morarem NESTE arquivo: o
// teste de arquitetura varre os _test.go também, e um duplo em
// internal/adapter seria o domínio importando infra por uma porta dos fundos.

// ── duplos ───────────────────────────────────────────────────────────────────

// relogioFixo torna determinístico o que depende de tempo. Um relógio de
// parede aqui faria o teste de versionamento passar por sorte.
type relogioFixo struct{ t time.Time }

func (r *relogioFixo) Now() time.Time { r.t = r.t.Add(time.Second); return r.t }

type registro struct {
	meta     workflow.Flow // identidade: dono, conta, versão corrente
	versoes  map[int32]workflow.Flow
	corrente int32
}

type fakeRepo struct {
	flows  map[string]*registro
	chaves map[string]string // chave de idempotência → id do fluxo
	nextID int
	// Contadores para provar as promessas de forma da porta.
	byOwnersCalls int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{flows: map[string]*registro{}, chaves: map[string]string{}}
}

func (f *fakeRepo) id() string {
	f.nextID++
	return "flw-" + string(rune('a'+f.nextID-1))
}

func (f *fakeRepo) visivel(accountID string, r *registro) bool {
	return r.meta.AccountID == accountID || r.meta.OwnerScope == workflow.ScopePlatform
}

func (f *fakeRepo) corrente(r *registro) workflow.Flow { return r.versoes[r.corrente] }

func (f *fakeRepo) List(_ context.Context, accountID string, scope workflow.Scope, ownerID string) ([]workflow.Flow, error) {
	var out []workflow.Flow
	for _, r := range f.flows {
		if !f.visivel(accountID, r) {
			continue
		}
		if scope != "" && r.meta.OwnerScope != scope {
			continue
		}
		if ownerID != "" && r.meta.OwnerID != ownerID {
			continue
		}
		out = append(out, f.corrente(r))
	}
	return out, nil
}

func (f *fakeRepo) ByID(_ context.Context, accountID, id string) (*workflow.Flow, error) {
	r, ok := f.flows[id]
	if !ok || !f.visivel(accountID, r) {
		return nil, errs.NotFound("fluxo")
	}
	cur := f.corrente(r)
	return &cur, nil
}

func (f *fakeRepo) VersionOf(_ context.Context, accountID, id string, version int32) (*workflow.Flow, error) {
	r, ok := f.flows[id]
	if !ok || !f.visivel(accountID, r) {
		return nil, errs.NotFound("fluxo")
	}
	v, ok := r.versoes[version]
	if !ok {
		return nil, errs.NotFound("versão do fluxo")
	}
	return &v, nil
}

func (f *fakeRepo) ByOwners(_ context.Context, accountID string, refs []workflow.ScopeRef) ([]workflow.Flow, error) {
	f.byOwnersCalls++
	want := make(map[workflow.ScopeRef]bool, len(refs))
	for _, ref := range refs {
		want[ref] = true
	}
	var out []workflow.Flow
	for _, r := range f.flows {
		if f.visivel(accountID, r) && want[r.meta.Ref()] {
			out = append(out, f.corrente(r))
		}
	}
	return out, nil
}

func (f *fakeRepo) Create(_ context.Context, flow *workflow.Flow, key string) (*workflow.Flow, error) {
	if id, repetida := f.chaves[key]; repetida {
		cur := f.corrente(f.flows[id])
		return &cur, nil
	}
	for _, r := range f.flows {
		if r.meta.Ref() == flow.Ref() {
			return nil, errs.New(errs.KindAlreadyExists, "o nível já tem fluxo")
		}
	}
	cp := *flow
	cp.ID = f.id()
	cp.Version = 1
	f.flows[cp.ID] = &registro{meta: cp, versoes: map[int32]workflow.Flow{1: cp}, corrente: 1}
	f.chaves[key] = cp.ID
	return &cp, nil
}

func (f *fakeRepo) AppendVersion(_ context.Context, accountID string, flow *workflow.Flow, base int32, key string) (*workflow.Flow, error) {
	r, ok := f.flows[flow.ID]
	if !ok || r.meta.AccountID != accountID {
		return nil, errs.NotFound("fluxo")
	}
	if id, repetida := f.chaves[key]; repetida {
		cur := f.corrente(f.flows[id])
		return &cur, nil
	}
	if r.corrente != base {
		return nil, errs.Conflict("o fluxo já está na versão %d", r.corrente)
	}
	cp := *flow
	cp.Version = r.corrente + 1
	r.versoes[cp.Version] = cp
	r.corrente = cp.Version
	r.meta = cp
	f.chaves[key] = cp.ID
	return &cp, nil
}

func (f *fakeRepo) Promote(_ context.Context, accountID string, src *workflow.Flow, target workflow.ScopeRef, key string) (*workflow.Flow, error) {
	if id, repetida := f.chaves[key]; repetida {
		cur := f.corrente(f.flows[id])
		return &cur, nil
	}
	for _, r := range f.flows {
		if r.meta.Ref() == target && r.meta.AccountID == accountID {
			cp := *src
			cp.ID = r.meta.ID
			cp.OwnerScope, cp.OwnerID = target.Scope, target.ID
			cp.Version = r.corrente + 1
			r.versoes[cp.Version] = cp
			r.corrente = cp.Version
			r.meta = cp
			f.chaves[key] = cp.ID
			return &cp, nil
		}
	}
	cp := *src
	cp.ID = f.id()
	cp.AccountID = accountID
	cp.OwnerScope, cp.OwnerID = target.Scope, target.ID
	cp.Version = 1
	f.flows[cp.ID] = &registro{meta: cp, versoes: map[int32]workflow.Flow{1: cp}, corrente: 1}
	f.chaves[key] = cp.ID
	return &cp, nil
}

var _ workflow.Repository = (*fakeRepo)(nil)

// fakeTree é a porta estreita da linhagem: quem está dentro de quem.
type fakeTree struct {
	workspaceDe map[string]string // projeto → workspace
	contaDe     map[string]string // workspace → conta
	projetoDe   map[string]string // demanda → projeto
}

func (t *fakeTree) ChainOf(_ context.Context, accountID string, target workflow.ScopeRef) ([]workflow.ScopeRef, error) {
	base := []workflow.ScopeRef{{Scope: workflow.ScopePlatform}}
	switch target.Scope {
	case workflow.ScopePlatform:
		return base, nil
	case workflow.ScopeAccount:
		if target.ID != accountID {
			return nil, errs.NotFound("conta")
		}
		return append(base, target), nil
	}
	base = append(base, workflow.ScopeRef{Scope: workflow.ScopeAccount, ID: accountID})

	projeto := ""
	workspace := ""
	switch target.Scope {
	case workflow.ScopeWorkspace:
		workspace = target.ID
	case workflow.ScopeProject:
		projeto = target.ID
		workspace = t.workspaceDe[projeto]
	case workflow.ScopeDemand:
		projeto = t.projetoDe[target.ID]
		workspace = t.workspaceDe[projeto]
	}
	if workspace == "" || t.contaDe[workspace] != accountID {
		return nil, errs.NotFound("%s", target.Scope.Label())
	}
	out := append(base, workflow.ScopeRef{Scope: workflow.ScopeWorkspace, ID: workspace})
	if projeto != "" {
		out = append(out, workflow.ScopeRef{Scope: workflow.ScopeProject, ID: projeto})
	}
	if target.Scope == workflow.ScopeDemand {
		out = append(out, target)
	}
	return out, nil
}

var _ workflow.Ancestry = (*fakeTree)(nil)

// fakeAccess é a porta estreita de identidade: só o papel na conta ativa.
type fakeAccess struct{ papel map[string]string }

func (a *fakeAccess) RoleOf(_ context.Context, userID, accountID string) (string, error) {
	p, ok := a.papel[userID+"@"+accountID]
	if !ok {
		return "", errs.Permission("sem vínculo com esta conta")
	}
	return p, nil
}

var _ workflow.Access = (*fakeAccess)(nil)

// ── cenário ──────────────────────────────────────────────────────────────────

const (
	conta     = "acct-1"
	workspace = "ws-1"
	projeto   = "prj-1"
	demanda   = "dmd-1"
	dono      = "usr-owner"
	membro    = "usr-dev"
)

func cenario(t *testing.T) (*fakeRepo, *workflow.Service, context.Context) {
	t.Helper()
	repo := newFakeRepo()
	tree := &fakeTree{
		workspaceDe: map[string]string{projeto: workspace},
		contaDe:     map[string]string{workspace: conta, "ws-vizinho": conta, "ws-alheio": "acct-2"},
		projetoDe:   map[string]string{demanda: projeto},
	}
	acc := &fakeAccess{papel: map[string]string{
		dono + "@" + conta:   workflow.RoleOwner,
		membro + "@" + conta: "developer",
	}}
	svc := workflow.NewService(repo, tree, acc, &relogioFixo{t: time.Unix(1_700_000_000, 0).UTC()})
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: conta, ActorID: dono, ActorKind: ctxutil.ActorUser,
	})
	return repo, svc, ctx
}

// semear grava direto no duplo um fluxo já existente num nível da cadeia.
func semear(repo *fakeRepo, accountID string, scope workflow.Scope, ownerID string, stages ...workflow.StageSpec) workflow.Flow {
	f := workflow.Flow{
		AccountID: accountID, OwnerScope: scope, OwnerID: ownerID,
		Name: string(scope) + " flow", Version: 1, Stages: stages,
	}
	f.ID = repo.id()
	repo.flows[f.ID] = &registro{meta: f, versoes: map[int32]workflow.Flow{1: f}, corrente: 1}
	return f
}

func etapa(key string, tipo workflow.StageType, artefatos ...workflow.ArtifactKind) workflow.StageSpec {
	return workflow.StageSpec{
		Key: key, Name: key, Type: tipo, Artifacts: artefatos, Gate: workflow.GateNone,
	}
}

func fluxoValido() workflow.Flow {
	return workflow.Flow{
		Name:       "Padrão do time",
		OwnerScope: workflow.ScopeProject,
		OwnerID:    projeto,
		Stages: []workflow.StageSpec{
			etapa("contexto", workflow.TypeContext, workflow.ArtifactDocument),
			etapa("spec", workflow.TypeSpec, workflow.ArtifactSpec),
			{Key: "validacao", Name: "Validação", Type: workflow.TypeHumanValidation,
				Artifacts: []workflow.ArtifactKind{workflow.ArtifactReport}, Gate: workflow.GateHuman},
		},
	}
}

// ── a cadeia de herança ──────────────────────────────────────────────────────

// O teste central do domínio: a cadeia inteira, com sobreposição e PROCEDÊNCIA.
// Sem a procedência, ninguém consegue depurar por que uma demanda seguiu um
// fluxo que ninguém lembra de ter escrito (ADR-0014, consequências).
func TestCadeiaDeHerancaSobrepoeEDizDeOndeVeioCadaEtapa(t *testing.T) {
	repo, svc, ctx := cenario(t)

	// Plataforma: o catálogo, nível 0.
	semear(repo, "", workflow.ScopePlatform, "",
		etapa("contexto", workflow.TypeContext, workflow.ArtifactDocument),
		etapa("spec", workflow.TypeSpec, workflow.ArtifactSpec),
		etapa("implementacao", workflow.TypeImplementation))
	// Conta: reescreve a spec (mesma chave) — sobrepõe por declaração.
	semear(repo, conta, workflow.ScopeAccount, conta,
		etapa("spec", workflow.TypeSpec, workflow.ArtifactSpec, workflow.ArtifactDiagram))
	// Workspace: não declara nada — herda por omissão.
	// Projeto: acrescenta uma etapa nova, que entra no fim.
	semear(repo, conta, workflow.ScopeProject, projeto,
		etapa("teste", workflow.TypeTest, workflow.ArtifactTestPlan))

	eff, err := svc.Resolve(ctx, workflow.ScopeProject, projeto)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	esperado := []string{"contexto", "spec", "implementacao", "teste"}
	if len(eff.Flow.Stages) != len(esperado) {
		t.Fatalf("esperadas %d etapas, vieram %d (%v)", len(esperado), len(eff.Flow.Stages), eff.Flow.Stages)
	}
	for i, key := range esperado {
		if eff.Flow.Stages[i].Key != key {
			t.Errorf("etapa %d deveria ser %q, veio %q", i, key, eff.Flow.Stages[i].Key)
		}
	}

	// A spec da conta venceu a da plataforma — e a posição herdada foi mantida.
	if got := len(eff.Flow.Stages[1].Artifacts); got != 2 {
		t.Errorf("a spec da conta deveria ter vencido a da plataforma (%d artefatos)", got)
	}

	// A procedência, etapa por etapa.
	proc := map[string]workflow.Scope{
		"contexto":      workflow.ScopePlatform,
		"spec":          workflow.ScopeAccount,
		"implementacao": workflow.ScopePlatform,
		"teste":         workflow.ScopeProject,
	}
	for key, esperada := range proc {
		origem, ok := eff.OriginOf(key)
		if !ok {
			t.Fatalf("etapa %q sem procedência registrada", key)
		}
		if origem.Scope != esperada {
			t.Errorf("etapa %q deveria vir de %s, veio de %s", key, esperada, origem.Scope)
		}
	}

	// O rastro visível, do mais específico ao mais genérico. O workspace não
	// aparece: ele não declarou nada, então não contribuiu.
	if !strings.HasPrefix(eff.ResolvedFrom, "projeto ◂ conta ◂ plataforma") {
		t.Errorf("rastro deveria começar em 'projeto ◂ conta ◂ plataforma', veio %q", eff.ResolvedFrom)
	}
	if strings.Contains(eff.ResolvedFrom, "workspace") {
		t.Errorf("nível que não declarou nada não pode aparecer no rastro: %q", eff.ResolvedFrom)
	}
	if !strings.Contains(eff.ResolvedFrom, "spec (conta)") {
		t.Errorf("o rastro deveria dizer de onde veio cada etapa: %q", eff.ResolvedFrom)
	}

	// A cadeia inteira custa UMA ida ao repositório, não uma por nível.
	if repo.byOwnersCalls != 1 {
		t.Errorf("a resolução deveria custar UMA consulta, custou %d", repo.byOwnersCalls)
	}
}

func TestResolveDeDemandaAtravessaACadeiaInteira(t *testing.T) {
	repo, svc, ctx := cenario(t)
	semear(repo, "", workflow.ScopePlatform, "", etapa("contexto", workflow.TypeContext))
	semear(repo, conta, workflow.ScopeDemand, demanda, etapa("hotfix", workflow.TypeGeneric))

	eff, err := svc.Resolve(ctx, workflow.ScopeDemand, demanda)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(eff.Flow.Stages) != 2 {
		t.Fatalf("a demanda deveria herdar a plataforma e somar a sua etapa: %v", eff.Flow.Stages)
	}
	if eff.Contributors[0].Scope != workflow.ScopeDemand {
		t.Errorf("o nível mais específico deveria abrir o rastro, veio %v", eff.Contributors)
	}
}

func TestResolveSemNenhumNivelDeclaradoNaoInventaFluxo(t *testing.T) {
	_, svc, ctx := cenario(t)
	if _, err := svc.Resolve(ctx, workflow.ScopeProject, projeto); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("cadeia vazia deveria dar NotFound; erro: %v", err)
	}
}

func TestResolveNaoAlcancaOutraConta(t *testing.T) {
	repo, svc, ctx := cenario(t)
	semear(repo, "acct-2", workflow.ScopeWorkspace, "ws-alheio", etapa("x", workflow.TypeGeneric))
	if _, err := svc.Resolve(ctx, workflow.ScopeWorkspace, "ws-alheio"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("workspace de outra conta deveria dar NotFound; erro: %v", err)
	}
}

func TestResolveExigeContaAtiva(t *testing.T) {
	_, svc, _ := cenario(t)
	if _, err := svc.Resolve(context.Background(), workflow.ScopeAccount, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Fatal("requisição sem conta ativa é inválida por definição")
	}
}

// ── congelamento de versão ───────────────────────────────────────────────────

// A invariante que protege a demanda em execução: atualizar GERA versão nova e
// a anterior fica exatamente como estava.
func TestUpdateGeraVersaoNovaSemTocarNaAnterior(t *testing.T) {
	_, svc, ctx := cenario(t)
	criado, err := svc.Create(ctx, fluxoValido(), "k1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if criado.Version != 1 {
		t.Fatalf("o fluxo nasce na versão 1, veio %d", criado.Version)
	}

	alterado := *criado
	alterado.Stages = append([]workflow.StageSpec{}, criado.Stages...)
	alterado.Stages[0].Name = "Contexto revisado"
	atualizado, err := svc.Update(ctx, alterado)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if atualizado.Version != 2 {
		t.Fatalf("Update deveria gerar a versão 2, veio %d", atualizado.Version)
	}

	// O que a demanda congelou continua exatamente como estava.
	congelada, err := svc.GetVersion(ctx, criado.ID, 1)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if congelada.Stages[0].Name != criado.Stages[0].Name {
		t.Errorf("a versão 1 foi reescrita: %q virou %q",
			criado.Stages[0].Name, congelada.Stages[0].Name)
	}
	if congelada.Version != 1 {
		t.Errorf("GetVersion(1) devolveu a versão %d", congelada.Version)
	}

	// E a versão corrente é a nova.
	atual, err := svc.Get(ctx, criado.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if atual.Version != 2 || atual.Stages[0].Name != "Contexto revisado" {
		t.Errorf("a versão corrente deveria ser a 2 revisada, veio v%d %q",
			atual.Version, atual.Stages[0].Name)
	}
}

func TestUpdateSobreVersaoDesatualizadaEConflito(t *testing.T) {
	_, svc, ctx := cenario(t)
	criado, _ := svc.Create(ctx, fluxoValido(), "k1")

	primeira := *criado
	primeira.Stages = append([]workflow.StageSpec{}, criado.Stages...)
	primeira.Stages[0].Name = "A"
	if _, err := svc.Update(ctx, primeira); err != nil {
		t.Fatalf("primeira edição: %v", err)
	}

	// A segunda pessoa ainda estava com a versão 1 aberta na tela.
	segunda := *criado
	segunda.Stages = append([]workflow.StageSpec{}, criado.Stages...)
	segunda.Stages[0].Name = "B"
	if _, err := svc.Update(ctx, segunda); errs.KindOf(err) != errs.KindConflict {
		t.Fatalf("edição sobre versão vencida deveria dar Conflict; erro: %v", err)
	}
}

// Reenvio idêntico não versiona: um cliente com retry automático versionaria o
// fluxo para sempre, e a demanda apontaria para versões que ninguém escreveu.
func TestReenvioIdenticoNaoGeraVersao(t *testing.T) {
	_, svc, ctx := cenario(t)
	criado, _ := svc.Create(ctx, fluxoValido(), "k1")

	igual := *criado
	novo, err := svc.Update(ctx, igual)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if novo.Version != 1 {
		t.Errorf("reenvio idêntico não pode versionar; foi para a versão %d", novo.Version)
	}
}

func TestCreateRepetidoComAMesmaChaveNaoDuplica(t *testing.T) {
	repo, svc, ctx := cenario(t)
	primeiro, err := svc.Create(ctx, fluxoValido(), "k1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	segundo, err := svc.Create(ctx, fluxoValido(), "k1")
	if err != nil {
		t.Fatalf("repetição deveria devolver o mesmo fluxo: %v", err)
	}
	if primeiro.ID != segundo.ID || len(repo.flows) != 1 {
		t.Errorf("a chave de idempotência não impediu o fluxo gêmeo: %d fluxos", len(repo.flows))
	}
}

func TestCreateSemChaveDerivaUmaDoConteudo(t *testing.T) {
	repo, svc, ctx := cenario(t)
	if _, err := svc.Create(ctx, fluxoValido(), ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Create(ctx, fluxoValido(), ""); err != nil {
		t.Fatalf("o mesmo conteúdo deveria colidir na chave derivada: %v", err)
	}
	if len(repo.flows) != 1 {
		t.Errorf("escrita sem chave do cliente ainda precisa ser idempotente: %d fluxos", len(repo.flows))
	}
}

// ── recusas de ValidateFlow ──────────────────────────────────────────────────

func TestValidateRecusaCadaDefeitoQueQuebrariaUmaDemanda(t *testing.T) {
	casos := []struct {
		nome   string
		fluxo  workflow.Flow
		trecho string // pedaço da mensagem que localiza o problema
	}{
		{
			nome: "etapa órfã: sem chave, nenhum evento consegue apontar para ela",
			fluxo: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				{Key: "", Name: "sem chave", Type: workflow.TypeGeneric, Gate: workflow.GateNone},
			}},
			trecho: "órfã",
		},
		{
			nome: "ciclo: a mesma chave duas vezes",
			fluxo: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				etapa("spec", workflow.TypeSpec),
				etapa("spec", workflow.TypeSpec),
			}},
			trecho: "ciclo",
		},
		{
			nome: "portão sem decisor: validação humana que não interrompe",
			fluxo: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				{Key: "val", Name: "val", Type: workflow.TypeHumanValidation, Gate: workflow.GateNone,
					Artifacts: []workflow.ArtifactKind{workflow.ArtifactReport}},
			}},
			trecho: "não tem portão",
		},
		{
			nome: "portão sem decisor: portão humano sem nada sobre o que decidir",
			fluxo: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				{Key: "gate", Name: "gate", Type: workflow.TypeGeneric, Gate: workflow.GateHuman},
			}},
			trecho: "não há sobre o que decidir",
		},
		{
			nome: "tipo de etapa desconhecido",
			fluxo: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				{Key: "x", Name: "x", Type: workflow.StageType("revisao-mistica"), Gate: workflow.GateNone},
			}},
			trecho: "tipo desconhecido",
		},
		{
			nome:   "fluxo sem etapa nenhuma",
			fluxo:  workflow.Flow{Name: "f"},
			trecho: "sem etapa nenhuma",
		},
		{
			nome: "fluxo sem nome",
			fluxo: workflow.Flow{Stages: []workflow.StageSpec{
				etapa("spec", workflow.TypeSpec),
			}},
			trecho: "sem nome",
		},
		{
			nome: "artefato desconhecido",
			fluxo: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				etapa("x", workflow.TypeGeneric, workflow.ArtifactKind("pergaminho")),
			}},
			trecho: "artefato desconhecido",
		},
		{
			nome: "chave com espaço não sobrevive a evento nem a assunto",
			fluxo: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				etapa("valida cao", workflow.TypeGeneric),
			}},
			trecho: "espaço",
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			f := c.fluxo
			f.Normalize()
			rep := workflow.Validate(f)
			if rep.Valid() {
				t.Fatalf("deveria ser recusado; relatório: %+v", rep)
			}
			if !contemTrecho(rep.Errors, c.trecho) {
				t.Errorf("a mensagem precisa dizer QUAL etapa e POR QUÊ; veio %v", rep.Errors)
			}
			// E a recusa é do CLIENTE, não falha interna.
			if errs.KindOf(rep.Err()) != errs.KindInvalid {
				t.Errorf("erro de validação é do cliente: %v", rep.Err())
			}
		})
	}
}

func TestValidateAvisaSemRecusar(t *testing.T) {
	f := workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
		{Key: "impl", Name: "impl", Type: workflow.TypeImplementation, Gate: workflow.GateNone,
			Subtypes: []string{"aaa"}},
	}}
	f.Normalize()
	rep := workflow.Validate(f)
	if !rep.Valid() {
		t.Fatalf("nada aqui impede a execução: %v", rep.Errors)
	}
	if len(rep.Warnings) < 2 {
		t.Errorf("faltaram avisos (sem spec, sem portão, subetapas fora de teste): %v", rep.Warnings)
	}
}

// Em Create e Update a recusa vira erro: aqui o fluxo inválido de fato impede
// alguma coisa, e gravar seria deixar uma demanda futura sem resposta.
func TestCreateRecusaFluxoInvalido(t *testing.T) {
	repo, svc, ctx := cenario(t)
	f := fluxoValido()
	f.Stages = append(f.Stages, etapa("spec", workflow.TypeSpec)) // chave repetida
	if _, err := svc.Create(ctx, f, "k1"); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("fluxo com ciclo deveria dar Invalid; erro: %v", err)
	}
	if len(repo.flows) != 0 {
		t.Error("nada pode ter sido gravado")
	}
}

func TestValidateNaoGravaNada(t *testing.T) {
	repo, svc, ctx := cenario(t)
	rep, err := svc.Validate(ctx, fluxoValido())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !rep.Valid() {
		t.Errorf("o fluxo do cenário é válido: %v", rep.Errors)
	}
	if len(repo.flows) != 0 {
		t.Error("Validate é ensaio: não pode gravar")
	}
}

// ── nível e promoção ─────────────────────────────────────────────────────────

func TestCreateRecusaNivelDeOutraConta(t *testing.T) {
	_, svc, ctx := cenario(t)
	f := fluxoValido()
	f.OwnerScope, f.OwnerID = workflow.ScopeWorkspace, "ws-alheio"
	if _, err := svc.Create(ctx, f, "k1"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("nível de outra conta não existe daqui; erro: %v", err)
	}
}

func TestCreateNoCatalogoDaPlataformaERecusado(t *testing.T) {
	_, svc, ctx := cenario(t)
	f := fluxoValido()
	f.OwnerScope, f.OwnerID = workflow.ScopePlatform, ""
	if _, err := svc.Create(ctx, f, "k1"); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("o nível 0 vale para todas as contas; erro: %v", err)
	}
}

func TestPromoteSobeUmNivelEPublicaSemMoverAOrigem(t *testing.T) {
	_, svc, ctx := cenario(t)
	criado, _ := svc.Create(ctx, fluxoValido(), "k1")

	promovido, err := svc.Promote(ctx, criado.ID, workflow.ScopeWorkspace, workspace)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promovido.OwnerScope != workflow.ScopeWorkspace || promovido.OwnerID != workspace {
		t.Errorf("o fluxo deveria ter sido publicado no workspace, veio %s/%s",
			promovido.OwnerScope, promovido.OwnerID)
	}
	if promovido.ID == criado.ID {
		t.Error("promover PUBLICA, não move: a origem continua onde estava")
	}
	if origem, err := svc.Get(ctx, criado.ID); err != nil || origem.OwnerScope != workflow.ScopeProject {
		t.Errorf("o fluxo de origem sumiu do projeto: %v %v", origem, err)
	}
}

func TestPromoteParaNivelAbaixoOuIgualERecusado(t *testing.T) {
	_, svc, ctx := cenario(t)
	criado, _ := svc.Create(ctx, fluxoValido(), "k1")
	if _, err := svc.Promote(ctx, criado.ID, workflow.ScopeDemand, demanda); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("promoção desce a cadeia? erro: %v", err)
	}
	if _, err := svc.Promote(ctx, criado.ID, workflow.ScopeProject, projeto); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("promoção para o mesmo nível deveria ser recusada; erro: %v", err)
	}
}

// Dentro de uma conta PJ um fluxo de nível inferior é público DENTRO da conta —
// nunca fora dela (ADR-0014 §6 e §7).
func TestPromoteParaOCatalogoDaPlataformaERecusado(t *testing.T) {
	_, svc, ctx := cenario(t)
	criado, _ := svc.Create(ctx, fluxoValido(), "k1")
	if _, err := svc.Promote(ctx, criado.ID, workflow.ScopePlatform, ""); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("o catálogo não recebe fluxo de conta; erro: %v", err)
	}
}

func TestPromoteExigeManage(t *testing.T) {
	_, svc, ctx := cenario(t)
	criado, _ := svc.Create(ctx, fluxoValido(), "k1")

	// O mesmo pedido, feito por quem não tem manage sobre o conteúdo da conta.
	ctxDev := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: conta, ActorID: membro, ActorKind: ctxutil.ActorUser,
	})
	if _, err := svc.Promote(ctxDev, criado.ID, workflow.ScopeWorkspace, workspace); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("promover muda o fluxo de quem não pediu nada; erro: %v", err)
	}
}

func TestPromoteParaRamoVizinhoERecusado(t *testing.T) {
	_, svc, ctx := cenario(t)
	criado, _ := svc.Create(ctx, fluxoValido(), "k1")

	// ws-vizinho é da MESMA conta — e ainda assim está fora da linhagem deste
	// fluxo. Promover é subir na PRÓPRIA cadeia, não aterrissar num ramo ao
	// lado que por acaso pertence ao mesmo tenant.
	if _, err := svc.Promote(ctx, criado.ID, workflow.ScopeWorkspace, "ws-vizinho"); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("promoção para ramo vizinho deveria dar Invalid; erro: %v", err)
	}
}

func TestPromoteVersionaOFluxoQueJaExisteNoDestino(t *testing.T) {
	_, svc, ctx := cenario(t)
	base := fluxoValido()
	base.OwnerScope, base.OwnerID = workflow.ScopeWorkspace, workspace
	doWorkspace, err := svc.Create(ctx, base, "k0")
	if err != nil {
		t.Fatalf("Create no workspace: %v", err)
	}

	doProjeto, err := svc.Create(ctx, fluxoValido(), "k1")
	if err != nil {
		t.Fatalf("Create no projeto: %v", err)
	}

	promovido, err := svc.Promote(ctx, doProjeto.ID, workflow.ScopeWorkspace, workspace)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promovido.ID != doWorkspace.ID {
		t.Errorf("a promoção deveria versionar o fluxo do destino, não criar um segundo")
	}
	if promovido.Version != 2 {
		t.Errorf("o destino deveria ir para a versão 2, veio %d", promovido.Version)
	}
	// E a versão 1 do destino continua legível, para quem a congelou.
	if v1, err := svc.GetVersion(ctx, doWorkspace.ID, 1); err != nil || v1.Version != 1 {
		t.Errorf("a versão 1 do destino foi perdida: %v %v", v1, err)
	}
}

// ── portas obrigatórias ──────────────────────────────────────────────────────

// Relógio nulo desligaria a porta sem ninguém perceber e devolveria ao teste a
// dependência do relógio de parede que a porta existe para remover.
func TestNewServiceRecusaRelogioNulo(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("construtor deveria recusar relógio nulo")
		}
	}()
	workflow.NewService(newFakeRepo(), &fakeTree{}, &fakeAccess{}, nil)
}

func contemTrecho(msgs []string, trecho string) bool {
	for _, m := range msgs {
		if strings.Contains(m, trecho) {
			return true
		}
	}
	return false
}
