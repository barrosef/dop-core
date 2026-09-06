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

// The domain is testable WITHOUT a database: repository, lineage, identity and
// clock are ports, and in-memory doubles go in here. It is the practical return
// arquitetura hexagonal — e o motivo de os duplos morarem NESTE arquivo: o
// architecture test scans the _test.go files too, and a double under
// internal/adapter would be the domain importing infra through a back door.

// ── duplos ───────────────────────────────────────────────────────────────────

// fixedClock makes what depends on time deterministic. A wall clock here would
// make the versioning test pass by luck.
type fixedClock struct{ t time.Time }

func (r *fixedClock) Now() time.Time { r.t = r.t.Add(time.Second); return r.t }

type registro struct {
	meta     workflow.Flow // identity: owner, account, current version
	versions map[int32]workflow.Flow
	current  int32
}

type fakeRepo struct {
	flows  map[string]*registro
	keys   map[string]string // idempotency key → flow id
	nextID int
	// Counters, to prove the port's promises of shape.
	byOwnersCalls int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{flows: map[string]*registro{}, keys: map[string]string{}}
}

func (f *fakeRepo) id() string {
	f.nextID++
	return "flw-" + string(rune('a'+f.nextID-1))
}

func (f *fakeRepo) visivel(accountID string, r *registro) bool {
	return r.meta.AccountID == accountID || r.meta.OwnerScope == workflow.ScopePlatform
}

func (f *fakeRepo) current(r *registro) workflow.Flow { return r.versions[r.current] }

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
		out = append(out, f.current(r))
	}
	return out, nil
}

func (f *fakeRepo) ByID(_ context.Context, accountID, id string) (*workflow.Flow, error) {
	r, ok := f.flows[id]
	if !ok || !f.visivel(accountID, r) {
		return nil, errs.NotFound("flow")
	}
	cur := f.current(r)
	return &cur, nil
}

func (f *fakeRepo) VersionOf(_ context.Context, accountID, id string, version int32) (*workflow.Flow, error) {
	r, ok := f.flows[id]
	if !ok || !f.visivel(accountID, r) {
		return nil, errs.NotFound("flow")
	}
	v, ok := r.versions[version]
	if !ok {
		return nil, errs.NotFound("flow version")
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
			out = append(out, f.current(r))
		}
	}
	return out, nil
}

func (f *fakeRepo) Create(_ context.Context, flow *workflow.Flow, key string) (*workflow.Flow, error) {
	if id, repetida := f.keys[key]; repetida {
		cur := f.current(f.flows[id])
		return &cur, nil
	}
	for _, r := range f.flows {
		if r.meta.Ref() == flow.Ref() {
			return nil, errs.New(errs.KindAlreadyExists, "the level already has a flow")
		}
	}
	cp := *flow
	cp.ID = f.id()
	cp.Version = 1
	f.flows[cp.ID] = &registro{meta: cp, versions: map[int32]workflow.Flow{1: cp}, current: 1}
	f.keys[key] = cp.ID
	return &cp, nil
}

func (f *fakeRepo) AppendVersion(_ context.Context, accountID string, flow *workflow.Flow, base int32, key string) (*workflow.Flow, error) {
	r, ok := f.flows[flow.ID]
	if !ok || r.meta.AccountID != accountID {
		return nil, errs.NotFound("flow")
	}
	if id, repetida := f.keys[key]; repetida {
		cur := f.current(f.flows[id])
		return &cur, nil
	}
	if r.current != base {
		return nil, errs.Conflict("the flow is already at version %d", r.current)
	}
	cp := *flow
	cp.Version = r.current + 1
	r.versions[cp.Version] = cp
	r.current = cp.Version
	r.meta = cp
	f.keys[key] = cp.ID
	return &cp, nil
}

func (f *fakeRepo) Promote(_ context.Context, accountID string, src *workflow.Flow, target workflow.ScopeRef, key string) (*workflow.Flow, error) {
	if id, repetida := f.keys[key]; repetida {
		cur := f.current(f.flows[id])
		return &cur, nil
	}
	for _, r := range f.flows {
		if r.meta.Ref() == target && r.meta.AccountID == accountID {
			cp := *src
			cp.ID = r.meta.ID
			cp.OwnerScope, cp.OwnerID = target.Scope, target.ID
			cp.Version = r.current + 1
			r.versions[cp.Version] = cp
			r.current = cp.Version
			r.meta = cp
			f.keys[key] = cp.ID
			return &cp, nil
		}
	}
	cp := *src
	cp.ID = f.id()
	cp.AccountID = accountID
	cp.OwnerScope, cp.OwnerID = target.Scope, target.ID
	cp.Version = 1
	f.flows[cp.ID] = &registro{meta: cp, versions: map[int32]workflow.Flow{1: cp}, current: 1}
	f.keys[key] = cp.ID
	return &cp, nil
}

var _ workflow.Repository = (*fakeRepo)(nil)

// fakeTree is the lineage's narrow port: who is inside whom.
type fakeTree struct {
	workspaceDe map[string]string // project → workspace
	contaDe     map[string]string // workspace → account
	projetoDe   map[string]string // demand → project
}

func (t *fakeTree) ChainOf(_ context.Context, accountID string, target workflow.ScopeRef) ([]workflow.ScopeRef, error) {
	base := []workflow.ScopeRef{{Scope: workflow.ScopePlatform}}
	switch target.Scope {
	case workflow.ScopePlatform:
		return base, nil
	case workflow.ScopeAccount:
		if target.ID != accountID {
			return nil, errs.NotFound("account")
		}
		return append(base, target), nil
	}
	base = append(base, workflow.ScopeRef{Scope: workflow.ScopeAccount, ID: accountID})

	project := ""
	workspace := ""
	switch target.Scope {
	case workflow.ScopeWorkspace:
		workspace = target.ID
	case workflow.ScopeProject:
		project = target.ID
		workspace = t.workspaceDe[project]
	case workflow.ScopeDemand:
		project = t.projetoDe[target.ID]
		workspace = t.workspaceDe[project]
	}
	if workspace == "" || t.contaDe[workspace] != accountID {
		return nil, errs.NotFound("%s", target.Scope.Label())
	}
	out := append(base, workflow.ScopeRef{Scope: workflow.ScopeWorkspace, ID: workspace})
	if project != "" {
		out = append(out, workflow.ScopeRef{Scope: workflow.ScopeProject, ID: project})
	}
	if target.Scope == workflow.ScopeDemand {
		out = append(out, target)
	}
	return out, nil
}

var _ workflow.Ancestry = (*fakeTree)(nil)

// fakeAccess is identity's narrow port: only the role in the active account.
type fakeAccess struct{ papel map[string]string }

func (a *fakeAccess) RoleOf(_ context.Context, userID, accountID string) (string, error) {
	p, ok := a.papel[userID+"@"+accountID]
	if !ok {
		return "", errs.Permission("no membership in this account")
	}
	return p, nil
}

var _ workflow.Access = (*fakeAccess)(nil)

// ── scenario ─────────────────────────────────────────────────────────────────

const (
	account   = "acct-1"
	workspace = "ws-1"
	project   = "prj-1"
	demand    = "dmd-1"
	dono      = "usr-owner"
	member    = "usr-dev"
)

func scenario(t *testing.T) (*fakeRepo, *workflow.Service, context.Context) {
	t.Helper()
	repo := newFakeRepo()
	tree := &fakeTree{
		workspaceDe: map[string]string{project: workspace},
		contaDe:     map[string]string{workspace: account, "ws-vizinho": account, "ws-alheio": "acct-2"},
		projetoDe:   map[string]string{demand: project},
	}
	acc := &fakeAccess{papel: map[string]string{
		dono + "@" + account:   workflow.RoleOwner,
		member + "@" + account: "developer",
	}}
	svc := workflow.NewService(repo, tree, acc, &fixedClock{t: time.Unix(1_700_000_000, 0).UTC()}, nil)
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: account, ActorID: dono, ActorKind: ctxutil.ActorUser,
	})
	return repo, svc, ctx
}

// seed writes a flow that already exists at a level of the chain straight into the double.
func seed(repo *fakeRepo, accountID string, scope workflow.Scope, ownerID string, stages ...workflow.StageSpec) workflow.Flow {
	f := workflow.Flow{
		AccountID: accountID, OwnerScope: scope, OwnerID: ownerID,
		Name: string(scope) + " flow", Version: 1, Stages: stages,
	}
	f.ID = repo.id()
	repo.flows[f.ID] = &registro{meta: f, versions: map[int32]workflow.Flow{1: f}, current: 1}
	return f
}

func stage(key string, kind workflow.StageType, artifacts ...workflow.ArtifactKind) workflow.StageSpec {
	return workflow.StageSpec{
		Key: key, Name: key, Type: kind, Artifacts: artifacts, Gate: workflow.GateNone,
	}
}

func validFlow() workflow.Flow {
	return workflow.Flow{
		Name:       "Team default",
		OwnerScope: workflow.ScopeProject,
		OwnerID:    project,
		Stages: []workflow.StageSpec{
			stage("context", workflow.TypeContext, workflow.ArtifactDocument),
			stage("spec", workflow.TypeSpec, workflow.ArtifactSpec),
			{Key: "validation", Name: "Validation", Type: workflow.TypeHumanValidation,
				Artifacts: []workflow.ArtifactKind{workflow.ArtifactReport}, Gate: workflow.GateHuman},
		},
	}
}

// ── the inheritance chain ────────────────────────────────────────────────────

// The domain's central test: the whole chain, with overlay and PROVENANCE.
// Without the provenance, nobody can debug why a demand followed a flow nobody
// remembers writing (ADR-0014, consequences).
func TestTheInheritanceChainOverlaysAndSaysWhereEachStageCameFrom(t *testing.T) {
	repo, svc, ctx := scenario(t)

	// Platform: the catalogue, level 0.
	seed(repo, "", workflow.ScopePlatform, "",
		stage("context", workflow.TypeContext, workflow.ArtifactDocument),
		stage("spec", workflow.TypeSpec, workflow.ArtifactSpec),
		stage("implementation", workflow.TypeImplementation))
	// Account: rewrites the spec (same key) — overlays by declaration.
	seed(repo, account, workflow.ScopeAccount, account,
		stage("spec", workflow.TypeSpec, workflow.ArtifactSpec, workflow.ArtifactDiagram))
	// Workspace: declares nothing — inherits by omission.
	// Project: it adds a new stage, which goes at the end.
	seed(repo, account, workflow.ScopeProject, project,
		stage("test", workflow.TypeTest, workflow.ArtifactTestPlan))

	eff, err := svc.Resolve(ctx, workflow.ScopeProject, project)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := []string{"context", "spec", "implementation", "test"}
	if len(eff.Flow.Stages) != len(want) {
		t.Fatalf("esperadas %d stages, vieram %d (%v)", len(want), len(eff.Flow.Stages), eff.Flow.Stages)
	}
	for i, key := range want {
		if eff.Flow.Stages[i].Key != key {
			t.Errorf("stage %d should be %q, got %q", i, key, eff.Flow.Stages[i].Key)
		}
	}

	// The account's spec won over the platform's — and the inherited position was kept.
	if got := len(eff.Flow.Stages[1].Artifacts); got != 2 {
		t.Errorf("the account's spec should have beaten the platform's (%d artifacts)", got)
	}

	// The provenance, stage by stage.
	proc := map[string]workflow.Scope{
		"context":        workflow.ScopePlatform,
		"spec":           workflow.ScopeAccount,
		"implementation": workflow.ScopePlatform,
		"test":           workflow.ScopeProject,
	}
	for key, wantScope := range proc {
		origin, ok := eff.OriginOf(key)
		if !ok {
			t.Fatalf("stage %q has no recorded provenance", key)
		}
		if origin.Scope != wantScope {
			t.Errorf("stage %q should come from %s, came from %s", key, wantScope, origin.Scope)
		}
	}

	// The visible trail, from the most specific to the most generic. The workspace
	// does not appear: it declared nothing, so it did not contribute.
	if !strings.HasPrefix(eff.ResolvedFrom, "project ◂ account ◂ platform") {
		t.Errorf("the trail should start at 'project ◂ account ◂ platform', got %q", eff.ResolvedFrom)
	}
	if strings.Contains(eff.ResolvedFrom, "workspace") {
		t.Errorf("a level that declared nothing must not appear in the trail: %q", eff.ResolvedFrom)
	}
	if !strings.Contains(eff.ResolvedFrom, "spec (account)") {
		t.Errorf("the trail should say where each stage came from: %q", eff.ResolvedFrom)
	}

	// The whole chain costs ONE trip to the repository, not one per level.
	if repo.byOwnersCalls != 1 {
		t.Errorf("resolution should cost ONE query, it cost %d", repo.byOwnersCalls)
	}
}

func TestResolvingADemandCrossesTheWholeChain(t *testing.T) {
	repo, svc, ctx := scenario(t)
	seed(repo, "", workflow.ScopePlatform, "", stage("context", workflow.TypeContext))
	seed(repo, account, workflow.ScopeDemand, demand, stage("hotfix", workflow.TypeGeneric))

	eff, err := svc.Resolve(ctx, workflow.ScopeDemand, demand)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(eff.Flow.Stages) != 2 {
		t.Fatalf("the demand should inherit the platform and add its own stage: %v", eff.Flow.Stages)
	}
	if eff.Contributors[0].Scope != workflow.ScopeDemand {
		t.Errorf("the most specific level should open the trail, got %v", eff.Contributors)
	}
}

func TestResolveWithNoDeclaredLevelDoesNotInventAFlow(t *testing.T) {
	_, svc, ctx := scenario(t)
	if _, err := svc.Resolve(ctx, workflow.ScopeProject, project); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("an empty chain should give NotFound; err: %v", err)
	}
}

func TestResolveDoesNotReachAnotherAccount(t *testing.T) {
	repo, svc, ctx := scenario(t)
	seed(repo, "acct-2", workflow.ScopeWorkspace, "ws-alheio", stage("x", workflow.TypeGeneric))
	if _, err := svc.Resolve(ctx, workflow.ScopeWorkspace, "ws-alheio"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("a workspace of another account should give NotFound; err: %v", err)
	}
}

func TestResolveRequiresAnActiveAccount(t *testing.T) {
	_, svc, _ := scenario(t)
	if _, err := svc.Resolve(context.Background(), workflow.ScopeAccount, ""); errs.KindOf(err) != errs.KindInvalid {
		t.Fatal("a request with no active account is invalid by definition")
	}
}

// ── version freezing ─────────────────────────────────────────────────────────

// The invariant that protects a demand in execution: updating CREATES a new version and
// a anterior fica exatamente como estava.
func TestUpdateCreatesANewVersionWithoutTouchingThePrevious(t *testing.T) {
	_, svc, ctx := scenario(t)
	created, err := svc.Create(ctx, validFlow(), "k1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Version != 1 {
		t.Fatalf("a flow is born at version 1, got %d", created.Version)
	}

	alterado := *created
	alterado.Stages = append([]workflow.StageSpec{}, created.Stages...)
	alterado.Stages[0].Name = "Contexto revisado"
	updated, err := svc.Update(ctx, alterado)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("Update should have created version 2, got %d", updated.Version)
	}

	// What the demand froze stays exactly as it was.
	frozen, err := svc.GetVersion(ctx, created.ID, 1)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if frozen.Stages[0].Name != created.Stages[0].Name {
		t.Errorf("version 1 was rewritten: %q became %q",
			created.Stages[0].Name, frozen.Stages[0].Name)
	}
	if frozen.Version != 1 {
		t.Errorf("GetVersion(1) returned version %d", frozen.Version)
	}

	// And the current version is the new one.
	atual, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if atual.Version != 2 || atual.Stages[0].Name != "Contexto revisado" {
		t.Errorf("the current version should be the revised 2, got v%d %q",
			atual.Version, atual.Stages[0].Name)
	}
}

func TestUpdateOverAStaleVersionIsAConflict(t *testing.T) {
	_, svc, ctx := scenario(t)
	created, _ := svc.Create(ctx, validFlow(), "k1")

	primeira := *created
	primeira.Stages = append([]workflow.StageSpec{}, created.Stages...)
	primeira.Stages[0].Name = "A"
	if _, err := svc.Update(ctx, primeira); err != nil {
		t.Fatalf("first edit: %v", err)
	}

	// The second person still had version 1 open on screen.
	segunda := *created
	segunda.Stages = append([]workflow.StageSpec{}, created.Stages...)
	segunda.Stages[0].Name = "B"
	if _, err := svc.Update(ctx, segunda); errs.KindOf(err) != errs.KindConflict {
		t.Fatalf("an edit over a stale version should be a Conflict; error: %v", err)
	}
}

// An identical resend does not version: a client with automatic retries would
// version the flow forever, and the demand would point at versions nobody wrote.
func TestAnIdenticalResendCreatesNoVersion(t *testing.T) {
	_, svc, ctx := scenario(t)
	created, _ := svc.Create(ctx, validFlow(), "k1")

	igual := *created
	fresh, err := svc.Update(ctx, igual)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if fresh.Version != 1 {
		t.Errorf("an identical resend must not version; it went to version %d", fresh.Version)
	}
}

func TestRepeatedCreateWithTheSameKeyDoesNotDuplicate(t *testing.T) {
	repo, svc, ctx := scenario(t)
	first, err := svc.Create(ctx, validFlow(), "k1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := svc.Create(ctx, validFlow(), "k1")
	if err != nil {
		t.Fatalf("a repeat should return the same flow: %v", err)
	}
	if first.ID != second.ID || len(repo.flows) != 1 {
		t.Errorf("the idempotency key did not prevent the twin flow: %d flows", len(repo.flows))
	}
}

func TestCreateWithNoKeyDerivesOneFromTheContent(t *testing.T) {
	repo, svc, ctx := scenario(t)
	if _, err := svc.Create(ctx, validFlow(), ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Create(ctx, validFlow(), ""); err != nil {
		t.Fatalf("the same content should collide on the derived key: %v", err)
	}
	if len(repo.flows) != 1 {
		t.Errorf("a write with no client key still has to be idempotent: %d flows", len(repo.flows))
	}
}

// ── recusas de ValidateFlow ──────────────────────────────────────────────────

func TestValidateRefusesEveryDefectThatWouldBreakADemand(t *testing.T) {
	cases := []struct {
		name    string
		flow    workflow.Flow
		snippet string // the piece of the message that locates the problem
	}{
		{
			name: "orphan stage: no key, no event can point at it",
			flow: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				{Key: "", Name: "no key", Type: workflow.TypeGeneric, Gate: workflow.GateNone},
			}},
			snippet: "orphaned",
		},
		{
			name: "cycle: the same key twice",
			flow: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				stage("spec", workflow.TypeSpec),
				stage("spec", workflow.TypeSpec),
			}},
			snippet: "that is a cycle",
		},
		{
			name: "gate with no decider: a human validation that does not interrupt",
			flow: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				{Key: "val", Name: "val", Type: workflow.TypeHumanValidation, Gate: workflow.GateNone,
					Artifacts: []workflow.ArtifactKind{workflow.ArtifactReport}},
			}},
			snippet: "has no gate",
		},
		{
			name: "gate with no decider: a human gate with nothing to decide about",
			flow: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				{Key: "gate", Name: "gate", Type: workflow.TypeGeneric, Gate: workflow.GateHuman},
			}},
			snippet: "nothing to decide about",
		},
		{
			name: "unknown stage type",
			flow: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				{Key: "x", Name: "x", Type: workflow.StageType("mystic-review"), Gate: workflow.GateNone},
			}},
			snippet: "unknown type",
		},
		{
			name:    "flow with no stages at all",
			flow:    workflow.Flow{Name: "f"},
			snippet: "no stages at all",
		},
		{
			name: "flow with no name",
			flow: workflow.Flow{Stages: []workflow.StageSpec{
				stage("spec", workflow.TypeSpec),
			}},
			snippet: "no name",
		},
		{
			name: "unknown artifact",
			flow: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				stage("x", workflow.TypeGeneric, workflow.ArtifactKind("scroll")),
			}},
			snippet: "unknown artifact",
		},
		{
			name: "a key with a space survives neither an event nor a subject",
			flow: workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
				stage("valida tion", workflow.TypeGeneric),
			}},
			snippet: "space",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.flow
			f.Normalize()
			rep := workflow.Validate(f)
			if rep.Valid() {
				t.Fatalf("it should be refused; report: %+v", rep)
			}
			if !containsSnippet(rep.Errors, c.snippet) {
				t.Errorf("the message has to say WHICH stage and WHY; got %v", rep.Errors)
			}
			// And the refusal belongs to the CLIENT, not an internal failure.
			if errs.KindOf(rep.Err()) != errs.KindInvalid {
				t.Errorf("a validation error belongs to the client: %v", rep.Err())
			}
		})
	}
}

func TestValidateWarnsWithoutRefusing(t *testing.T) {
	f := workflow.Flow{Name: "f", Stages: []workflow.StageSpec{
		{Key: "impl", Name: "impl", Type: workflow.TypeImplementation, Gate: workflow.GateNone,
			Subtypes: []string{"aaa"}},
	}}
	f.Normalize()
	rep := workflow.Validate(f)
	if !rep.Valid() {
		t.Fatalf("nothing here prevents execution: %v", rep.Errors)
	}
	if len(rep.Warnings) < 2 {
		t.Errorf("warnings are missing (no spec, no gate, substages outside a test): %v", rep.Warnings)
	}
}

// In Create and Update the refusal becomes an error: there an invalid flow is
// something somebody is writing, and storing it would leave a future demand
// with no answer.
func TestCreateRefusesAnInvalidFlow(t *testing.T) {
	repo, svc, ctx := scenario(t)
	f := validFlow()
	f.Stages = append(f.Stages, stage("spec", workflow.TypeSpec)) // key repetida
	if _, err := svc.Create(ctx, f, "k1"); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("a flow with a cycle should give Invalid; err: %v", err)
	}
	if len(repo.flows) != 0 {
		t.Error("nothing may have been written")
	}
}

func TestValidateWritesNothing(t *testing.T) {
	repo, svc, ctx := scenario(t)
	rep, err := svc.Validate(ctx, validFlow())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !rep.Valid() {
		t.Errorf("the scenario's flow is valid: %v", rep.Errors)
	}
	if len(repo.flows) != 0 {
		t.Error("Validate is a dry run: it must not write")
	}
}

// ── level and promotion ──────────────────────────────────────────────────────

func TestCreateRefusesAnotherAccountsLevel(t *testing.T) {
	_, svc, ctx := scenario(t)
	f := validFlow()
	f.OwnerScope, f.OwnerID = workflow.ScopeWorkspace, "ws-alheio"
	if _, err := svc.Create(ctx, f, "k1"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("another account's level does not exist from here; error: %v", err)
	}
}

func TestCreatingInThePlatformCatalogueIsRefused(t *testing.T) {
	_, svc, ctx := scenario(t)
	f := validFlow()
	f.OwnerScope, f.OwnerID = workflow.ScopePlatform, ""
	if _, err := svc.Create(ctx, f, "k1"); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("level 0 applies to every account; error: %v", err)
	}
}

func TestPromoteClimbsOneLevelAndPublishesWithoutMovingTheSource(t *testing.T) {
	_, svc, ctx := scenario(t)
	created, _ := svc.Create(ctx, validFlow(), "k1")

	promoted, err := svc.Promote(ctx, created.ID, workflow.ScopeWorkspace, workspace)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted.OwnerScope != workflow.ScopeWorkspace || promoted.OwnerID != workspace {
		t.Errorf("the flow should have been published in the workspace, got %s/%s",
			promoted.OwnerScope, promoted.OwnerID)
	}
	if promoted.ID == created.ID {
		t.Error("promoting PUBLISHES, it does not move: the source stays where it was")
	}
	if origin, err := svc.Get(ctx, created.ID); err != nil || origin.OwnerScope != workflow.ScopeProject {
		t.Errorf("the source flow vanished from the project: %v %v", origin, err)
	}
}

func TestPromotingToALevelBelowOrEqualIsRefused(t *testing.T) {
	_, svc, ctx := scenario(t)
	created, _ := svc.Create(ctx, validFlow(), "k1")
	if _, err := svc.Promote(ctx, created.ID, workflow.ScopeDemand, demand); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("does promotion descend the chain? error: %v", err)
	}
	if _, err := svc.Promote(ctx, created.ID, workflow.ScopeProject, project); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("promoting to the same level should be refused; error: %v", err)
	}
}

// Inside an organization account a lower-level flow is public WITHIN the account —
// nunca fora dela (ADR-0014 §6 e §7).
func TestPromotingToThePlatformCatalogueIsRefused(t *testing.T) {
	_, svc, ctx := scenario(t)
	created, _ := svc.Create(ctx, validFlow(), "k1")
	if _, err := svc.Promote(ctx, created.ID, workflow.ScopePlatform, ""); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("the catalogue does not receive an account flow; error: %v", err)
	}
}

func TestPromoteRequiresManage(t *testing.T) {
	_, svc, ctx := scenario(t)
	created, _ := svc.Create(ctx, validFlow(), "k1")

	// The same request, made by somebody with no manage over the account's content.
	ctxDev := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: account, ActorID: member, ActorKind: ctxutil.ActorUser,
	})
	if _, err := svc.Promote(ctxDev, created.ID, workflow.ScopeWorkspace, workspace); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("promoting changes the flow of people who asked for nothing; error: %v", err)
	}
}

func TestPromotingToANeighbouringBranchIsRefused(t *testing.T) {
	_, svc, ctx := scenario(t)
	created, _ := svc.Create(ctx, validFlow(), "k1")

	// ws-neighbour belongs to the SAME account — and is still outside this flow's
	// lineage. Promoting is climbing your OWN chain, not landing on a branch next
	// door that happens to belong to the same tenant.
	if _, err := svc.Promote(ctx, created.ID, workflow.ScopeWorkspace, "ws-vizinho"); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("promoting to a neighbouring branch should be Invalid; error: %v", err)
	}
}

func TestPromoteVersionsTheFlowThatAlreadyExistsAtTheTarget(t *testing.T) {
	_, svc, ctx := scenario(t)
	base := validFlow()
	base.OwnerScope, base.OwnerID = workflow.ScopeWorkspace, workspace
	doWorkspace, err := svc.Create(ctx, base, "k0")
	if err != nil {
		t.Fatalf("Create no workspace: %v", err)
	}

	doProjeto, err := svc.Create(ctx, validFlow(), "k1")
	if err != nil {
		t.Fatalf("Create no project: %v", err)
	}

	promoted, err := svc.Promote(ctx, doProjeto.ID, workflow.ScopeWorkspace, workspace)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted.ID != doWorkspace.ID {
		t.Errorf("promotion should version the target flow, not create a second one")
	}
	if promoted.Version != 2 {
		t.Errorf("the target should go to version 2, got %d", promoted.Version)
	}
	// And the target's version 1 stays readable, for whoever froze it.
	if v1, err := svc.GetVersion(ctx, doWorkspace.ID, 1); err != nil || v1.Version != 1 {
		t.Errorf("the target's version 1 was lost: %v %v", v1, err)
	}
}

// ── required ports ───────────────────────────────────────────────────────────

// A nil clock would switch the port off without anyone noticing and would hand
// the test back the wall-clock dependency the port exists to remove.
func TestNewServiceRefusesANilClock(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("the constructor should refuse a nil clock")
		}
	}()
	workflow.NewService(newFakeRepo(), &fakeTree{}, &fakeAccess{}, nil, nil)
}

func containsSnippet(msgs []string, snippet string) bool {
	for _, m := range msgs {
		if strings.Contains(m, snippet) {
			return true
		}
	}
	return false
}
