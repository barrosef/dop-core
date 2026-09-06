package workflow_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

func TestRevocationPolicyVocabularyIsClosed(t *testing.T) {
	for _, p := range []workflow.RevocationPolicy{
		workflow.PolicyProspective, workflow.PolicyDrain, workflow.PolicyTerminate,
	} {
		if !workflow.ValidRevocationPolicy(p) {
			t.Fatalf("%q has to be accepted", p)
		}
	}
	// An unknown value is a contract error, not user data: whoever sends it is
	// speaking a vocabulary this platform does not have.
	for _, p := range []workflow.RevocationPolicy{"", "soft", "hard", "cascade"} {
		if workflow.ValidRevocationPolicy(p) {
			t.Fatalf("%q must not be accepted", p)
		}
	}
	if workflow.DefaultRevocationPolicy != workflow.PolicyProspective {
		t.Fatal("the default has to be prospective: revoking must not reach a copy unless somebody chose that")
	}
}

func TestThePublicationReferenceIsTypeable(t *testing.T) {
	ok := map[string]workflow.PublicationRef{
		"@acme/backend-go":    {Handle: "acme", Slug: "backend-go", Version: 0},
		"@acme/backend-go@v3": {Handle: "acme", Slug: "backend-go", Version: 3},
		"@ed/meu-fluxo@v12":   {Handle: "ed", Slug: "meu-fluxo", Version: 12},
	}
	for raw, want := range ok {
		got, err := workflow.ParseRef(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got != want {
			t.Fatalf("%q parsed as %+v, wanted %+v", raw, got, want)
		}
		if got.String() != raw {
			t.Fatalf("%q formats back as %q", raw, got.String())
		}
	}
	// The refusals name what is wrong: somebody typed this by hand.
	for _, raw := range []string{
		"", "acme/backend-go", "@acme", "@acme/", "@/slug",
		"@acme/backend-go@3", "@acme/backend-go@v0", "@acme/backend-go@vx",
		"@ACME/backend-go", "@acme/Backend Go",
	} {
		if _, err := workflow.ParseRef(raw); err == nil {
			t.Fatalf("%q had to be refused", raw)
		}
	}
	if (workflow.PublicationRef{Version: 0}).Pinned() {
		t.Fatal("version zero means the latest published, not a pin")
	}
}

// ── the sharing double ───────────────────────────────────────────────────────
//
// It lives here, next to fakeRepo/fakeTree/fakeAccess in service_test.go, for
// the same reason those do: the domain is tested with no database, and a
// second file of doubles for the same domain is how two sets of tests start
// disagreeing about what the domain does.

// pinRow's mere PRESENCE in the map means pinned: PinOf's second return value
// comes from the map lookup itself, not from a field that would just repeat it.
type pinRow struct {
	version int32
}

// fakeSharing plays the SharingRepository port. Every operation mirrors the
// isolation migration 0021 enforces with foreign keys and a WHERE clause: an
// account only ever sees its own publications, shares and adoptions — the one
// exception is ResolvePublication, matching the port's own exception.
type fakeSharing struct {
	pubs      map[string]*workflow.Publication
	pubKeys   map[string]string // idempotency key → publication id
	nextPubID int

	shares      map[string]*workflow.Share
	shareKeys   map[string]string // idempotency key → share id
	nextShareID int

	adoptions   map[string]*workflow.Adoption
	nextAdoptID int

	pins map[string]pinRow // "accountID|flowID" → row

	// handles stands in for identity's account-by-handle lookup: the real
	// adapter joins `accounts`, the double is seeded directly by the harness.
	handles map[string]string // handle → accountID

	// Side channels the harness reads to prove what a revocation reached,
	// without the domain having to expose its own storage.
	revokedFlows map[string]bool
	revocations  []workflow.Revocation
}

func newFakeSharing() *fakeSharing {
	return &fakeSharing{
		pubs:    map[string]*workflow.Publication{},
		pubKeys: map[string]string{},

		shares:    map[string]*workflow.Share{},
		shareKeys: map[string]string{},

		adoptions: map[string]*workflow.Adoption{},

		pins:    map[string]pinRow{},
		handles: map[string]string{},

		revokedFlows: map[string]bool{},
	}
}

func (f *fakeSharing) pinKey(accountID, flowID string) string { return accountID + "|" + flowID }

func nextID(prefix string, n *int) string {
	*n++
	return prefix + "-" + strconv.Itoa(*n)
}

func (f *fakeSharing) CreatePublication(_ context.Context, p *workflow.Publication, key string) (*workflow.Publication, error) {
	if key != "" {
		if id, repeat := f.pubKeys[key]; repeat {
			cp := *f.pubs[id]
			return &cp, nil
		}
	}
	// Mirrors the migration's UNIQUE (account_id, slug, version): publishing the
	// same version under the same name twice is a retry, not a second
	// publication — and a retry arrives with the SAME key, handled above.
	for _, existing := range f.pubs {
		if existing.AccountID == p.AccountID && existing.Slug == p.Slug && existing.Version == p.Version {
			return nil, errs.New(errs.KindAlreadyExists, "this version is already published under that name")
		}
	}
	cp := *p
	cp.ID = nextID("pub", &f.nextPubID)
	f.pubs[cp.ID] = &cp
	if key != "" {
		f.pubKeys[key] = cp.ID
	}
	out := cp
	return &out, nil
}

func (f *fakeSharing) PublicationByID(_ context.Context, accountID, id string) (*workflow.Publication, error) {
	p, ok := f.pubs[id]
	if !ok || p.AccountID != accountID {
		return nil, errs.NotFound("publication")
	}
	cp := *p
	return &cp, nil
}

func (f *fakeSharing) Withdraw(_ context.Context, accountID, id string, at time.Time) error {
	p, ok := f.pubs[id]
	if !ok || p.AccountID != accountID {
		return errs.NotFound("publication")
	}
	if p.WithdrawnAt.IsZero() {
		p.WithdrawnAt = at
	}
	return nil // withdrawing what is already withdrawn is success
}

func (f *fakeSharing) ResolvePublication(_ context.Context, callerAccountID string, ref workflow.PublicationRef) (*workflow.Publication, error) {
	accountID, ok := f.handles[ref.Handle]
	if !ok {
		return nil, errs.NotFound("no flow published as %s", ref)
	}
	var best *workflow.Publication
	for _, p := range f.pubs {
		if p.AccountID != accountID || p.Slug != ref.Slug || !p.WithdrawnAt.IsZero() {
			continue
		}
		if ref.Pinned() && p.Version != ref.Version {
			continue
		}
		if best == nil || p.Version > best.Version {
			best = p
		}
	}
	if best == nil {
		return nil, errs.NotFound("no flow published as %s", ref)
	}
	granted := false
	for _, s := range f.shares {
		if s.PublicationID == best.ID && s.ToAccountID == callerAccountID && s.RevokedAt.IsZero() {
			granted = true
			break
		}
	}
	if !granted {
		// NEVER Permission here: whether a flow exists in another account is
		// not an outsider's to learn.
		return nil, errs.NotFound("no flow published as %s", ref)
	}
	cp := *best
	return &cp, nil
}

func (f *fakeSharing) CreateShare(_ context.Context, s *workflow.Share, key string) (*workflow.Share, error) {
	if key != "" {
		if id, repeat := f.shareKeys[key]; repeat {
			cp := *f.shares[id]
			return &cp, nil
		}
	}
	for _, existing := range f.shares {
		if existing.PublicationID == s.PublicationID && existing.ToAccountID == s.ToAccountID && existing.RevokedAt.IsZero() {
			return nil, errs.New(errs.KindAlreadyExists, "this account already holds a grant on that publication")
		}
	}
	cp := *s
	cp.ID = nextID("shr", &f.nextShareID)
	f.shares[cp.ID] = &cp
	if key != "" {
		f.shareKeys[key] = cp.ID
	}
	out := cp
	return &out, nil
}

func (f *fakeSharing) ShareByID(_ context.Context, accountID, id string) (*workflow.Share, error) {
	s, ok := f.shares[id]
	if !ok {
		return nil, errs.NotFound("share")
	}
	pub, ok := f.pubs[s.PublicationID]
	if !ok || pub.AccountID != accountID {
		return nil, errs.NotFound("share")
	}
	cp := *s
	return &cp, nil
}

func (f *fakeSharing) SharesOfPublication(_ context.Context, accountID, publicationID string) ([]workflow.Share, error) {
	pub, ok := f.pubs[publicationID]
	if !ok || pub.AccountID != accountID {
		return nil, errs.NotFound("publication")
	}
	var out []workflow.Share
	for _, s := range f.shares {
		if s.PublicationID == publicationID {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeSharing) RevokeShare(_ context.Context, accountID string, rev workflow.Revocation) error {
	s, ok := f.shares[rev.ShareID]
	if !ok {
		return errs.NotFound("share")
	}
	pub, ok := f.pubs[s.PublicationID]
	if !ok || pub.AccountID != accountID {
		return errs.NotFound("share")
	}
	if s.RevokedAt.IsZero() {
		s.RevokedAt = rev.At
	}
	// Marking, never deleting: the adopter's own edits and the audit of a
	// demand that already ran under the copy have to survive.
	for _, ref := range rev.Adoptions {
		if a, ok := f.adoptions[ref.ID]; ok && a.RevokedAt.IsZero() {
			a.RevokedAt = rev.At
		}
		f.revokedFlows[ref.FlowID] = true
	}
	f.revocations = append(f.revocations, rev)
	return nil
}

func (f *fakeSharing) RecordAdoption(_ context.Context, a *workflow.Adoption) error {
	cp := *a
	cp.ID = nextID("adp", &f.nextAdoptID)
	f.adoptions[cp.ID] = &cp
	*a = cp
	return nil
}

func (f *fakeSharing) AdoptionsOfPublication(_ context.Context, accountID, publicationID string) ([]workflow.Adoption, error) {
	pub, ok := f.pubs[publicationID]
	if !ok || pub.AccountID != accountID {
		return nil, errs.NotFound("publication")
	}
	var out []workflow.Adoption
	for _, a := range f.adoptions {
		if a.PublicationID == publicationID {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeSharing) Pin(_ context.Context, accountID, flowID string, version int32, _ string, _ time.Time) error {
	f.pins[f.pinKey(accountID, flowID)] = pinRow{version: version}
	return nil
}

func (f *fakeSharing) PinOf(_ context.Context, accountID, flowID string) (int32, bool, error) {
	row, ok := f.pins[f.pinKey(accountID, flowID)]
	if !ok {
		return 0, false, nil
	}
	return row.version, true, nil
}

var _ workflow.SharingRepository = (*fakeSharing)(nil)

// fakeAccountDefaults plays the AccountDefaults port by reading the harness's
// env LIVE, at call time — never a value captured when the double was built.
// Grant is supposed to STAMP the default onto the Share and never consult this
// port again; a double that snapshot the value at construction would let a
// broken Grant (one that re-reads the account instead of the stamp) pass the
// stamping test for the wrong reason.
type fakeAccountDefaults struct {
	env *sharingEnv
}

func (f *fakeAccountDefaults) DefaultRevocationPolicy(_ context.Context, _ string) (string, error) {
	return f.env.AccountDefault, nil
}

var _ workflow.AccountDefaults = (*fakeAccountDefaults)(nil)

// ── the harness ──────────────────────────────────────────────────────────────

// sharingEnv is the fixed cast every sharing test plays against: a publisher
// account with an owner and a developer, a second account with its own owner
// and a project, and a third account that holds no grant at all — which is
// what proves ResolvePublication answers NotFound and not Permission.
type sharingEnv struct {
	AccountID, OtherAccountID, ThirdAccountID string
	OwnerID, DeveloperID, OtherOwnerID        string
	FlowID, PlatformFlowID, OtherProjectID    string
	CurrentVersion                            int32
	AccountDefault                            string // what the AccountDefaults port answers
	roles                                     map[string]string
	sharing                                   *fakeSharing
}

func (e *sharingEnv) CtxAs(userID string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: userID, AccountID: e.AccountID, ActorKind: ctxutil.ActorUser,
	})
}

func (e *sharingEnv) CtxAsOther(userID string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: userID, AccountID: e.OtherAccountID, ActorKind: ctxutil.ActorUser,
	})
}

func (e *sharingEnv) CtxAsThird() context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: "u-third", AccountID: e.ThirdAccountID, ActorKind: ctxutil.ActorUser,
	})
}

func (e *sharingEnv) FlowRevoked(id string) bool { return e.sharing.revokedFlows[id] }

const (
	sharingOwner      = "usr-sharing-owner"
	sharingDeveloper  = "usr-sharing-dev"
	sharingOtherOwner = "usr-sharing-other-owner"
	sharingAccount    = "acct-sharing-pub"
	sharingOther      = "acct-sharing-other"
	sharingThird      = "acct-sharing-third"
	sharingWorkspace  = "ws-sharing-other"
	sharingProject    = "prj-sharing-other"
	sharingHandle     = "acme"
)

func newSharingHarness(t *testing.T) (*workflow.Service, *sharingEnv) {
	t.Helper()

	repo := newFakeRepo()
	tree := &fakeTree{
		workspaceDe: map[string]string{sharingProject: sharingWorkspace},
		contaDe:     map[string]string{sharingWorkspace: sharingOther},
		projetoDe:   map[string]string{},
	}
	access := &fakeAccess{papel: map[string]string{
		sharingOwner + "@" + sharingAccount:     workflow.RoleOwner,
		sharingDeveloper + "@" + sharingAccount: "developer",
		sharingOtherOwner + "@" + sharingOther:  workflow.RoleOwner,
	}}
	sharing := newFakeSharing()
	sharing.handles[sharingHandle] = sharingAccount

	clock := &fixedClock{t: time.Unix(1_700_000_000, 0).UTC()}

	// The publisher's flow, taken to version 2 directly through the repository
	// (bypassing Update's rules on purpose, the same way seed() does): Publish
	// has to freeze whatever is CURRENT, and a harness stuck at version 1 could
	// not tell "freezes the current version" apart from "always returns 1".
	flow := seed(repo, sharingAccount, workflow.ScopeAccount, sharingAccount,
		stage("context", workflow.TypeContext, workflow.ArtifactDocument),
		stage("spec", workflow.TypeSpec, workflow.ArtifactSpec))
	bumped := flow
	bumped.Version = 2
	if _, err := repo.AppendVersion(context.Background(), sharingAccount, &bumped, 1, "seed-v2"); err != nil {
		t.Fatalf("seeding the flow's second version: %v", err)
	}

	// The platform catalogue's flow: what later tasks pin an account to when
	// inheritance crosses the ownership boundary.
	platformFlow := seed(repo, "", workflow.ScopePlatform, "",
		stage("context", workflow.TypeContext, workflow.ArtifactDocument))

	env := &sharingEnv{
		AccountID: sharingAccount, OtherAccountID: sharingOther, ThirdAccountID: sharingThird,
		OwnerID: sharingOwner, DeveloperID: sharingDeveloper, OtherOwnerID: sharingOtherOwner,
		FlowID: flow.ID, PlatformFlowID: platformFlow.ID, OtherProjectID: sharingProject,
		CurrentVersion: 2,
		AccountDefault: string(workflow.DefaultRevocationPolicy),
		roles:          access.papel,
		sharing:        sharing,
	}
	// The double reads env.AccountDefault live: built AFTER env so it can hold a
	// pointer to it, not a copy of whatever the field held at this moment.
	defaults := &fakeAccountDefaults{env: env}
	svc := workflow.NewService(repo, tree, access, clock, sharing, defaults)

	return svc, env
}

// ── Publish / Withdraw ───────────────────────────────────────────────────────

func TestPublishingFreezesAVersionAndNeedsManage(t *testing.T) {
	svc, env := newSharingHarness(t)
	// A developer does not publish: publishing exposes the account's work
	// outside it, and that is an act of governance.
	ctx := env.CtxAs(env.DeveloperID)
	if _, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1"); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("a developer must not publish: %v", err)
	}

	ctx = env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "first cut", "k2")
	if err != nil {
		t.Fatal(err)
	}
	if pub.Version != env.CurrentVersion {
		t.Fatalf("publishing has to freeze the CURRENT version, got %d", pub.Version)
	}
	// Repeating with the same key is a retry, not a second publication.
	again, err := svc.Publish(ctx, env.FlowID, "backend-go", "first cut", "k2")
	if err != nil || again.ID != pub.ID {
		t.Fatalf("the same key had to return the same publication: %v", err)
	}
	// A slug outside the reference's alphabet is refused HERE, not when somebody
	// later fails to type the reference.
	if _, err := svc.Publish(ctx, env.FlowID, "Backend Go", "", "k3"); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("an unusable slug had to be refused: %v", err)
	}
}

func TestWithdrawStopsNewDerivationsAndNothingElse(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Withdraw(ctx, pub.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Withdraw(ctx, pub.ID); err != nil {
		t.Fatalf("withdrawing what is already withdrawn is success: %v", err)
	}
}

// The platform catalogue is inherited by the chain, never published for
// adoption — the same refusal Create and Promote already give it
// (TestCreatingInThePlatformCatalogueIsRefused,
// TestPromotingToThePlatformCatalogueIsRefused): a level with no owner of its
// own has nothing an account boundary could cross.
func TestPublishingThePlatformCatalogueIsRefused(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAs(env.OwnerID)
	if _, err := svc.Publish(ctx, env.PlatformFlowID, "backend-go", "", "k1"); errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("the platform's flow is inherited by the chain, not published for adoption; err: %v", err)
	}
}

// ── the double's isolation guarantee ─────────────────────────────────────────

// ResolvePublication is the one deliberate crossing the whole feature rests
// on, and fakeSharing is what every later task's tests trust to enforce it.
// This tests the DOUBLE directly, not a Service method, because nothing in
// this domain calls ResolvePublication yet (Task 6 does) — and because a
// later "fix" that quietly turned one of these branches into Permission would
// otherwise break the isolation guarantee with nothing red.
func TestFakeSharingResolvePublicationAnswersNotFoundNeverPermission(t *testing.T) {
	const (
		publisher = "acct-resolve-pub"
		granted   = "acct-resolve-granted"
		other     = "acct-resolve-other"
	)
	ctx := context.Background()
	sharing := newFakeSharing()
	sharing.handles["acme"] = publisher
	ref := workflow.PublicationRef{Handle: "acme", Slug: "backend-go"}

	// An unknown handle: nothing to resolve against at all.
	if _, err := sharing.ResolvePublication(ctx, granted, workflow.PublicationRef{Handle: "ghost", Slug: "backend-go"}); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("an unknown handle has to be NotFound: %v", err)
	}

	pub, err := sharing.CreatePublication(ctx, &workflow.Publication{
		FlowID: "flw-1", AccountID: publisher, Slug: "backend-go", Version: 1,
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}

	// A known handle, a real publication, and NO grant at all.
	if _, err := sharing.ResolvePublication(ctx, granted, ref); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("no grant has to be NotFound, never Permission: %v", err)
	}

	share, err := sharing.CreateShare(ctx, &workflow.Share{
		PublicationID: pub.ID, ToAccountID: granted, RevocationPolicy: workflow.PolicyProspective,
	}, "g1")
	if err != nil {
		t.Fatal(err)
	}
	// The grant, while it holds, resolves — the positive control for the three
	// refusals around it.
	if _, err := sharing.ResolvePublication(ctx, granted, ref); err != nil {
		t.Fatalf("the granted account had to resolve it: %v", err)
	}

	// A grant that WAS revoked.
	if err := sharing.RevokeShare(ctx, publisher, workflow.Revocation{
		ShareID: share.ID, PublicationID: pub.ID, ToAccountID: granted,
		Policy: workflow.PolicyProspective, At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sharing.ResolvePublication(ctx, granted, ref); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("a revoked grant has to be NotFound: %v", err)
	}

	// A publication that was WITHDRAWN — checked against a fresh, still-valid
	// grant, so withdrawal and revocation are not conflated.
	if _, err := sharing.CreateShare(ctx, &workflow.Share{
		PublicationID: pub.ID, ToAccountID: other, RevocationPolicy: workflow.PolicyProspective,
	}, "g2"); err != nil {
		t.Fatal(err)
	}
	if err := sharing.Withdraw(ctx, publisher, pub.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := sharing.ResolvePublication(ctx, other, ref); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("a withdrawn publication has to be NotFound even for a granted account: %v", err)
	}
}

// ── Grant ────────────────────────────────────────────────────────────────────

func TestTheGrantStampsThePolicyItWasMadeUnder(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}

	env.AccountDefault = "prospective"
	share, err := svc.Grant(ctx, pub.ID, env.OtherAccountID, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if share.RevocationPolicy != workflow.PolicyProspective {
		t.Fatalf("the grant had to carry the default in force, got %q", share.RevocationPolicy)
	}

	// Changing the account's default AFTERWARDS must not change the terms of a
	// grant somebody already accepted.
	env.AccountDefault = "terminate"
	got, err := svc.SharesOf(ctx, pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].RevocationPolicy != workflow.PolicyProspective {
		t.Fatal("the account's default changed the terms of an existing grant: the stamp is not being honoured")
	}
}

// GrantedBy is an AUDIT field: it is stored verbatim from the actor, and a
// grant nobody can be held to is worse than no grant at all. fakeAccess
// happens to reject an empty actor key too (there is no membership row for
// ""), which would mask a missing guard behind a Permission error instead of
// Unauthorized — so this asserts the SPECIFIC kind the guard raises.
func TestGrantRequiresAnIdentifiedActor(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	anon := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: env.AccountID, ActorKind: ctxutil.ActorUser,
	})
	if _, err := svc.Grant(anon, pub.ID, env.OtherAccountID, "g-anon"); errs.KindOf(err) != errs.KindUnauthorized {
		t.Fatalf("an unidentified actor must not grant, and GrantedBy must never be recorded empty: %v", err)
	}
}

func TestGrantRefusesAnEmptyTarget(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(ctx, pub.ID, "   ", "g1"); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("an empty target account has to be refused: %v", err)
	}
}

func TestGrantRefusesGrantingToYourOwnAccount(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	// Padded with whitespace on purpose: the trim has to apply BEFORE the
	// self-grant comparison and stay applied for what gets stored — trimming
	// only the emptiness check and comparing the untrimmed value afterwards
	// would let this one slip through as a "grant" to a mistyped variant of
	// the caller's own account.
	if _, err := svc.Grant(ctx, pub.ID, " "+env.AccountID+" ", "g1"); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("an account already sees its own flows: there is nothing to grant: %v", err)
	}
}

func TestGrantRefusesAWithdrawnPublication(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Withdraw(ctx, pub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(ctx, pub.ID, env.OtherAccountID, "g1"); errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("a withdrawn publication has to be granted again, not still granted from: %v", err)
	}
}
