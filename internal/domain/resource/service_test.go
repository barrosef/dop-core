package resource_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/domain/resource"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT a database and WITHOUT a real vault:
// repository and SecretStore are ports, and in-memory doubles go in here. It is
// the practical return on hexagonal architecture.
//
// The vault used in these tests is a local double, not the adapter
// internal/adapter/secretstore: the architecture test sweeps EVERY .go under
// internal/domain, the _test.go files included, and importing an adapter from
// here would break the very boundary it protects. The double satisfies
// ports.SecretStore — the same port, the same contract.

// ── natureza do recurso ──────────────────────────────────────────────────────

func TestResourceNature(t *testing.T) {
	if !resource.KindIntegration.HasCredential() {
		t.Error("an integration is the only kind with a credential")
	}
	for _, k := range []resource.Kind{resource.KindSkill, resource.KindWorkflow, resource.KindGitFlow} {
		if k.HasCredential() {
			t.Errorf("%s should not have a credential", k)
		}
		if !k.IsContent() {
			t.Errorf("%s is a content resource", k)
		}
	}
	if resource.ValidKind("database") {
		t.Error("a kind outside the vocabulary should be refused")
	}
	// Content is versioned; a credential is not.
	if !(resource.Resource{Kind: resource.KindSkill}).IsVersioned() {
		t.Error("a skill should be versioned")
	}
	if (resource.Resource{Kind: resource.KindIntegration}).IsVersioned() {
		t.Error("an integration is not versioned — config is current state, not history")
	}
}

func TestManageIncludesUse(t *testing.T) {
	if !resource.LevelManage.AtLeast(resource.LevelUse) {
		t.Error("whoever manages also uses")
	}
	if resource.LevelUse.AtLeast(resource.LevelManage) {
		t.Error("use does NOT grant manage")
	}
	if resource.LevelNone.AtLeast(resource.LevelUse) {
		t.Error("the absence of a grant does not give use")
	}
	if resource.ValidLevel("admin") {
		t.Error("a level outside use|manage should be refused")
	}
}

// TestAccessDefaultByNature is this domain's central test: a credential is
// risk, knowledge is knowledge (ADR-0014 §6).
func TestAccessDefaultByNature(t *testing.T) {
	integracao := resource.Resource{ID: "r1", Kind: resource.KindIntegration}
	skill := resource.Resource{ID: "r2", Kind: resource.KindSkill}
	org := identity.AccountOrganization

	// Recurso COM credencial nasce FECHADO.
	if lvl := resource.EffectiveLevel(integracao, identity.RoleDeveloper, org, nil); lvl != resource.LevelNone {
		t.Errorf("an integration with no grant should be inaccessible, got %q", lvl)
	}
	// A CONTENT resource is born OPEN inside an organization account.
	if lvl := resource.EffectiveLevel(skill, identity.RoleDeveloper, org, nil); lvl != resource.LevelUse {
		t.Errorf("a skill should be usable by a member of the organization, got %q", lvl)
	}
	// ...but open means USE, not MANAGE: changing the team's knowledge requires
	// an explicit grant.
	if resource.EffectiveLevel(skill, identity.RoleDeveloper, org, nil).AtLeast(resource.LevelManage) {
		t.Error("the open default must not grant manage")
	}
	// Owner and admin manage EVERY resource — without that nobody fixes a broken
	// integration.
	for _, role := range []identity.Role{identity.RoleOwner, identity.RoleAdmin} {
		if lvl := resource.EffectiveLevel(integracao, role, org, nil); lvl != resource.LevelManage {
			t.Errorf("%s should have implicit manage, got %q", role, lvl)
		}
	}
	// An explicit grant unlocks the closed integration.
	g := &resource.Grant{ResourceID: "r1", UserID: "u2", Level: resource.LevelUse}
	if lvl := resource.EffectiveLevel(integracao, identity.RoleDeveloper, org, g); lvl != resource.LevelUse {
		t.Errorf("an explicit grant should hold, got %q", lvl)
	}
	// And it also lifts open content: a viewer with explicit manage manages.
	gm := &resource.Grant{ResourceID: "r2", UserID: "u3", Level: resource.LevelManage}
	if lvl := resource.EffectiveLevel(skill, identity.RoleViewer, org, gm); lvl != resource.LevelManage {
		t.Errorf("an explicit manage grant should hold for a viewer, got %q", lvl)
	}
}

func TestIntegrationNeedsCategoryAndProvider(t *testing.T) {
	if _, err := resource.ParseIntegration(map[string]any{"provider": "github"}); err == nil {
		t.Error("an integration with no category should be refused")
	}
	if _, err := resource.ParseIntegration(map[string]any{"category": "banco", "provider": "x"}); err == nil {
		t.Error("a category outside git|task_manager|agent should be refused")
	}
	if _, err := resource.ParseIntegration(map[string]any{"category": "git"}); err == nil {
		t.Error("an integration with no provider should be refused")
	}
	for _, c := range []string{"git", "task_manager", "agent"} {
		if _, err := resource.ParseIntegration(map[string]any{"category": c, "provider": "p"}); err != nil {
			t.Errorf("category %q should be accepted: %v", c, err)
		}
	}
}

// ── in-memory doubles ────────────────────────────────────────────────────────

// fakeVault is the tests' SecretStore: the port's four operations and nothing
// more. It mirrors the in-memory adapter in internal/adapter/secretstore, with
// the guarantees that matter here — read-after-write, a Get of a reference
// inexistente devolvendo (nil, nil), Delete idempotente, Put substituindo, e
// isolamento por account (a chave inclui AccountID).
type fakeVault struct {
	mu   sync.RWMutex
	data map[string]ports.SecretValue
}

func newVault() *fakeVault { return &fakeVault{data: map[string]ports.SecretValue{}} }

func chave(r ports.SecretRef) string { return r.AccountID + "/" + r.Kind + "/" + r.OwnerID }

func (c *fakeVault) Put(_ context.Context, ref ports.SecretRef, v ports.SecretValue) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make(ports.SecretValue, len(v))
	copy(cp, v)
	c.data[chave(ref)] = cp
	return nil
}

func (c *fakeVault) Get(_ context.Context, ref ports.SecretRef) (ports.SecretValue, error) {
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

func (c *fakeVault) Delete(_ context.Context, ref ports.SecretRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, chave(ref))
	return nil
}

func (c *fakeVault) Exists(ctx context.Context, ref ports.SecretRef) (bool, error) {
	v, err := c.Get(ctx, ref)
	return v != nil, err
}

var _ ports.SecretStore = (*fakeVault)(nil)

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
			return nil, errs.New(errs.KindAlreadyExists, "resource already exists")
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
	for _, ex := range f.grants { // upsert by (resource, user)
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
		return errs.NotFound("grant")
	}
	if r, ok := f.res[g.ResourceID]; !ok || r.AccountID != accountID {
		return errs.NotFound("grant")
	}
	delete(f.grants, grantID)
	return nil
}

// RevokeGrantsOfUser is ALL-OR-NOTHING here too, and deliberately so: the fake
// that preceded it deleted one by one, so a test could pass while the real
// adapter left half the grants behind. It collects first and mutates last.
func (f *fakeRepo) RevokeGrantsOfUser(_ context.Context, accountID, userID string) error {
	var doomed []string
	for id, g := range f.grants {
		if g.UserID != userID {
			continue
		}
		r, ok := f.res[g.ResourceID]
		if !ok || r.AccountID != accountID {
			continue
		}
		doomed = append(doomed, id)
	}
	for _, id := range doomed {
		delete(f.grants, id)
	}
	return nil
}

var _ resource.Repository = (*fakeRepo)(nil)

// fakeAccess is identity's narrow port: the role in the account and the nature
// of the
// account, nothing else.
type fakeAccess struct {
	account identity.Account
	members map[string]identity.Role
}

func (f *fakeAccess) Authorize(_ context.Context, userID, accountID string) (*identity.Membership, error) {
	if accountID != f.account.ID {
		return nil, errs.Permission("no membership in this account")
	}
	role, ok := f.members[userID]
	if !ok {
		return nil, errs.Permission("no membership in this account")
	}
	return &identity.Membership{UserID: userID, AccountID: accountID, Role: role}, nil
}

func (f *fakeAccess) GetAccount(_ context.Context, id string) (*identity.Account, error) {
	if id != f.account.ID {
		return nil, errs.NotFound("account")
	}
	a := f.account
	return &a, nil
}

var _ resource.Access = (*fakeAccess)(nil)

// ── standard scenario: an organization with owner, developer and viewer ──────

const acctID = "acct-1"

func scenario() (*resource.Service, *fakeRepo, *fakeAccess, *fakeVault) {
	repo := newFakeRepo()
	acc := &fakeAccess{
		account: identity.Account{ID: acctID, Kind: identity.AccountOrganization, Handle: "acme"},
		members: map[string]identity.Role{
			"owner": identity.RoleOwner,
			"dev":   identity.RoleDeveloper,
			"dev2":  identity.RoleDeveloper,
			"view":  identity.RoleViewer,
		},
	}
	vault := newVault()
	return resource.NewService(repo, acc, vault), repo, acc, vault
}

func asActor(user string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: acctID, ActorID: user, ActorKind: ctxutil.ActorUser,
	})
}

func integrationConfig() map[string]any {
	return map[string]any{"category": "git", "provider": "github"}
}

// ── service tests ────────────────────────────────────────────────────────────

func TestOperationWithoutAnActiveAccountIsRefused(t *testing.T) {
	svc, _, _, _ := scenario()
	// No AccountID: the SP-0 rule — a request with no active account is invalid.
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "owner"})
	if _, err := svc.List(ctx, ""); err == nil {
		t.Error("an operation with no active account should be refused")
	}
}

func TestCreateValidatesKindNameAndCategory(t *testing.T) {
	svc, _, _, _ := scenario()
	ctx := asActor("owner")

	if _, err := svc.Create(ctx, "database", "x", nil); err == nil {
		t.Error("an unknown kind should be refused")
	}
	if _, err := svc.Create(ctx, resource.KindSkill, "  ", nil); err == nil {
		t.Error("an empty name should be refused")
	}
	if _, err := svc.Create(ctx, resource.KindIntegration, "gh", map[string]any{"provider": "github"}); err == nil {
		t.Error("an integration with no category should be refused")
	}
	if _, err := svc.Create(ctx, resource.KindIntegration, "gh", integrationConfig()); err != nil {
		t.Errorf("a well-formed integration should be accepted: %v", err)
	}
	// Viewer is a read-only role.
	if _, err := svc.Create(asActor("view"), resource.KindSkill, "review", nil); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("a viewer should not create a resource; error: %v", err)
	}
}

// Without explicit manage for the creator, whoever connects the integration
// loses access to
// ela no instante seguinte — o default fechado viraria armadilha.
func TestIntegrationCreatorReceivesManage(t *testing.T) {
	svc, _, _, _ := scenario()
	ctx := asActor("dev")

	r, err := svc.Create(ctx, resource.KindIntegration, "github", integrationConfig())
	if err != nil {
		t.Fatalf("creation: %v", err)
	}
	if _, err := svc.Update(ctx, r.ID, integrationConfig()); err != nil {
		t.Errorf("the creator should manage their own integration: %v", err)
	}
	// And the other developer stays out: closed is closed.
	if _, err := svc.Get(asActor("dev2"), r.ID); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("somebody else\u2019s integration should not be reachable; error: %v", err)
	}
}

func TestListFiltersByWhatTheActorMayUse(t *testing.T) {
	svc, _, _, _ := scenario()
	owner := asActor("owner")

	if _, err := svc.Create(owner, resource.KindIntegration, "github", integrationConfig()); err != nil {
		t.Fatal(err)
	}
	skill, err := svc.Create(owner, resource.KindSkill, "revisar-pr", nil)
	if err != nil {
		t.Fatal(err)
	}

	// The owner sees everything — implicit manage.
	all, err := svc.List(owner, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("the owner should see both resources, got %d (%v)", len(all), err)
	}

	// The developer sees the skill (content is open in an organization) and does
	// NOT see the integration the owner created (a credential is born closed).
	visible, err := svc.List(asActor("dev2"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ID != skill.ID {
		t.Fatalf("the developer should see only the skill, got %+v", visible)
	}
}

func TestUpdateVersionsContentButNotIntegrations(t *testing.T) {
	svc, _, _, _ := scenario()
	ctx := asActor("owner")

	skill, _ := svc.Create(ctx, resource.KindSkill, "revisar-pr", map[string]any{"body": "v1"})
	updated, err := svc.Update(ctx, skill.ID, map[string]any{"body": "v2"})
	if err != nil {
		t.Fatalf("updating the skill: %v", err)
	}
	if updated.Version != skill.Version+1 {
		t.Errorf("content should be versioned: %d -> %d", skill.Version, updated.Version)
	}

	integ, _ := svc.Create(ctx, resource.KindIntegration, "github", integrationConfig())
	cfg := integrationConfig()
	cfg["base_url"] = "https://git.acme.dev"
	after, err := svc.Update(ctx, integ.ID, cfg)
	if err != nil {
		t.Fatalf("updating the integration: %v", err)
	}
	if after.Version != integ.Version {
		t.Errorf("an integration is not versioned: %d -> %d", integ.Version, after.Version)
	}
}

// The developer only has USE over the open skill — changing it requires manage.
func TestUpdatingContentRequiresManage(t *testing.T) {
	svc, _, _, _ := scenario()
	skill, _ := svc.Create(asActor("owner"), resource.KindSkill, "revisar-pr", nil)

	if _, err := svc.Get(asActor("dev"), skill.ID); err != nil {
		t.Errorf("a developer should be able to READ the open skill: %v", err)
	}
	if _, err := svc.Update(asActor("dev"), skill.ID, map[string]any{"body": "x"}); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("a developer without manage should not change the skill; error: %v", err)
	}
}

func TestGrantRequiresManageAndAccountMembership(t *testing.T) {
	svc, _, _, _ := scenario()
	integ, _ := svc.Create(asActor("owner"), resource.KindIntegration, "github", integrationConfig())

	// Whoever does not manage the resource does not grant.
	if _, err := svc.Grant(asActor("dev"), integ.ID, "dev2", resource.LevelUse); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("a developer without manage should not grant; error: %v", err)
	}
	// A level outside the vocabulary.
	if _, err := svc.Grant(asActor("owner"), integ.ID, "dev", "admin"); err == nil {
		t.Error("an invalid grant level should be refused")
	}
	// A grant is not a way into the account.
	if _, err := svc.Grant(asActor("owner"), integ.ID, "estranho", resource.LevelUse); err == nil ||
		errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("a non-member should not receive a grant; error: %v", err)
	}

	g, err := svc.Grant(asActor("owner"), integ.ID, "dev", resource.LevelUse)
	if err != nil {
		t.Fatalf("the owner should grant: %v", err)
	}
	if _, err := svc.Get(asActor("dev"), integ.ID); err != nil {
		t.Errorf("the grant should unlock access: %v", err)
	}

	// Revogar fecha de newValue.
	if err := svc.RevokeGrant(asActor("owner"), g.ID); err != nil {
		t.Fatalf("revocation: %v", err)
	}
	if _, err := svc.Get(asActor("dev"), integ.ID); err == nil {
		t.Error("after revocation access should close")
	}
	// ...but never for the owner: their manage does not go through this table.
	if _, err := svc.Get(asActor("owner"), integ.ID); err != nil {
		t.Errorf("an owner does not lose access to a revocation: %v", err)
	}
}

// TestSetCredential is the other central test: the secret goes to the
// SecretStore and
// a row guarda APENAS o ponteiro opaco.
func TestSetCredentialKeepsTheSecretOutOfTheDatabase(t *testing.T) {
	svc, repo, _, vault := scenario()
	ctx := asActor("owner")
	integ, _ := svc.Create(ctx, resource.KindIntegration, "github", integrationConfig())

	secret := []byte("ghp_super_secret_token")
	ref, err := svc.SetCredential(ctx, integ.ID, secret)
	if err != nil {
		t.Fatalf("SetCredential: %v", err)
	}

	// The reference is opaque: it does not carry the secret.
	if ref == "" || strings.Contains(ref, string(secret)) {
		t.Fatalf("the reference must not contain the secret: %q", ref)
	}
	// The row keeps the pointer, and only the pointer.
	row, _ := repo.ByID(ctx, acctID, integ.ID)
	if row.CredentialRef != ref {
		t.Errorf("the row should keep the reference, got %q", row.CredentialRef)
	}
	for _, v := range row.Config {
		if s, ok := v.(string); ok && strings.Contains(s, string(secret)) {
			t.Fatal("the secret leaked into the resource's config")
		}
	}
	// The value is in the vault, resolved by the LOGICAL reference.
	stored, err := vault.Get(ctx, resource.SecretRefFor(acctID, integ.ID))
	if err != nil {
		t.Fatalf("reading the vault: %v", err)
	}
	if !bytes.Equal(stored, secret) {
		t.Error("the vault should keep exactly the value that was sent")
	}
	// And the value never prints itself — not in a log, not in an error.
	if got := ports.SecretValue(secret).String(); got != "***" {
		t.Errorf("SecretValue must not print itself: %q", got)
	}

	// Rotation: writing again replaces.
	if _, err := svc.SetCredential(ctx, integ.ID, []byte("ghp_novo")); err != nil {
		t.Fatalf("rotation: %v", err)
	}
	newValue, _ := vault.Get(ctx, resource.SecretRefFor(acctID, integ.ID))
	if string(newValue) != "ghp_novo" {
		t.Error("rotation should replace the stored value")
	}
}

func TestSetCredentialOnlyAppliesToIntegrations(t *testing.T) {
	svc, _, _, vault := scenario()
	ctx := asActor("owner")

	skill, _ := svc.Create(ctx, resource.KindSkill, "revisar-pr", nil)
	if _, err := svc.SetCredential(ctx, skill.ID, []byte("x")); err == nil ||
		errs.KindOf(err) != errs.KindPrecondition {
		t.Errorf("a content resource has no credential; error: %v", err)
	}
	if exists, _ := vault.Exists(ctx, resource.SecretRefFor(acctID, skill.ID)); exists {
		t.Error("nothing should have been written to the vault")
	}

	integ, _ := svc.Create(ctx, resource.KindIntegration, "github", integrationConfig())
	if _, err := svc.SetCredential(ctx, integ.ID, nil); err == nil ||
		errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("an empty credential should be refused; err: %v", err)
	}
	// Whoever does not manage the integration does not change its credential.
	if _, err := svc.SetCredential(asActor("dev"), integ.ID, []byte("x")); err == nil ||
		errs.KindOf(err) != errs.KindPermission {
		t.Errorf("a developer without manage should not store a credential; error: %v", err)
	}
}

// The credential does not outlive the resource: vault first, row second.
func TestDeleteRemovesTheCredentialFromTheVault(t *testing.T) {
	svc, _, _, vault := scenario()
	ctx := asActor("owner")

	integ, _ := svc.Create(ctx, resource.KindIntegration, "github", integrationConfig())
	if _, err := svc.SetCredential(ctx, integ.ID, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, integ.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if exists, _ := vault.Exists(ctx, resource.SecretRefFor(acctID, integ.ID)); exists {
		t.Error("the secret must not outlive the deleted resource")
	}
	if _, err := svc.Get(ctx, integ.ID); err == nil || errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("the deleted resource should not be found; error: %v", err)
	}
}

// A personal account has no second member: the resource is never shareable, and
// that falls out of the cardinality, with no special rule.
func TestAPersonalAccountsResourceIsNotShareable(t *testing.T) {
	repo := newFakeRepo()
	acc := &fakeAccess{
		account: identity.Account{ID: acctID, Kind: identity.AccountPersonal, Handle: "ed"},
		members: map[string]identity.Role{"ed": identity.RoleOwner},
	}
	svc := resource.NewService(repo, acc, newVault())
	ctx := asActor("ed")

	skill, err := svc.Create(ctx, resource.KindSkill, "revisar-pr", nil)
	if err != nil {
		t.Fatalf("creation: %v", err)
	}
	if _, err := svc.Grant(ctx, skill.ID, "any-other", resource.LevelUse); err == nil ||
		errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("there is nobody to grant to in a personal account; error: %v", err)
	}
	// And the only member is owner: manages everything, through implicit manage.
	if _, err := svc.Update(ctx, skill.ID, map[string]any{"body": "v2"}); err != nil {
		t.Errorf("a personal account owner manages their own resources: %v", err)
	}
}

func TestTheSweepTakesEveryGrantOfTheMemberAndNobodyElses(t *testing.T) {
	// It is what identity calls when somebody leaves the account. A grant that
	// outlives the membership is access with nothing left to justify it — and a
	// grant of ANOTHER person swept along would be the opposite mistake.
	svc, repo, _, _ := scenario()
	ctx := asActor("owner")

	r1, err := svc.Create(ctx, resource.KindIntegration, "ClickUp",
		map[string]any{"category": "task_manager", "provider": "clickup"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.Create(ctx, resource.KindIntegration, "GitHub",
		map[string]any{"category": "git", "provider": "github"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct{ res, user string }{
		{r1.ID, "dev"}, {r2.ID, "dev"}, {r1.ID, "dev2"},
	} {
		if _, err := svc.Grant(ctx, g.res, g.user, resource.LevelUse); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.RevokeAllOfMember(ctx, acctID, "dev"); err != nil {
		t.Fatal(err)
	}
	left, err := repo.GrantsOfUser(ctx, acctID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d grants survived the sweep", len(left))
	}
	stayed, err := repo.GrantsOfUser(ctx, acctID, "dev2")
	if err != nil {
		t.Fatal(err)
	}
	if len(stayed) != 1 {
		t.Errorf("the sweep took somebody else's grants: %d left", len(stayed))
	}
}
