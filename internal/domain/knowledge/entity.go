// Package knowledge is the project's knowledge base — rules, index and memory —
// and the assembly of the per-demand context package (ADR-0006).
//
// House rule: this package knows nothing of Postgres, gRPC or any SDK. It
// declares what it needs as a PORT (repository.go) and the composition root
// wires it.
//
// The idea that organizes the whole file: context is NOT "gather files and send
// them to the model". The package is SELECTED — it grows with the demand, not
// with the project — and the selection is a pure function, testable without a
// database, because it is what decides whether the agent is born knowing or born
// digging.
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ── the three layers (ADR-0006 §1) ───────────────────────────────────────────

type Kind string

const (
	// KindRule is a convention the agent OBEYS ("never merge develop into the
	// feature branch"). A rule that did not make it into the package is a rule
	// that gets violated.
	KindRule Kind = "rule"
	// KindIndex is ONE repository's map: what lives where, how to build, how to
	// test. Without it, every demand spends its first 30 minutes rediscovering
	// the repository.
	KindIndex Kind = "index"
	// KindMemory is a finding, a lesson and forensic analysis from past demands.
	// It is the layer that grows fastest and the one that most easily becomes
	// noise without curation (R-1).
	KindMemory Kind = "memory"
)

func ValidKind(k Kind) bool {
	switch k {
	case KindRule, KindIndex, KindMemory:
		return true
	}
	return false
}

// ── scope and inheritance ────────────────────────────────────────────────────

// ScopeLevel is where the artifact lives in the hierarchy. Inheritance is the
// reason the level exists: an account's rule applies to all of its projects, and
// a project can replace it with a rule of the same name — without copying the
// rule everywhere, which is how knowledge bases rot.
type ScopeLevel string

const (
	ScopeAccount   ScopeLevel = "account"
	ScopeWorkspace ScopeLevel = "workspace"
	ScopeProject   ScopeLevel = "project"
)

// Specificity orders the inheritance: the more specific wins over the more general.
func (l ScopeLevel) Specificity() int {
	switch l {
	case ScopeProject:
		return 2
	case ScopeWorkspace:
		return 1
	case ScopeAccount:
		return 0
	}
	return -1
}

// Scope ties the artifact to the account ALWAYS, and to the workspace or project
// when the level calls for it. AccountID is never optional: knowledge leaked
// between accounts is the worst defect possible on this platform, so the account
// is a field of the scope, not a parameter the adapter could forget.
type Scope struct {
	Level       ScopeLevel
	AccountID   string
	WorkspaceID string
	ProjectID   string
}

func AccountScope(accountID string) Scope {
	return Scope{Level: ScopeAccount, AccountID: accountID}
}

func WorkspaceScope(accountID, workspaceID string) Scope {
	return Scope{Level: ScopeWorkspace, AccountID: accountID, WorkspaceID: workspaceID}
}

func ProjectScope(accountID, projectID string) Scope {
	return Scope{Level: ScopeProject, AccountID: accountID, ProjectID: projectID}
}

// Validate refuses an incoherent scope on WRITE. The database repeats the check
// through a CHECK constraint; here the message is useful, there it is the last
// barrier.
func (s Scope) Validate() error {
	if strings.TrimSpace(s.AccountID) == "" {
		return errs.Invalid("knowledge artifact with no account")
	}
	switch s.Level {
	case ScopeAccount:
		if s.WorkspaceID != "" || s.ProjectID != "" {
			return errs.Invalid("an account scope points at neither a workspace nor a project")
		}
	case ScopeWorkspace:
		if s.WorkspaceID == "" || s.ProjectID != "" {
			return errs.Invalid("a workspace scope requires a workspace and no project")
		}
	case ScopeProject:
		if s.ProjectID == "" || s.WorkspaceID != "" {
			return errs.Invalid("a project scope requires a project and no workspace")
		}
	default:
		return errs.Invalid("unknown scope: %q", s.Level)
	}
	return nil
}

// ── the artifact ─────────────────────────────────────────────────────────────

// Artifact is one versioned piece of knowledge.
//
// The content lives in ONE of two places, never both:
//
//   - Body, in Postgres, when it is small — because what is small needs to be
//     indexable (trigram and vector) and read without a second network round
//     trip;
//   - ObjectRef, in the ObjectStore, when it is large — because a 4 MB
//     repository map inside a row turns every read of the table into a 4 MB
//     read, and Postgres is not an object store (ADR-0006 §2).
type Artifact struct {
	ID        string
	Scope     Scope
	Kind      Kind
	Name      string // rule: title; index: THE REPO NAME; memory: the finding's title
	Version   int32
	Body      string
	ObjectRef string
	SizeBytes int
	EstTokens int
	Embedding []float32 // only memory has one; empty when no Embedder is wired
	Meta      map[string]any
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AccountID is used constantly — in queries, in events and in edge conversion.
// The shortcut earns its keep.
func (a Artifact) AccountID() string { return a.Scope.AccountID }

// Externalized says whether the content is in the ObjectStore. Whoever reads an
// externalized artifact receives the REFERENCE, not the bytes: the sandbox
// fetches from storage, read-only, without pushing the binary through the
// core.
func (a Artifact) Externalized() bool { return a.ObjectRef != "" }

// InlineMaxBytes is the boundary between "fits in the row" and "goes to
// storage".
//
// 16 KiB is not a lucky magic number: it is the order of magnitude of a document
// an agent reads whole without blowing its budget (≈4k tokens), and it is small
// enough for Postgres to keep in-row without TOAST in most cases.
const InlineMaxBytes = 16 * 1024

// ArtifactContentType is the Content-Type used when writing to the ObjectStore.
//
// A documented caution, and the reason it is NOT application/json: the local
// environment's Storage emulator HANGS — the connection stays open until the
// client's timeout — on an upload with that exact type
// (dop-infra/docs/ambiente-local.md, ROADMAP item P-13). "text/plain;
// charset=utf-8" answers 200 in the emulator and in real GCS, and knowledge
// content is text (markdown, index JSON) anyway. Swapping this for
// application/json freezes the local environment with no error message at all —
// the worst kind of regression.
const ArtifactContentType = "text/plain; charset=utf-8"

// KnowledgeBucket holds the knowledge artifacts. The key is OPAQUE and FLAT by
// the port's contract (ports.ObjectStore); the account prefix serves operations
// and auditing, not navigation — the real isolation comes from the query, which
// always filters by account before returning any reference.
const KnowledgeBucket = "dop-knowledge"

// ObjectRefFor derives the content's reference from the artifact's IDENTITY
// (account, scope, kind, name) — never from the row's id.
//
// Two consequences, both wanted: the content can be written BEFORE the row
// exists (the id belongs to the database, and writing the object first is what
// avoids a row pointing at a nonexistent object); and a new version REPLACES the
// object atomically, which is the port's guarantee 2. Content history is not a
// promise of this layer: the version is the number on the row.
func ObjectRefFor(s Scope, kind Kind, name string) ports.ObjectRef {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{string(s.Level), s.AccountID, s.WorkspaceID, s.ProjectID, string(kind), name}, "|")))
	return ports.ObjectRef{
		Bucket: KnowledgeBucket,
		Key:    s.AccountID + "/" + hex.EncodeToString(sum[:16]),
	}
}

const nameMaxLen = 200

func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errs.Invalid("the knowledge artifact needs a name")
	}
	if len(name) > nameMaxLen {
		return errs.Invalid("the artifact name may have at most %d characters", nameMaxLen)
	}
	return nil
}

// EstimateTokens estimates the text's cost in tokens.
//
// The package's REAL measurement is token counting at the edge (a free endpoint,
// ADR-0008) — but the CUT has to happen here, offline and deterministic: a
// selection that depended on a network call would be non-deterministic, and a
// non-deterministic package invalidates the prompt's cached prefix, which is
// where 90% of the discount comes from. Four bytes per token is the usual
// approximation for Latin text; it errs by a little and it errs ALWAYS THE SAME
// WAY, which is what matters for the cache.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// perItemOverhead covers the header the package's serialization adds to each
// item (name, delimiter, layer label). Without counting it, the budget leaks a
// little per item — and "a little per item" over a long memory is enough to blow
// the window.
const perItemOverhead = 8

// TokenCost is what the artifact costs INSIDE the package. An externalized
// artifact costs only the header: the package carries the reference, not the
// bytes.
func (a Artifact) TokenCost() int {
	cost := EstimateTokens(a.Name) + perItemOverhead
	if a.Externalized() {
		return cost
	}
	if a.EstTokens > 0 {
		return cost + a.EstTokens
	}
	return cost + EstimateTokens(a.Body)
}

// ScoredArtifact is the artifact with the relevance the search assigned it.
// Score is comparable within ONE search, not across searches.
type ScoredArtifact struct {
	Artifact Artifact
	Score    float32
}

// Finding is the conclusion a subagent published on the demand (ADR-0007). It
// lives in the demand domain; here it comes in through the Demands port, with
// the minimal surface the package assembly consumes.
type Finding struct {
	ID       string
	ThreadID string
	Title    string
	Summary  string
}

func (f Finding) TokenCost() int {
	return EstimateTokens(f.Title) + EstimateTokens(f.Summary) + perItemOverhead
}

// ── rule inheritance ─────────────────────────────────────────────────────────

// ResolveRules applies the hierarchy's inheritance over a scope's rules.
//
// Two decisions, both visible in the result:
//
//  1. the more specific WINS: a project rule with the same name as an account
//     rule replaces the account's. It is what allows "the house testing policy,
//     except in this legacy project" without duplicating the policy;
//  2. the list comes out from the most specific to the most general. That is not
//     aesthetics: it is what makes the budget cut sacrifice the generic rule
//     first — the one the project has least reason to depend on.
//
// An externalized rule does NOT get in: a rule is text the agent reads whole,
// and PutArtifact refuses a rule that does not fit inline for exactly this
// reason.
func ResolveRules(rules []Artifact) []string {
	resolved := ResolveRuleArtifacts(rules)
	out := make([]string, 0, len(resolved))
	for _, r := range resolved {
		out = append(out, r.Body)
	}
	return out
}

// ResolveRuleArtifacts is the same resolution keeping the ARTIFACT.
//
// It exists because the shelf (library.go) needs what ResolveRules throws away:
// the name, so the file has one, and the scope, so the manifest can say whether
// the rule came from the account or was written for the project. The precedence
// lives here, in ONE place — which is exactly what the comment above asks for.
func ResolveRuleArtifacts(rules []Artifact) []Artifact {
	ordered := make([]Artifact, len(rules))
	copy(ordered, rules)
	sort.SliceStable(ordered, func(i, j int) bool {
		si, sj := ordered[i].Scope.Level.Specificity(), ordered[j].Scope.Level.Specificity()
		if si != sj {
			return si > sj
		}
		// Tie-break by name: the package's order has to be stable across runs,
		// otherwise the prompt's cached prefix changes for no reason.
		return ordered[i].Name < ordered[j].Name
	})

	seen := make(map[string]bool, len(ordered))
	out := make([]Artifact, 0, len(ordered))
	for _, r := range ordered {
		if r.Kind != KindRule || seen[r.Name] || strings.TrimSpace(r.Body) == "" {
			continue
		}
		seen[r.Name] = true
		out = append(out, r)
	}
	return out
}

// ── budget and package selection (ADR-0008) ──────────────────────────────────

// Budget is the package's budget, in tokens.
//
// It is a PARAMETER, not a buried constant: the ceiling changes per project, per
// model and per cost decision, and a number hidden in the middle of the assembly
// would be impossible to adjust without recompiling. The fractions bound EACH
// layer, so that memory — the only one that grows without end — does not eat the
// whole package (R-1).
type Budget struct {
	Total        int     // ceiling of the whole package
	FindingShare float64 // fraction of the total reserved for the demand's findings
	IndexShare   float64 // ...for the index of the demand's repositories
	MemoryShare  float64 // ...for the relevant memories
}

// DefaultBudget is the starting point — and nothing more. Whoever assembles the
// service picks the ceiling; this value exists so that "I did not pick one" does
// not mean "no ceiling".
func DefaultBudget() Budget {
	return Budget{Total: 24000, FindingShare: 0.20, IndexShare: 0.35, MemoryShare: 0.30}
}

// Normalize fills in whatever arrived as zero. A zero budget means "use the
// default", never "nothing fits": an empty package through a configuration
// mistake would be an agent born blind, and that has to be an explicit decision,
// not a default.
func (b Budget) Normalize() Budget {
	d := DefaultBudget()
	if b.Total <= 0 {
		b.Total = d.Total
	}
	if b.FindingShare <= 0 {
		b.FindingShare = d.FindingShare
	}
	if b.IndexShare <= 0 {
		b.IndexShare = d.IndexShare
	}
	if b.MemoryShare <= 0 {
		b.MemoryShare = d.MemoryShare
	}
	return b
}

func share(total int, frac float64) int { return int(float64(total) * frac) }

// Dropped counts what was LEFT OUT, per layer.
//
// It exists to be emitted together with the measurement: "the package fit" and
// "the package fit because we threw away half the relevant memory" are different
// facts, and only the second explains an agent that did not know what it should
// have known.
type Dropped struct {
	Rules    int
	Findings int
	Index    int
	Memories int
}

func (d Dropped) Any() bool { return d.Rules+d.Findings+d.Index+d.Memories > 0 }

// Candidates is everything that COULD enter the package, already filtered by
// account and by demand in the queries. The selection decides what actually
// enters.
type Candidates struct {
	Rules    []Artifact
	Findings []Finding
	Index    []Artifact
	Memories []ScoredArtifact
}

// Package is the agent's carry-on luggage (ADR-0006 §3).
//
// No timestamp and no volatile id, on purpose: the package enters the prompt's
// CACHED PREFIX, and a byte that changes on every assembly burns the cache
// discount in silence (ADR-0008 §1).
type Package struct {
	DemandID        string
	Rules           []string
	Findings        []Finding
	Index           []Artifact
	Memories        []Artifact
	EstimatedTokens int
	Budget          int
	Dropped         Dropped
}

// Truncated says whether curation had to cut. It is the alert's trigger: a
// ceiling hit often means a demand that is too large or a memory badly pruned.
func (p Package) Truncated() bool { return p.Dropped.Any() }

// SelectPackage assembles the package within the budget. It is THIS DOMAIN'S
// HEART, and it is a pure function so it can be tested without a database,
// without a network and without a model.
//
// What goes in, in what order and by what criterion:
//
//  1. RULES, already resolved by inheritance (most specific first). They go in
//     ahead of everything because an ignored rule is guaranteed rework: the
//     agent opens a PR against the wrong branch and the cost is a whole review
//     cycle.
//  2. FINDINGS already published on the demand. On a resume, it is what keeps
//     the agent from redoing an investigation a sibling already finished
//     (ADR-0007/0012 §3).
//  3. THE INDEX OF THE DEMAND'S REPOSITORIES — not the whole project's. This is
//     where the discipline "it grows with the DEMAND" lives: the project may
//     have 40 repos, the demand touches two. The query is what selected the two;
//     this function only respects the budget.
//  4. MEMORIES, by descending relevance. They come last because they are the
//     bulkiest and least precise layer: it is the first place where cutting
//     hurts little, and the agent can ask for more during execution
//     (SearchMemory).
//
// What is left out, and why:
//
//   - whatever does not fit its own layer's ceiling — memory does not invade the
//     index's quota even when there is room to spare;
//   - within a layer, EVERYTHING from the first item that did not fit. We do not
//     skip the large item to squeeze in the next smaller one: that would trade
//     relevance order for a packing heuristic, and would return a package where
//     the 7th memory got in and the 3rd did not. Curation is not a knapsack.
func SelectPackage(b Budget, in Candidates) Package {
	b = b.Normalize()
	p := Package{Budget: b.Total}

	remaining := b.Total
	// spend tries to pay `cost` respecting the layer's ceiling and the package's.
	spend := func(cost int, layerCeiling *int) bool {
		if cost > remaining || cost > *layerCeiling {
			return false
		}
		remaining -= cost
		*layerCeiling -= cost
		return true
	}

	// Rules have no quota of their own: their ceiling is the whole package. A
	// rule base that on its own blows the budget is a rule-curation problem, and
	// Dropped reports it — but cutting a rule to fit memory would be the wrong
	// trade.
	rulesCeiling := b.Total
	rules := ResolveRules(in.Rules)
	for i, r := range rules {
		if !spend(EstimateTokens(r)+perItemOverhead, &rulesCeiling) {
			p.Dropped.Rules = len(rules) - i
			break
		}
		p.Rules = append(p.Rules, r)
	}

	findingsCeiling := share(b.Total, b.FindingShare)
	for i, f := range in.Findings {
		if !spend(f.TokenCost(), &findingsCeiling) {
			p.Dropped.Findings = len(in.Findings) - i
			break
		}
		p.Findings = append(p.Findings, f)
	}

	// The index in a stable name order: repository A comes before B in every
	// assembly, today and a month from now.
	index := make([]Artifact, len(in.Index))
	copy(index, in.Index)
	sort.SliceStable(index, func(i, j int) bool { return index[i].Name < index[j].Name })

	indexCeiling := share(b.Total, b.IndexShare)
	for i := range index {
		if !spend(index[i].TokenCost(), &indexCeiling) {
			p.Dropped.Index = len(index) - i
			break
		}
		p.Index = append(p.Index, index[i])
	}

	// Memories by relevance; ties broken by id, again for determinism — two
	// memories with the same score must not swap places between two assemblies of
	// the same demand.
	mem := make([]ScoredArtifact, len(in.Memories))
	copy(mem, in.Memories)
	sort.SliceStable(mem, func(i, j int) bool {
		if mem[i].Score != mem[j].Score {
			return mem[i].Score > mem[j].Score
		}
		return mem[i].Artifact.ID < mem[j].Artifact.ID
	})

	memoryCeiling := share(b.Total, b.MemoryShare)
	for i := range mem {
		if !spend(mem[i].Artifact.TokenCost(), &memoryCeiling) {
			p.Dropped.Memories = len(mem) - i
			break
		}
		p.Memories = append(p.Memories, mem[i].Artifact)
	}

	p.EstimatedTokens = b.Total - remaining
	return p
}
