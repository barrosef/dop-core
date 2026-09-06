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
	repo     Repository
	tree     Ancestry
	access   Access
	clock    ports.Clock
	sharing  SharingRepository
	defaults AccountDefaults
}

// NewService requires a clock. Accepting nil is what kept the port decorative:
// the service fell back to time.Now() internally and no versioning test was
// deterministic. The panic here is deliberate — it is a wiring error, caught at
// boot.
func NewService(repo Repository, tree Ancestry, access Access, clock ports.Clock, sharing SharingRepository, defaults AccountDefaults) *Service {
	if clock == nil {
		panic("workflow.NewService: clock is required — use clock.NewSystem()")
	}
	return &Service{repo: repo, tree: tree, access: access, clock: clock, sharing: sharing, defaults: defaults}
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

	eff, err := s.resolveChain(ctx, accountID, ref)
	if err != nil {
		return nil, err
	}
	if len(eff.Flow.Stages) == 0 {
		return nil, errs.NotFound(
			"a flow applicable to %s: no level of the chain declares stages, not even the platform catalogue", ref)
	}

	// A flow whose owner is not this account crossed an OWNERSHIP boundary, and
	// the version that applies is the pinned one — not the newest. Whoever
	// changes it is not whoever lives with the change, and an automatic change
	// to how somebody's development runs is what the copy-versus-reference
	// decision refused in the first place (see Derive).
	//
	// Inheritance INSIDE one account is deliberately untouched: there, the
	// person changing the flow is the same owner who lives with it, and a pin
	// would be ceremony. That is why this compares ACCOUNTS, not chain depth —
	// account ▷ workspace ▷ project ▷ demand all share one owner and stay live.
	if eff.Flow.AccountID != accountID {
		version, pinned, err := s.sharing.PinOf(ctx, accountID, eff.Flow.ID)
		if err != nil {
			return nil, err
		}
		if !pinned {
			// A first resolution pins to what is current, so the account starts
			// on a known version instead of on "whatever is newest today".
			version = eff.Flow.Version
			if err := s.sharing.Pin(ctx, accountID, eff.Flow.ID, version, "", s.now()); err != nil {
				return nil, err
			}
		}
		if version != eff.Flow.Version {
			frozen, err := s.repo.VersionOf(ctx, eff.Flow.AccountID, eff.Flow.ID, version)
			if err != nil {
				return nil, err
			}
			eff.Flow = *frozen
		}
	}
	return &eff, nil
}

// resolveChain walks the account's chain and overlays what it declares, with
// NO pin applied. Resolve builds on it and then decides whether the result
// crossed an ownership boundary; BumpPin builds on it to confirm the flow it
// is asked to pin is actually the one the account would inherit that way —
// splitting the walk out is what lets both share it instead of drifting apart
// over time into two slightly different ideas of "what this account inherits".
func (s *Service) resolveChain(ctx context.Context, accountID string, ref ScopeRef) (EffectiveFlow, error) {
	chain, err := s.tree.ChainOf(ctx, accountID, ref)
	if err != nil {
		return EffectiveFlow{}, err
	}
	declared, err := s.repo.ByOwners(ctx, accountID, chain)
	if err != nil {
		return EffectiveFlow{}, err
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
	return MergeChain(levels), nil
}

// BumpPin moves the account onto a different (usually newer) version of a
// flow it inherits across an ownership boundary. It is the deliberate act
// that a live reference would have taken away: the platform (or another
// account's publication) can move on without dragging every inheritor with
// it, and moving is something an owner or admin chooses, on purpose.
func (s *Service) BumpPin(ctx context.Context, flowID string, version int32) error {
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
		return errs.Permission("moving the account onto another version requires owner or admin")
	}
	flowID = strings.TrimSpace(flowID)
	if flowID == "" {
		return errs.Invalid("flow identifier not provided")
	}
	if version <= 0 {
		return errs.Invalid("version has to be positive")
	}

	// R10: nothing in the schema can check, ACROSS TABLES, that flowID is
	// something this account actually inherits — a Postgres CHECK cannot see
	// another table, which is exactly why flows.owner_id is validated in
	// Service.resolveOwner instead of in SQL. Without this, any account could
	// pin — and freeze itself onto — a flow it has no relationship with at
	// all, including one it was never granted and cannot even read.
	//
	// The account's OWN chain (rooted at the account level, not at whatever
	// scope the caller happens to be resolving today) is what decides this: the
	// pin is stored per (account, flow), not per resolution path, so the flow
	// it is allowed to reach is the one the account would inherit ACROSS AN
	// OWNERSHIP BOUNDARY by default — the same flow Resolve would pin the very
	// first time it crossed one.
	eff, err := s.resolveChain(ctx, accountID, ScopeRef{Scope: ScopeAccount, ID: accountID})
	if err != nil {
		return err
	}
	if eff.Flow.AccountID == accountID || eff.Flow.ID != flowID {
		return errs.Invalid(
			"%q is not a flow this account inherits across an ownership boundary: there is nothing to pin", flowID)
	}

	return s.sharing.Pin(ctx, accountID, flowID, version, call.ActorID, s.now())
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
	if call.ActorID == "" {
		return errs.New(errs.KindUnauthorized, "actor not identified")
	}
	role, err := s.access.RoleOf(ctx, call.ActorID, accountID)
	if err != nil {
		return err
	}
	if !canManage(role) {
		return errs.Permission("withdrawing a publication requires owner or admin")
	}
	return s.sharing.Withdraw(ctx, accountID, publicationID, s.now())
}

// Grant lets ONE account derive from a publication.
//
// The revocation policy is COPIED here from the publisher's default and stored
// on the grant. Reading it at revocation time would let the publisher change
// the terms after they were accepted — the difference between "no new
// derivations" and "your running demand stops now".
func (s *Service) Grant(ctx context.Context, publicationID, toAccountID, idempotencyKey string) (*Share, error) {
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
		return nil, errs.Permission("granting a flow requires owner or admin")
	}
	toAccountID = strings.TrimSpace(toAccountID)
	if toAccountID == "" {
		return nil, errs.Invalid("no account to grant to")
	}
	if toAccountID == accountID {
		return nil, errs.Invalid("an account already sees its own flows: there is nothing to grant")
	}
	pub, err := s.sharing.PublicationByID(ctx, accountID, publicationID)
	if err != nil {
		return nil, err
	}
	if pub.Withdrawn() {
		return nil, errs.Precondition("this publication was withdrawn: publish a version again before granting it")
	}
	raw, err := s.defaults.DefaultRevocationPolicy(ctx, accountID)
	if err != nil {
		return nil, err
	}
	policy := RevocationPolicy(raw)
	if !ValidRevocationPolicy(policy) {
		policy = DefaultRevocationPolicy
	}
	sh := Share{
		PublicationID: pub.ID, ToAccountID: toAccountID,
		RevocationPolicy: policy, GrantedBy: call.ActorID, GrantedAt: s.now(),
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		key = "wf:" + idem.Hash("grant", pub.ID, toAccountID)
	}
	return s.sharing.CreateShare(ctx, &sh, key)
}

// SharesOf lists who a publication was granted to.
func (s *Service) SharesOf(ctx context.Context, publicationID string) ([]Share, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.sharing.SharesOfPublication(ctx, accountID, publicationID)
}

// Derive adopts a published flow: it creates a COPY in the caller's account, at
// the level the caller chooses, carrying where it came from.
//
// A copy and not a reference, because a live reference would let one company's
// edit change how another company's development runs, and revoking it would
// break demands already moving.
func (s *Service) Derive(ctx context.Context, rawRef string, target ScopeRef, idempotencyKey string) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "actor not identified")
	}
	ref, err := ParseRef(rawRef)
	if err != nil {
		return nil, err
	}
	// The one deliberate crossing. It answers NotFound when there is no grant:
	// whether a flow exists in another account is not an outsider's to learn.
	pub, err := s.sharing.ResolvePublication(ctx, accountID, ref)
	if err != nil {
		return nil, err
	}
	// The CONTENT comes from the publisher's frozen version, read by id and
	// version — the same path a demand uses to read what it froze on.
	src, err := s.repo.VersionOf(ctx, pub.AccountID, pub.FlowID, pub.Version)
	if err != nil {
		return nil, err
	}
	owner, err := s.resolveOwner(ctx, accountID, target)
	if err != nil {
		return nil, err
	}
	if owner.Scope == ScopePlatform {
		return nil, errs.Permission("a derived flow does not go into the platform catalogue")
	}
	now := s.now()
	copied := Flow{
		AccountID: accountID, OwnerScope: owner.Scope, OwnerID: owner.ID,
		Name: src.Name, Description: src.Description, Version: 1, Stages: src.Stages,
		Origin:    &Origin{Ref: ref.WithoutVersion().String(), Version: pub.Version, AdoptedAt: now},
		CreatedBy: call.ActorID, CreatedAt: now, UpdatedAt: now,
	}
	// The copy and the publisher's half of the same fact are written together.
	// Two separate calls (create the copy, then record the adoption) would let a
	// crash between them leave a copy the publisher never learns of — and the
	// adoption record is exactly what a later revocation uses to reach the copy,
	// so an orphaned copy is one nobody could ever revoke.
	adoption := &Adoption{
		PublicationID: pub.ID, Version: pub.Version, ByAccountID: accountID, DerivedAt: now,
	}
	return s.sharing.RecordDerivation(ctx, accountID, &copied, adoption,
		s.writeKey(idempotencyKey, "derive", copied, 0))
}

// Revoke withdraws a grant. WHAT it reaches is the policy stamped on that
// grant when it was made, never the account's current default: reading the
// live default here would let the publisher change the terms after somebody
// already accepted them.
//
// Under `prospective` it reaches the grant and nothing else. Under `drain`
// and `terminate` it also marks the copies derived under it as revoked — a
// STATE CHANGE, never a deletion: the adopter's own edits and the audit trail
// of a demand that already ran have to survive, or a green becomes
// uncheckable.
//
// The DIFFERENCE between drain and terminate is what happens to a demand
// already running, and that does not happen here: this decides which
// adoptions the policy reaches and hands the whole thing to RevokeShare in
// one call, which writes it — and, from Task 9, the events — in one
// transaction (ADR-0019). Putting demand control in this service would make
// the flow domain a client of the demand domain over one decision.
func (s *Service) Revoke(ctx context.Context, shareID string) error {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return errs.New(errs.KindUnauthorized, "actor not identified")
	}
	role, err := s.access.RoleOf(ctx, call.ActorID, accountID)
	if err != nil {
		return err
	}
	if !canManage(role) {
		return errs.Permission("revoking a grant requires owner or admin")
	}
	share, err := s.sharing.ShareByID(ctx, accountID, shareID)
	if err != nil {
		return err
	}
	if share.Revoked() {
		return nil // idempotent: the outcome asked for is already true
	}
	rev := Revocation{
		ShareID: share.ID, PublicationID: share.PublicationID,
		ToAccountID: share.ToAccountID, Policy: share.RevocationPolicy, At: s.now(),
	}
	if share.RevocationPolicy != PolicyProspective {
		ads, err := s.sharing.AdoptionsOfPublication(ctx, accountID, share.PublicationID)
		if err != nil {
			return err
		}
		for _, a := range ads {
			if a.ByAccountID != share.ToAccountID || !a.RevokedAt.IsZero() {
				continue
			}
			rev.Adoptions = append(rev.Adoptions, AdoptionRef{ID: a.ID, FlowID: a.FlowID})
		}
	}
	return s.sharing.RevokeShare(ctx, accountID, rev)
}

// AdoptionsOf lists who derived from a publication.
func (s *Service) AdoptionsOf(ctx context.Context, publicationID string) ([]Adoption, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.sharing.AdoptionsOfPublication(ctx, accountID, publicationID)
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
