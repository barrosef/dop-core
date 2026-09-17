package knowledge

import (
	"context"
	"time"
)

// Repository is the knowledge domain's persistence PORT.
//
// Declared here, in domain language; implemented in internal/adapter/postgres.
// The domain never sees SQL.
//
// Every operation takes accountID explicitly. It is not redundant with Scope:
// this is the read side, where there is no scope to carry the account. Knowledge
// that leaks from one account to another is the worst conceivable defect on this
// platform — the filter is a required parameter of the port, never trust in the
// caller.
type Repository interface {
	// Put writes the artifact and emits the event in the SAME transaction,
	// honouring the idempotency key: repeating the same write returns the same
	// artifact instead of creating a new version (ADR-0013/0019).
	Put(ctx context.Context, a *Artifact, idem Idempotency) (*Artifact, error)

	// IndexOf returns ONE of the project's repositories' map. (nil, nil) when it
	// does not exist: "index absent" is a legitimate answer, and the use case is
	// what decides whether that is an error.
	IndexOf(ctx context.Context, accountID, projectID, repo string) (*Artifact, error)

	// IndexFor brings the index OF THE REQUESTED REPOSITORIES — the demand's, not
	// the whole project's. An empty list returns nothing, not "everything": it is
	// the difference between a package that grows with the demand and one that
	// grows with the project.
	IndexFor(ctx context.Context, accountID, projectID string, repos []string) ([]Artifact, error)

	// RulesFor returns the rules that REACH the project: its own, its containing
	// workspace's and the account's. Resolving the inheritance belongs to the
	// domain (ResolveRules); this port only brings the candidates — that way the
	// precedence rule exists in one place.
	RulesFor(ctx context.Context, accountID, projectID string) ([]Artifact, error)

	// SearchMemory searches the memory layer. See MemoryQuery.
	SearchMemory(ctx context.Context, q MemoryQuery) ([]ScoredArtifact, error)

	// RecordContextBuild records the assembly's MEASUREMENT as an event
	// (ADR-0006 §3, ADR-0008 §1). It is not optional telemetry: "the package
	// grows with the demand" needs a number, otherwise it is an impression — and
	// a silent cut is exactly the defect nobody notices.
	RecordContextBuild(ctx context.Context, accountID, demandID string, m PackageMetrics) error
}

// Idempotency is the write's key and the signature of the content it carries.
//
// The two travel together because the SAME key with a different body is a
// conflict, not a repeat: without the hash, a client bug reusing a key would
// become silent corruption of the knowledge base.
type Idempotency struct {
	Key         string
	RequestHash string
}

// MemoryQuery is a search in memory. The two paths coexist on purpose:
//
//   - with Embedding filled in, the search is SEMANTIC (pgvector): it finds the
//     lesson about "connection timeout" when the demand talks about "intermittent
//     database drops";
//   - without it, the search is LEXICAL (trigram). It is not the intended path,
//     it is what is left when no embedding service is wired — and returning
//     nothing in that case would be worse: a memory found by word is still better
//     than an agent starting from zero.
type MemoryQuery struct {
	AccountID string
	ProjectID string
	Text      string
	Embedding []float32
	Limit     int
}

// PackageMetrics is what the assembly's measurement publishes.
type PackageMetrics struct {
	// At comes from the service's clock (ports.Clock), not from the database: it
	// is the instant of the ASSEMBLY, and it is what makes the measurement
	// deterministic in tests.
	At              time.Time
	EstimatedTokens int
	Budget          int
	Rules           int
	Findings        int
	Index           int
	Memories        int
	Dropped         Dropped
}

// ── narrow ports out of the domain ───────────────────────────────────────────

// Embedder vectorizes text for semantic search.
//
// It is a port, and it is NARROW on purpose: the domain does not know whether on
// the other side there is a hosted model, a third-party API or a local service,
// and it must not know. Two operations and nothing more.
//
// Dimensions exists so that an incompatibility surfaces at boot, not in silence:
// writing a 768-dimension vector into a vector(1536) column is a database error,
// but SEARCHING with a different embedder from the one that generated the vectors
// returns a plausible and wrong result — the worst failure mode a search can
// have.
//
// There is no adapter for this port yet; the wiring satisfies it when there is an
// embedding service. Until then, the service accepts a nil Embedder and falls
// back to lexical search (see MemoryQuery), which is documented in NewService.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dimensions() int
}

// EmbeddingDim is the dimension of migration 0007's `vector` column. Changing
// embedding model implies migrating the column AND reindexing all of memory: the
// old vectors are not comparable with the new ones.
const EmbeddingDim = 1536

// Demands is the NARROW port into the demand domain — the demand is the unit the
// context package serves, and knowledge needs three things about it: which
// project it lives in, which repositories it touches and what has already been
// concluded on it. Nothing beyond that.
//
// Declared here, and not imported from there, for the same reason
// resource.Access exists: the knowledge domain must not depend on another
// domain's internal shape, and the wiring joins the two ends.
type Demands interface {
	ContextOf(ctx context.Context, accountID, demandID string) (*DemandContext, error)
}

// DemandContext is the slice of the demand the assembly consumes.
type DemandContext struct {
	DemandID  string
	ProjectID string
	Title     string
	// Spec is the demand's statement. It feeds the SEARCH for relevant memories —
	// relevance is measured against what the demand asks for, not against the
	// whole project.
	Spec string
	// Repos are the repositories the demand touches. It is the filter that keeps
	// the index proportional to the demand (ADR-0006 §3).
	Repos []string
	// Findings are the findings already published on the demand — empty at the
	// start, populated on a resume (ADR-0008 §3).
	Findings []Finding
}
