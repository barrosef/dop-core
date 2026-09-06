package workflow

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/idem"
)

// Service concentrates the work flow rules. It takes only PORTS.
type Service struct {
	repo    Repository
	tree    Ancestry
	access  Access
	clock   ports.Clock
	sharing SharingRepository
}

// NewService requires a clock. Accepting nil is what kept the port decorative:
// the service fell back to time.Now() internally and no versioning test was
// deterministic. The panic here is deliberate — it is a wiring error, caught at
// boot.
func NewService(repo Repository, tree Ancestry, access Access, clock ports.Clock, sharing SharingRepository) *Service {
	if clock == nil {
		panic("workflow.NewService: clock is required — use clock.NewSystem()")
	}
	return &Service{repo: repo, tree: tree, access: access, clock: clock, sharing: sharing}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// ── reads ────────────────────────────────────────────────────────────────────

// List lists the account's visible flows. Content (flow, skill, git_flow) in an
// organization account is OPEN within the account by default (ADR-0014 §6):
// whoever is in the account sees what the account wrote. A credential is risk, a
// flow is knowledge.
func (s *Service) List(ctx context.Context, scope Scope, ownerID string) ([]Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if scope != "" && !ValidScope(scope) {
		return nil, errs.Invalid("unknown level: %q", scope)
	}
	return s.repo.List(ctx, accountID, scope, strings.TrimSpace(ownerID))
}

// Get returns the flow's CURRENT version.
func (s *Service) Get(ctx context.Context, id string) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("flow identifier not provided")
	}
	return s.repo.ByID(ctx, accountID, id)
}

// GetVersion returns a frozen version, exactly as it was written.
//
// It is what a demand in progress consumes: it stored (id, version) on start and
// has to keep seeing that document even after the flow has moved three versions
// on (ADR-0014 §4).
func (s *Service) GetVersion(ctx context.Context, id string, version int32) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("flow identifier not provided")
	}
	if version <= 0 {
		return nil, errs.Invalid("the flow version has to be positive")
	}
	return s.repo.VersionOf(ctx, accountID, id, version)
}

// Validate is the dry run: it returns the report without writing anything.
//
// It returns a Report and NOT an error when the flow is invalid — the client
// asked for an assessment, and receiving it as an RPC failure would force the
// screen to parse an error message to build the list of problems. The refusal
// with errs.Invalid happens in Create and Update, where an invalid flow actually
// prevents something.
func (s *Service) Validate(ctx context.Context, in Flow) (Report, error) {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return Report{}, err
	}
	in.Normalize()
	return Validate(in), nil
}

// ── writes ───────────────────────────────────────────────────────────────────

// Create writes a level's flow and its version 1.
//
// The level is checked against the account's TREE, not against what the client
// claims: a workspace id is guessable and travels in the request body, and
// without that check sending another account's id would be enough to hang a flow
// inside it.
func (s *Service) Create(ctx context.Context, in Flow, idempotencyKey string) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "actor not identified")
	}

	ref, err := s.resolveOwner(ctx, accountID, ScopeRef{Scope: in.OwnerScope, ID: in.OwnerID})
	if err != nil {
		return nil, err
	}
	if ref.Scope == ScopePlatform {
		return nil, errs.Permission("the platform catalogue is seeded by migration, not written by RPC: a level-0 flow applies to every account")
	}

	in.Normalize()
	if rep := Validate(in); !rep.Valid() {
		return nil, rep.Err()
	}

	now := s.now()
	f := Flow{
		AccountID:   accountID,
		OwnerScope:  ref.Scope,
		OwnerID:     ref.ID,
		Name:        in.Name,
		Description: in.Description,
		Version:     1,
		Stages:      in.Stages,
		CreatedBy:   call.ActorID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return s.repo.Create(ctx, &f, s.writeKey(idempotencyKey, "create", f, 0))
}

// Update CREATES A NEW VERSION. It never alters the existing one.
//
// Two defences against rewriting the past of something in execution:
//
//   - the previous version stays intact in the database (the port offers no path
//     to alter it, and the database refuses through a trigger);
//   - the version the author edited is compared with the current one BEFORE
//     writing. Without that, two people editing the same flow would produce
//     version 4 twice and the second would erase the first's work from the live
//     document.
//
// An identical resend does NOT version: a client with automatic retries would
// version the flow forever, and the demand would end up pointing at a version
// nobody wrote.
func (s *Service) Update(ctx context.Context, in Flow) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "actor not identified")
	}
	if strings.TrimSpace(in.ID) == "" {
		return nil, errs.Invalid("flow identifier not provided")
	}

	current, err := s.repo.ByID(ctx, accountID, in.ID)
	if err != nil {
		return nil, err
	}
	if current.OwnerScope == ScopePlatform {
		return nil, errs.Permission("the platform catalogue is not edited by RPC: derive a flow at your own level and the chain overlays what you declare")
	}

	in.Normalize()
	if rep := Validate(in); !rep.Valid() {
		return nil, rep.Err()
	}
	if in.Version != 0 && in.Version != current.Version {
		return nil, errs.Conflict(
			"flow %q is already at version %d and this change was written over version %d: reload before saving",
			current.Name, current.Version, in.Version)
	}
	if in.SameStages(*current) {
		return current, nil
	}

	next := Flow{
		ID:          current.ID,
		AccountID:   accountID,
		OwnerScope:  current.OwnerScope,
		OwnerID:     current.OwnerID,
		Name:        in.Name,
		Description: in.Description,
		Version:     current.Version + 1,
		Stages:      in.Stages,
		CreatedBy:   call.ActorID,
		CreatedAt:   current.CreatedAt,
		UpdatedAt:   s.now(),
	}
	// UpdateFlowRequest does not carry an idempotency_key: the key is DERIVED
	// from the content and the base version, which gives the same guarantee — the
	// same resend collides on the key and returns the version already written,
	// instead of stacking identical versions.
	return s.repo.AppendVersion(ctx, accountID, &next, current.Version,
		s.writeKey("", "update", next, current.Version))
}

// ── resolution ───────────────────────────────────────────────────────────────

// Resolve walks the chain platform ◁ account ◁ workspace ◁ project ◁ demand and
// returns the effective flow WITH its provenance.
//
// The order matters twice: the lineage comes from the account's tree (it is what
// says which project the demand lives in), and the overlay respects that order.
// A level that declares nothing inherits by omission and does not even appear in
// the trail.
func (s *Service) Resolve(ctx context.Context, scope Scope, scopeID string) (*EffectiveFlow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := s.resolveOwner(ctx, accountID, ScopeRef{Scope: scope, ID: scopeID})
	if err != nil {
		return nil, err
	}

	chain, err := s.tree.ChainOf(ctx, accountID, ref)
	if err != nil {
		return nil, err
	}
	declared, err := s.repo.ByOwners(ctx, accountID, chain)
	if err != nil {
		return nil, err
	}

	// Reorder what the repository returned according to the CHAIN — the overlay
	// depends on the order, and an order coming from ORDER BY would be order by
	// accident.
	byRef := make(map[ScopeRef]Flow, len(declared))
	for _, f := range declared {
		byRef[f.Ref()] = f
	}
	levels := make([]Flow, 0, len(chain))
	for _, c := range chain {
		if f, ok := byRef[c]; ok {
			levels = append(levels, f)
		}
	}

	eff := MergeChain(levels)
	if len(eff.Flow.Stages) == 0 {
		return nil, errs.NotFound(
			"a flow applicable to %s: no level of the chain declares stages, not even the platform catalogue", ref)
	}
	return &eff, nil
}

// ── promotion ────────────────────────────────────────────────────────────────

// Promote publishes the flow to a level ABOVE in the chain (ADR-0014 §5).
//
// Three refusals, each closing a different hole:
//
//   - a target outside the flow's own lineage: promoting is climbing YOUR chain,
//     not landing in a neighbouring project that happens to be in the same
//     account;
//   - a target in the platform catalogue: inside an organization account, a
//     lower-level flow is public WITHIN the account — never outside it. External
//     sharing is explicitly out of v1 (ADR-0014 §7);
//   - an actor without `manage`: publishing to the level above changes the flow
//     of people who asked for nothing, and that is not any member's decision.
func (s *Service) Promote(ctx context.Context, flowID string, target Scope, targetID string) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "actor not identified")
	}
	if strings.TrimSpace(flowID) == "" {
		return nil, errs.Invalid("flow identifier not provided")
	}
	if !ValidScope(target) {
		return nil, errs.Invalid("unknown target level: %q", target)
	}
	if target == ScopePlatform {
		return nil, errs.Permission("the platform catalogue does not receive an account\u2019s flow: inside an organization account a lower-level flow is public WITHIN the account, never outside it (ADR-0014 §6 and §7)")
	}

	src, err := s.repo.ByID(ctx, accountID, flowID)
	if err != nil {
		return nil, err
	}
	if src.OwnerScope == ScopePlatform {
		return nil, errs.Precondition("the flow is already in the platform catalogue: there is no level above")
	}
	if Rank(target) >= Rank(src.OwnerScope) {
		return nil, errs.Invalid(
			"promotion climbs the chain: %s is not above %s", target.Label(), src.OwnerScope.Label())
	}

	want := ScopeRef{Scope: target, ID: strings.TrimSpace(targetID)}
	if want.Scope == ScopeAccount && want.ID == "" {
		want.ID = accountID
	}
	chain, err := s.tree.ChainOf(ctx, accountID, src.Ref())
	if err != nil {
		return nil, err
	}
	if !containsRef(chain, want) {
		return nil, errs.Invalid(
			"%s %q is not in the flow\u2019s lineage: promoting is climbing your own chain, not publishing to a neighbouring branch",
			target.Label(), want.ID)
	}

	role, err := s.access.RoleOf(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !canManage(role) {
		return nil, errs.Permission("promoting requires manage over the account\u2019s content: owner or admin only")
	}

	src.CreatedBy = call.ActorID
	src.UpdatedAt = s.now()
	return s.repo.Promote(ctx, accountID, src, want,
		s.writeKey("", "promote:"+string(want.Scope)+":"+want.ID, *src, src.Version))
}

// ── sharing ──────────────────────────────────────────────────────────────────

// Publish makes the flow's CURRENT version addressable as @handle/slug@vN.
//
// It freezes a version rather than pointing at the flow: publishing again
// publishes a newer one, and what somebody already derived never moves under
// them.
func (s *Service) Publish(ctx context.Context, flowID, slug, notes, idempotencyKey string) (*Publication, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "actor not identified")
	}
	role, err := s.access.RoleOf(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !canManage(role) {
		return nil, errs.Permission("publishing a flow outside the account requires owner or admin")
	}
	// The slug is validated with the SAME rule the reference parser uses: a slug
	// accepted here and unusable in a reference would produce a publication
	// nobody can address.
	if !refPart.MatchString(slug) {
		return nil, errs.Invalid("%q is not a usable name for a reference: lowercase letters, digits and hyphens", slug)
	}
	f, err := s.repo.ByID(ctx, accountID, flowID)
	if err != nil {
		return nil, err
	}
	if f.OwnerScope == ScopePlatform {
		return nil, errs.Precondition("the platform's flow is inherited by the chain, not published for adoption")
	}
	p := Publication{
		FlowID: f.ID, AccountID: accountID, Slug: slug, Version: f.Version,
		Notes: notes, PublishedBy: call.ActorID, PublishedAt: s.now(),
	}
	return s.sharing.CreatePublication(ctx, &p, s.writeKey(idempotencyKey, "publish", *f, f.Version))
}

// Withdraw takes a publication out of circulation. It reaches NOBODY who has
// already derived: that is revocation's job, and it has its own policy.
func (s *Service) Withdraw(ctx context.Context, publicationID string) error {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return err
	}
	call, _ := ctxutil.From(ctx)
	role, err := s.access.RoleOf(ctx, call.ActorID, accountID)
	if err != nil {
		return err
	}
	if !canManage(role) {
		return errs.Permission("withdrawing a publication requires owner or admin")
	}
	return s.sharing.Withdraw(ctx, accountID, publicationID, s.now())
}

// ── helpers ──────────────────────────────────────────────────────────────────

// resolveOwner normalizes and CONFIRMS the addressed level.
//
// The account level is the only one that needs no id — it is always the active
// account, and accepting another id in the body would let the caller choose the
// tenant. The rest are confirmed against the tree: an id that does not exist, or
// that belongs to another account, comes back as NotFound from the port
// itself.
func (s *Service) resolveOwner(ctx context.Context, accountID string, ref ScopeRef) (ScopeRef, error) {
	ref.ID = strings.TrimSpace(ref.ID)
	if !ValidScope(ref.Scope) {
		return ScopeRef{}, errs.Invalid("unknown level: %q", ref.Scope)
	}
	switch ref.Scope {
	case ScopePlatform:
		return ScopeRef{Scope: ScopePlatform}, nil
	case ScopeAccount:
		if ref.ID != "" && ref.ID != accountID {
			return ScopeRef{}, errs.Permission("the account level is always the active account")
		}
		return ScopeRef{Scope: ScopeAccount, ID: accountID}, nil
	}
	if ref.ID == "" {
		return ScopeRef{}, errs.Invalid("level %s requires the owner identifier", ref.Scope.Label())
	}
	if _, err := s.tree.ChainOf(ctx, accountID, ref); err != nil {
		return ScopeRef{}, err
	}
	return ref, nil
}

// writeKey returns the write's idempotency key.
//
// Every write carries one (ADR-0017). When the contract does not bring the
// client's key, it is DERIVED from what is being written: the same resend
// produces the same key, collides on the unique index and returns what was
// already written — instead of creating a twin flow or a duplicate version.
func (s *Service) writeKey(given, op string, f Flow, base int32) string {
	if k := strings.TrimSpace(given); k != "" {
		return k
	}
	parts := []string{op, f.AccountID, string(f.OwnerScope), f.OwnerID, f.ID,
		strconv.Itoa(int(base)), f.Name, f.Description}
	for _, st := range f.Stages {
		parts = append(parts, st.Key, st.Name, string(st.Type), string(st.Gate))
		for _, a := range st.Artifacts {
			parts = append(parts, string(a))
		}
		parts = append(parts, st.Subtypes...)
	}
	return "wf:" + idem.Hash(parts...)
}

func containsRef(refs []ScopeRef, want ScopeRef) bool {
	for _, r := range refs {
		if r == want {
			return true
		}
	}
	return false
}
