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

	// repo is the SAME flow storage RecordDerivation writes the copy into —
	// standing in for the real adapter, which writes both the copy and the
	// adoption record through one DB connection in one transaction. Wiring it
	// in is what lets the double actually enforce "one write or neither",
	// instead of merely promising it.
	repo *fakeRepo
	// failDerivationAdoption is a test hook: when set, RecordDerivation's
	// adoption half fails AFTER the copy would have been written, so a test can
	// assert the copy does not survive — no orphaned copy left for a
	// revocation nobody can ever reach.
	failDerivationAdoption bool

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

// RecordDerivation writes the copy and the adoption record as ONE unit,
// mirroring the real adapter's transaction: it creates the flow through the
// same repo the domain would otherwise have called directly, and — if the
// adoption half is made to fail — UNDOES that creation rather than merely
// skipping the adoption write. A double that left the copy standing on a
// simulated failure would let the "no orphaned copy" test pass for the wrong
// reason: nothing would prove the copy was actually rolled back.
func (f *fakeSharing) RecordDerivation(ctx context.Context, accountID string, flow *workflow.Flow, adoption *workflow.Adoption, key string) (*workflow.Flow, error) {
	if f.repo == nil {
		panic("fakeSharing.RecordDerivation: repo not wired — see newSharingHarness")
	}
	out, err := f.repo.Create(ctx, flow, key)
	if err != nil {
		return nil, err
	}
	if f.failDerivationAdoption {
		f.repo.forget(out.ID, key)
		return nil, errs.New(errs.KindUnavailable, "simulated: the adoption half of the derivation's transaction failed")
	}
	cp := *adoption
	cp.ID = nextID("adp", &f.nextAdoptID)
	cp.FlowID = out.ID
	f.adoptions[cp.ID] = &cp
	*adoption = cp
	return out, nil
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

// fakeAccountFacts plays the AccountFacts port by reading the harness's
// env LIVE, at call time — never a value captured when the double was built.
// Grant is supposed to STAMP the default onto the Share and never consult this
// port again; a double that snapshot the value at construction would let a
// broken Grant (one that re-reads the account instead of the stamp) pass the
// stamping test for the wrong reason.
type fakeAccountFacts struct {
	env *sharingEnv
}

func (f *fakeAccountFacts) DefaultRevocationPolicy(_ context.Context, _ string) (string, error) {
	return f.env.AccountDefault, nil
}

// HandleOf reverse-looks-up the fakeSharing.handles map (handle → accountID,
// the same double ResolvePublication reads) — there is no second map to keep
// in sync, and a test that registers a handle for ResolvePublication gets it
// answered here too, for free.
func (f *fakeAccountFacts) HandleOf(_ context.Context, accountID string) (string, error) {
	for handle, id := range f.env.sharing.handles {
		if id == accountID {
			return handle, nil
		}
	}
	return "", errs.NotFound("account %q", accountID)
}

var _ workflow.AccountFacts = (*fakeAccountFacts)(nil)

// ── the harness ──────────────────────────────────────────────────────────────

// sharingEnv is the fixed cast every sharing test plays against: a publisher
// account with an owner and a developer, a second account with its own owner
// and a project, and a third account that holds no grant at all — which is
// what proves ResolvePublication answers NotFound and not Permission.
type sharingEnv struct {
	AccountID, OtherAccountID, ThirdAccountID string
	OwnerID, DeveloperID, OtherOwnerID        string
	FlowID, PlatformFlowID, OtherProjectID    string
	// OtherFlowID is the OTHER account's own account-level flow — absent until
	// AppendAccountVersion first creates it, which is deliberate: a test that
	// wants to observe "inherits the platform's flow because it declares
	// nothing of its own" needs that starting state to be reachable.
	OtherFlowID    string
	CurrentVersion int32
	AccountDefault string // what AccountFacts.DefaultRevocationPolicy answers
	roles          map[string]string
	sharing        *fakeSharing
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

// AppendPlatformVersion publishes a new version of the PLATFORM catalogue's
// flow — the level every account inherits from by default when its own chain
// declares nothing. It is what a test uses to prove an account pinned across
// the ownership boundary does NOT move by itself when the platform changes.
func (e *sharingEnv) AppendPlatformVersion() {
	e.bumpFlow("", e.PlatformFlowID)
}

// AppendAccountVersion moves the OTHER account's OWN account-level flow
// forward. The flow does not exist until the first call — creating it here,
// rather than in newSharingHarness, is deliberate: a test needs the account
// to start with NOTHING declared of its own (so it inherits the platform's
// flow, crossing the ownership boundary) and only later gain a flow it
// governs itself, to prove that inheritance INSIDE one account stays live
// even while a cross-boundary pin is in effect.
func (e *sharingEnv) AppendAccountVersion() {
	if e.OtherFlowID == "" {
		f := seed(e.sharing.repo, e.OtherAccountID, workflow.ScopeAccount, e.OtherAccountID,
			stage("context", workflow.TypeContext, workflow.ArtifactDocument))
		e.OtherFlowID = f.ID
	}
	e.bumpFlow(e.OtherAccountID, e.OtherFlowID)
}

// bumpFlow appends the next version straight through the repository double,
// the same shortcut newSharingHarness already takes to put the publisher's
// flow at version 2 (bypassing Service.Update's rules on purpose): what these
// tests need is a KNOWN version to move to, not another exercise of Update.
func (e *sharingEnv) bumpFlow(accountID, flowID string) {
	repo := e.sharing.repo
	r, ok := repo.flows[flowID]
	if !ok {
		panic("bumpFlow: unknown flow " + flowID)
	}
	next := repo.current(r)
	next.Version = r.current + 1
	key := "bump-" + flowID + "-" + strconv.Itoa(int(next.Version))
	if _, err := repo.AppendVersion(context.Background(), accountID, &next, r.current, key); err != nil {
		panic("bumpFlow: " + err.Error())
	}
}

// LastRevocation returns the most recent Revocation RevokeShare received —
// a real recording of what the domain decided to hand to the port, not a
// boolean nothing sets.
func (e *sharingEnv) LastRevocation() workflow.Revocation {
	n := len(e.sharing.revocations)
	if n == 0 {
		return workflow.Revocation{}
	}
	return e.sharing.revocations[n-1]
}

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
	// RecordDerivation writes the copy through this SAME repo — the double's
	// way of enforcing "one transaction, or neither write happens", the same
	// guarantee the real adapter gives by sharing one DB connection.
	sharing.repo = repo

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
	accounts := &fakeAccountFacts{env: env}
	svc := workflow.NewService(repo, tree, access, clock, sharing, accounts)

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

// ── Derive ───────────────────────────────────────────────────────────────────

func TestDerivingCopiesAndRecordsBothSides(t *testing.T) {
	svc, env := newSharingHarness(t)
	pub, err := svc.Publish(env.CtxAs(env.OwnerID), env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(env.CtxAs(env.OwnerID), pub.ID, env.OtherAccountID, "g1"); err != nil {
		t.Fatal(err)
	}

	// The publisher keeps developing AFTER publishing: a new version, with a
	// DIFFERENT stage set, so the frozen publication and the flow's current
	// state diverge. Without this, `VersionOf` (the frozen version) and `ByID`
	// (the current one) would return byte-identical content, and a regression
	// that swapped one for the other would pass this test unnoticed.
	newStages := []workflow.StageSpec{
		stage("context", workflow.TypeContext, workflow.ArtifactDocument),
		stage("spec", workflow.TypeSpec, workflow.ArtifactSpec),
		stage("test", workflow.TypeTest, workflow.ArtifactTestPlan),
	}
	if _, err := svc.Update(env.CtxAs(env.OwnerID), workflow.Flow{
		ID: env.FlowID, Name: "account flow", Stages: newStages,
	}); err != nil {
		t.Fatal(err)
	}

	// The other account derives it at ITS OWN project level.
	ctx := env.CtxAsOther(env.OtherOwnerID)
	target := workflow.ScopeRef{Scope: workflow.ScopeProject, ID: env.OtherProjectID}
	copied, err := svc.Derive(ctx, "@acme/backend-go", target, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if copied.AccountID != env.OtherAccountID {
		t.Fatal("the copy has to belong to whoever derived it")
	}
	if copied.Origin == nil || copied.Origin.Ref != "@acme/backend-go" || copied.Origin.Version != pub.Version {
		t.Fatalf("the copy does not carry where it came from: %+v", copied.Origin)
	}
	// The content has to be the FROZEN version — two stages — and not what the
	// publisher moved on to since (three, after the Update above).
	if len(copied.Stages) != 2 || copied.Stages[0].Key != "context" || copied.Stages[1].Key != "spec" {
		t.Fatalf("the copy carries the publisher's CURRENT flow, not the version it froze on: %+v", copied.Stages)
	}
	// The publisher's side records where it went, so it never has to scan
	// another account to find out.
	ads, err := svc.AdoptionsOf(env.CtxAs(env.OwnerID), pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ads) != 1 || ads[0].ByAccountID != env.OtherAccountID || ads[0].FlowID != copied.ID || ads[0].Version != pub.Version {
		t.Fatalf("the derivation was not recorded on the publisher's side: %+v", ads)
	}
	// Without a grant, the reference does not even exist for the caller.
	if _, err := svc.Derive(env.CtxAsThird(), "@acme/backend-go", target, "d2"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("an account with no grant has to get not-found, not permission-denied: %v", err)
	}
}

// TestDerivingRollsBackWhenTheAdoptionWriteFails is the fix for the defect the
// review caught: RecordDerivation writes the copy and the adoption record as
// ONE unit precisely because a crash between two separate writes would leave
// the copy standing with nobody — least of all the publisher — ever learning
// it exists, and no revocation able to reach it. This proves the double
// actually enforces that: a simulated failure on the adoption half leaves NO
// copy and NO adoption record, not a half-written pair.
func TestDerivingRollsBackWhenTheAdoptionWriteFails(t *testing.T) {
	svc, env := newSharingHarness(t)
	pub, err := svc.Publish(env.CtxAs(env.OwnerID), env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(env.CtxAs(env.OwnerID), pub.ID, env.OtherAccountID, "g1"); err != nil {
		t.Fatal(err)
	}

	env.sharing.failDerivationAdoption = true
	ctx := env.CtxAsOther(env.OtherOwnerID)
	target := workflow.ScopeRef{Scope: workflow.ScopeProject, ID: env.OtherProjectID}
	if _, err := svc.Derive(ctx, "@acme/backend-go", target, "d-fail"); errs.KindOf(err) != errs.KindUnavailable {
		t.Fatalf("expected the simulated transaction failure to surface: %v", err)
	}

	// No orphaned copy: the target project has to see NOTHING that was not
	// there before the failed attempt.
	list, err := svc.List(ctx, workflow.ScopeProject, env.OtherProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("a failed derivation left an orphaned copy nobody can ever revoke: %+v", list)
	}
	// And the publisher's side has to record nothing either — there is nothing
	// for it to have learned about.
	ads, err := svc.AdoptionsOf(env.CtxAs(env.OwnerID), pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ads) != 0 {
		t.Fatalf("a failed derivation still left an adoption record behind: %+v", ads)
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

// ── Revoke ───────────────────────────────────────────────────────────────────

// TestWhatARevocationReachesDependsOnTheStampedPolicy is the test the brief
// asked for, amended per ruling R14: the brief's own assertion —
// `env.FlowDeleted(copied.ID)` — called a map that was removed in an earlier
// fix round precisely because nothing ever wrote to it, which made the
// assertion unable to fail. "Revoking marks, never deletes" is checked here
// for real: the copy has to still be RETRIEVABLE, with its stages intact,
// after the revocation — that is the behaviour the design promises, and a
// boolean nothing sets is not it.
func TestWhatARevocationReachesDependsOnTheStampedPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy      workflow.RevocationPolicy
		copyRevoked bool
	}{
		{workflow.PolicyProspective, false}, // reaches the grant only
		{workflow.PolicyDrain, true},
		{workflow.PolicyTerminate, true},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			svc, env := newSharingHarness(t)
			env.AccountDefault = string(tc.policy)
			ctx := env.CtxAs(env.OwnerID)
			pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
			if err != nil {
				t.Fatal(err)
			}
			share, err := svc.Grant(ctx, pub.ID, env.OtherAccountID, "g1")
			if err != nil {
				t.Fatal(err)
			}
			otherCtx := env.CtxAsOther(env.OtherOwnerID)
			copied, err := svc.Derive(otherCtx,
				"@acme/backend-go", workflow.ScopeRef{Scope: workflow.ScopeProject, ID: env.OtherProjectID}, "d1")
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Revoke(ctx, share.ID); err != nil {
				t.Fatal(err)
			}
			if got := env.FlowRevoked(copied.ID); got != tc.copyRevoked {
				t.Fatalf("under %s the copy revoked = %v, wanted %v", tc.policy, got, tc.copyRevoked)
			}
			// Revoking marks, never deletes: the adopter's own edits and the audit
			// of a demand that already ran under the copy have to survive. That
			// means the copy is still there, retrievable, with its content whole —
			// so fetch it back and check exactly that, instead of asking a map
			// nothing writes to.
			still, err := svc.Get(otherCtx, copied.ID)
			if err != nil {
				t.Fatalf("the copy has to survive a revocation, retrievable: %v", err)
			}
			if len(still.Stages) != len(copied.Stages) {
				t.Fatalf("the copy's content changed on revocation: got %d stages, wanted %d", len(still.Stages), len(copied.Stages))
			}
			// The events are written by the adapter, in the same transaction
			// (ADR-0019), so what the DOMAIN owes is the decision: which
			// adoptions the policy reaches. Task 9 proves the events exist.
			rev := env.LastRevocation()
			if rev.Policy != tc.policy {
				t.Fatalf("the revocation carried %q, wanted %q", rev.Policy, tc.policy)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.copyRevoked]; len(rev.Adoptions) != want {
				t.Fatalf("under %s the revocation reached %d adoptions, wanted %d", tc.policy, len(rev.Adoptions), want)
			}
		})
	}
}

// TestRevocationReachesOnlyTheRevokedAccountsCopy is fix round 1's Important
// finding: a publication is granted to SEVERAL accounts over time — that is
// what the design is for — and the filter in Revoke that keeps a revocation
// to the account whose grant was actually revoked
// (`a.ByAccountID != share.ToAccountID`) had no test that could catch it
// being deleted. The original test only ever had one account derive from the
// publication, so `len(rev.Adoptions) == 1` held whether or not that filter
// existed.
//
// Here two accounts hold independent grants on the SAME publication and both
// derive from it. Revoking the SECOND account's grant must reach only its own
// copy — a THIRD account's copy, derived under its OWN still-standing grant,
// has to be left alone. Without the filter, revoking one company's grant
// would mark another company's copy revoked, and the first anyone would
// learn of it is a customer whose flow stopped resolving.
func TestRevocationReachesOnlyTheRevokedAccountsCopy(t *testing.T) {
	svc, env := newSharingHarness(t)
	env.AccountDefault = string(workflow.PolicyDrain)
	ctx := env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}

	otherShare, err := svc.Grant(ctx, pub.ID, env.OtherAccountID, "g-other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(ctx, pub.ID, env.ThirdAccountID, "g-third"); err != nil {
		t.Fatal(err)
	}

	otherCtx := env.CtxAsOther(env.OtherOwnerID)
	otherCopy, err := svc.Derive(otherCtx,
		"@acme/backend-go", workflow.ScopeRef{Scope: workflow.ScopeProject, ID: env.OtherProjectID}, "d-other")
	if err != nil {
		t.Fatal(err)
	}
	// The third account derives at its OWN account level — it needs no
	// workspace/project fixture, and Derive does not care which level within
	// the caller's own tree the copy lands at.
	thirdCopy, err := svc.Derive(env.CtxAsThird(),
		"@acme/backend-go", workflow.ScopeRef{Scope: workflow.ScopeAccount}, "d-third")
	if err != nil {
		t.Fatal(err)
	}

	// Revoke only the OTHER account's grant. The third account's grant is
	// untouched.
	if err := svc.Revoke(ctx, otherShare.ID); err != nil {
		t.Fatal(err)
	}

	if !env.FlowRevoked(otherCopy.ID) {
		t.Fatal("revoking the other account's grant has to reach its own copy")
	}
	if env.FlowRevoked(thirdCopy.ID) {
		t.Fatal("revoking one account's grant reached a DIFFERENT account's copy on the same publication")
	}
	rev := env.LastRevocation()
	if len(rev.Adoptions) != 1 || rev.Adoptions[0].FlowID != otherCopy.ID {
		t.Fatalf("the revocation has to carry only the revoked account's own adoption, got %+v", rev.Adoptions)
	}
}

// TestRevokingAnAlreadyRevokedShareIsIdempotent covers the early return in
// Revoke: asking for an outcome that already holds is success, and the
// second call must not touch anything a second time — not the share (already
// revoked, unchanged timestamp) and not the revocation log (RevokeShare is
// not called again).
func TestRevokingAnAlreadyRevokedShareIsIdempotent(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAs(env.OwnerID)
	pub, err := svc.Publish(ctx, env.FlowID, "backend-go", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	share, err := svc.Grant(ctx, pub.ID, env.OtherAccountID, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, share.ID); err != nil {
		t.Fatal(err)
	}
	before := len(env.sharing.revocations)

	if err := svc.Revoke(ctx, share.ID); err != nil {
		t.Fatalf("revoking an already-revoked share has to be success, not an error: %v", err)
	}
	if got := len(env.sharing.revocations); got != before {
		t.Fatalf("a second revoke recorded another revocation: had %d, now %d — RevokeShare must not be called again", before, got)
	}
}

// ── the pin ──────────────────────────────────────────────────────────────────

// TestInheritanceAcrossAnOwnerIsPinnedAndInsideOneAccountIsLive is the rule
// this task exists for, and it is deliberately written to fail in BOTH
// directions if it only tested one half: an account inheriting a flow it
// does NOT own (here, the platform catalogue — AccountID == "") must not move
// the instant the owner publishes; an account inheriting from ITSELF (its own
// account-level flow overlaying the same account's project) must move live,
// with no pin at all — that is governance already working, and a pin there
// would be ceremony.
func TestInheritanceAcrossAnOwnerIsPinnedAndInsideOneAccountIsLive(t *testing.T) {
	svc, env := newSharingHarness(t)
	// The platform's flow is at v1 and the account is pinned to it.
	eff, err := svc.Resolve(env.CtxAsOther(env.OtherOwnerID), workflow.ScopeProject, env.OtherProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Flow.Version != 1 {
		t.Fatalf("started on version %d", eff.Flow.Version)
	}
	// The platform publishes v2. The account must NOT move.
	env.AppendPlatformVersion()
	eff, err = svc.Resolve(env.CtxAsOther(env.OtherOwnerID), workflow.ScopeProject, env.OtherProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Flow.Version != 1 {
		t.Fatal("a change on the other side of an ownership boundary reached the account by itself")
	}
	// Bumping is deliberate.
	if err := svc.BumpPin(env.CtxAsOther(env.OtherOwnerID), env.PlatformFlowID, 2); err != nil {
		t.Fatal(err)
	}
	eff, err = svc.Resolve(env.CtxAsOther(env.OtherOwnerID), workflow.ScopeProject, env.OtherProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Flow.Version != 2 {
		t.Fatal("after bumping the pin the new version had to apply")
	}

	// Inside ONE account inheritance stays live: same owner, no pin, no ceremony.
	env.AppendAccountVersion() // creates the account's own flow, now at v2
	eff, err = svc.Resolve(env.CtxAsOther(env.OtherOwnerID), workflow.ScopeProject, env.OtherProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Flow.OwnerScope != workflow.ScopeAccount || eff.Flow.Version != 2 {
		t.Fatalf("the account's own flow had to overlay live at v2, got scope=%s version=%d",
			eff.Flow.OwnerScope, eff.Flow.Version)
	}
	// The account publishes AGAIN. A single resolve landing on v2 above proves
	// nothing by itself — a buggy Resolve that pins EVERY flow, own or not,
	// would also land on v2 here, because a first resolution auto-pins to
	// whatever is current. The real proof is that a SECOND change inside the
	// same account keeps applying live too, with no ceremony and no version
	// left behind — which a pin-everything bug would catch on this second
	// bump and freeze right here.
	env.AppendAccountVersion() // now at v3
	eff, err = svc.Resolve(env.CtxAsOther(env.OtherOwnerID), workflow.ScopeProject, env.OtherProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Flow.Version != 3 {
		t.Fatal("inheritance inside one account must not be pinned")
	}
}

// TestBumpPinRequiresManage mirrors Promote/Publish/Grant/Withdraw/Revoke: a
// developer moving the whole account onto another version of an inherited
// flow is not any member's decision to make alone.
func TestBumpPinRequiresManage(t *testing.T) {
	svc, env := newSharingHarness(t)
	if _, err := svc.Resolve(env.CtxAsOther(env.OtherOwnerID), workflow.ScopeProject, env.OtherProjectID); err != nil {
		t.Fatal(err)
	}
	dev := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: "usr-sharing-other-dev", AccountID: env.OtherAccountID, ActorKind: ctxutil.ActorUser,
	})
	env.roles["usr-sharing-other-dev@"+env.OtherAccountID] = "developer"
	if err := svc.BumpPin(dev, env.PlatformFlowID, 2); errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("a developer must not move the account onto another version: %v", err)
	}
}

// TestBumpPinRefusesAFlowTheAccountDoesNotInherit is R10: the schema cannot
// express, across tables, that a flow id is actually something the account
// inherits — the same reason flows.owner_id is checked in
// Service.resolveOwner rather than in a Postgres CHECK. Without this guard
// BumpPin would let an account pin ANY flow id, including one it has no
// relationship with at all — env.FlowID here belongs to a completely
// different account and never appears anywhere in the other account's chain.
func TestBumpPinRefusesAFlowTheAccountDoesNotInherit(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAsOther(env.OtherOwnerID)
	if err := svc.BumpPin(ctx, env.FlowID, 2); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("pinning a flow the account does not inherit at all has to be refused: %v", err)
	}

	// Even a flow the account DOES see — but only because it now declares its
	// OWN account-level flow, which stays live and crosses no boundary — must
	// be refused: there is nothing to pin when nothing is being pinned across
	// an ownership boundary.
	env.AppendAccountVersion()
	if err := svc.BumpPin(ctx, env.OtherFlowID, 2); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("pinning the account's OWN live flow has to be refused: %v", err)
	}
}

// TestBumpPinRequiresAnIdentifiedActor mirrors
// TestGrantRequiresAnIdentifiedActor: fakeAccess also rejects an empty actor
// key (there is no membership row for ""), which would mask a missing guard
// behind a Permission error instead of Unauthorized — asserting the SPECIFIC
// kind is what makes this test able to fail if the guard is ever removed.
func TestBumpPinRequiresAnIdentifiedActor(t *testing.T) {
	svc, env := newSharingHarness(t)
	if _, err := svc.Resolve(env.CtxAsOther(env.OtherOwnerID), workflow.ScopeProject, env.OtherProjectID); err != nil {
		t.Fatal(err)
	}
	anon := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: env.OtherAccountID, ActorKind: ctxutil.ActorUser,
	})
	if err := svc.BumpPin(anon, env.PlatformFlowID, 2); errs.KindOf(err) != errs.KindUnauthorized {
		t.Fatalf("an unidentified actor must not move the account's pin, and it must never be recorded empty: %v", err)
	}
}

// TestBumpPinRefusesAVersionThatWasNeverWritten is the fix for the review's
// Important finding #2: without this check, BumpPin(flowID, 99) succeeded
// unconditionally, and every FUTURE Resolve crossing this boundary would call
// VersionOf for a version nobody ever wrote and fail — flow resolution broken
// for every demand under the account until somebody re-pinned by hand.
func TestBumpPinRefusesAVersionThatWasNeverWritten(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAsOther(env.OtherOwnerID)
	// Resolve once so the account is actually pinned (to v1 — the only
	// version the platform's flow has in this harness).
	if _, err := svc.Resolve(ctx, workflow.ScopeProject, env.OtherProjectID); err != nil {
		t.Fatal(err)
	}
	if err := svc.BumpPin(ctx, env.PlatformFlowID, 99); errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("pinning a version that was never written has to be refused: %v", err)
	}
	// A refused bump must leave the EXISTING pin untouched — otherwise the
	// refusal itself would be the thing that breaks every future Resolve.
	version, pinned, err := env.sharing.PinOf(context.Background(), env.OtherAccountID, env.PlatformFlowID)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned || version != 1 {
		t.Fatalf("a refused BumpPin changed the existing pin: version=%d pinned=%v", version, pinned)
	}
}

// TestPinnedVersionsProvenanceReflectsThePinnedStagesNotTheCurrentOnes is the
// fix for the review's Important finding #1: substituting eff.Flow alone
// after MergeChain had already run left Contributors, Origins and
// ResolvedFrom describing the CURRENT stage set — exactly backwards from what
// EffectiveFlow's own doc comment promises support ("no way to explain a flow
// nobody remembers writing"), and exactly the case a pin is built for: the
// pinned and current versions declaring DIFFERENT stages.
func TestPinnedVersionsProvenanceReflectsThePinnedStagesNotTheCurrentOnes(t *testing.T) {
	svc, env := newSharingHarness(t)
	ctx := env.CtxAsOther(env.OtherOwnerID)

	// The first resolution pins to v1, which declares "context".
	eff, err := svc.Resolve(ctx, workflow.ScopeProject, env.OtherProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := eff.OriginOf("context"); !ok {
		t.Fatalf("v1's stage has to be in the provenance from the start: %+v", eff.Origins)
	}

	// The platform publishes v2 with a COMPLETELY DIFFERENT stage key — not
	// merely a new artifact on the same key, so a bug that reused the old
	// Origins/Contributors wholesale (instead of recomputing them) cannot
	// pass this test by accident.
	repo := env.sharing.repo
	r := repo.flows[env.PlatformFlowID]
	next := repo.current(r)
	next.Version = r.current + 1
	next.Stages = []workflow.StageSpec{stage("only-in-v2", workflow.TypeContext, workflow.ArtifactDocument)}
	if _, err := repo.AppendVersion(context.Background(), "", &next, r.current, "diff-stage-v2"); err != nil {
		t.Fatal(err)
	}

	eff, err = svc.Resolve(ctx, workflow.ScopeProject, env.OtherProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Flow.Version != 1 {
		t.Fatalf("still pinned to v1, got %d", eff.Flow.Version)
	}
	if len(eff.Flow.Stages) != 1 || eff.Flow.Stages[0].Key != "context" {
		t.Fatalf("the effective flow's own stages have to match the PINNED version: %+v", eff.Flow.Stages)
	}
	// The bug: patching eff.Flow alone while Origins/Contributors/ResolvedFrom
	// stayed computed against v2.
	if _, ok := eff.OriginOf("context"); !ok {
		t.Fatal("the pinned version's stage is missing from the provenance — Origins were computed against v2, not the pinned v1")
	}
	if _, ok := eff.OriginOf("only-in-v2"); ok {
		t.Fatal("a stage that exists ONLY in the unpinned current version leaked into the provenance")
	}
	// Only ONE level ever declares here (platform) — the point of this check
	// is that Contributors/Origins came out of re-running MergeChain against
	// the PINNED level, not that the trail happens to look a particular way.
	if len(eff.Contributors) != 1 || eff.Contributors[0].Scope != workflow.ScopePlatform {
		t.Fatalf("the provenance has to trace back to the platform level that declared the pinned stage: %+v", eff.Contributors)
	}
}
