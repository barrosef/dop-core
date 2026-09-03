package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/idem"
)

// Service concentrates the knowledge rules. It takes only PORTS.
type Service struct {
	repo     Repository
	objects  ports.ObjectStore
	demands  Demands
	embedder Embedder
	clock    ports.Clock
	budget   Budget
}

// NewService requires a repository, an ObjectStore, demands and a clock; the
// Embedder is the only optional one.
//
// The panic here is deliberate — it is a WIRING error, caught at boot, not in
// production at three in the morning. It holds especially for the clock:
// accepting nil would make it fall back to time.Now() internally, and the port
// would become decoration.
//
// The Embedder is optional because the alternative would be worse. With no
// embedding service wired, requiring the port would take the whole domain
// offline; with it nil, the memory search falls back to the LEXICAL path
// (trigram) and everything else works. The degradation is explicit, not silent:
// whoever assembles the service chooses, and SearchMemory says which path
// answered.
//
// The budget is a PARAMETER, not a constant hidden in the assembly: the
// package's ceiling changes per project, per model and per cost decision. A zero
// Budget means "use the default" (see Budget.Normalize), never "nothing fits".
func NewService(repo Repository, objects ports.ObjectStore, demands Demands,
	embedder Embedder, clock ports.Clock, budget Budget) *Service {
	if repo == nil {
		panic("knowledge.NewService: repository is required")
	}
	if objects == nil {
		panic("knowledge.NewService: ObjectStore is required — a large artifact does not fit in the row")
	}
	if demands == nil {
		panic("knowledge.NewService: the demands port is required — the context package is PER demand")
	}
	if clock == nil {
		panic("knowledge.NewService: clock is required — use clock.NewSystem()")
	}
	return &Service{
		repo: repo, objects: objects, demands: demands,
		embedder: embedder, clock: clock, budget: budget.Normalize(),
	}
}

// memoryCandidates is how many memories the search brings for the assembly to
// CONSIDER. The final cut belongs to the budget; bringing more candidates than
// fit is what lets the budget choose among them by relevance instead of
// accepting whatever the query returned.
const memoryCandidates = 24

// BuildContextPackage assembles the agent's carry-on luggage for a demand.
//
// This is where the token saving happens (ADR-0012): the package is SELECTED,
// not dumped. The composition comes from ADR-0009 §3 — rules + the index OF THE
// DEMAND'S REPOSITORIES + relevant memories + findings already published — and
// the cut criterion lives entirely in SelectPackage, which is a pure function.
//
// A zero budget uses the service's; passing a value lets the caller tighten the
// ceiling without reassembling the service.
func (s *Service) BuildContextPackage(ctx context.Context, demandID string, budget Budget) (*Package, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("demand not provided")
	}
	if budget.Total <= 0 {
		budget = s.budget
	}

	dc, err := s.demands.ContextOf(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	if dc == nil {
		return nil, errs.NotFound("demand")
	}

	rules, err := s.repo.RulesFor(ctx, accountID, dc.ProjectID)
	if err != nil {
		return nil, err
	}

	// The index: only the repositories the demand touches. The project may have
	// forty; the demand touches two. This line IS the discipline "the package
	// grows with the DEMAND, not with the project".
	index, err := s.repo.IndexFor(ctx, accountID, dc.ProjectID, dc.Repos)
	if err != nil {
		return nil, err
	}

	// Relevance is measured against what the demand ASKS FOR — title and
	// statement — not against the project. Memory relevant to the whole project
	// is the whole memory, and then there is no selection at all.
	memories, err := s.searchMemory(ctx, accountID, dc.ProjectID,
		strings.TrimSpace(dc.Title+"\n"+dc.Spec), memoryCandidates)
	if err != nil {
		return nil, err
	}

	pkg := SelectPackage(budget, Candidates{
		Rules:    rules,
		Findings: dc.Findings,
		Index:    index,
		Memories: memories,
	})
	pkg.DemandID = demandID

	// The assembly's measurement as an EVENT (ADR-0009 §3): with no number, "the
	// package grows with the demand" is an impression. Dropped comes along
	// because "it fit" and "it fit because we threw away half the memory" are
	// different facts.
	if err := s.repo.RecordContextBuild(ctx, accountID, demandID, PackageMetrics{
		At:              s.clock.Now(),
		EstimatedTokens: pkg.EstimatedTokens,
		Budget:          pkg.Budget,
		Rules:           len(pkg.Rules),
		Findings:        len(pkg.Findings),
		Index:           len(pkg.Index),
		Memories:        len(pkg.Memories),
		Dropped:         pkg.Dropped,
	}); err != nil {
		return nil, err
	}
	return &pkg, nil
}

// LibraryFor assembles everything the demand's agent should find already on
// disk when the sandbox comes up.
//
// It is the OPPOSITE selection from BuildContextPackage, and the difference is
// the point. The package is what goes INTO the prompt, so it is selected,
// measured and cut against a budget: every token there is paid for on every
// turn. The library is what sits on the FILESYSTEM, so it is complete: it costs
// nothing until the agent decides to open a file, and deciding is what the
// manifest is for.
//
// Cutting the library the way the package is cut would be the worst of both — a
// truncated documentation base the agent believes is whole.
func (s *Service) LibraryFor(ctx context.Context, demandID string) ([]ports.SandboxFile, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("demand not provided")
	}
	dc, err := s.demands.ContextOf(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	if dc == nil {
		return nil, errs.NotFound("demand")
	}

	rules, err := s.repo.RulesFor(ctx, accountID, dc.ProjectID)
	if err != nil {
		return nil, err
	}
	// The index of the demand's repositories, not the project's forty: a map of
	// a repository the demand does not touch is noise on the shelf.
	index, err := s.repo.IndexFor(ctx, accountID, dc.ProjectID, dc.Repos)
	if err != nil {
		return nil, err
	}
	// The memory goes in WHOLE — no relevance search. On the shelf, relevance is
	// the agent's to judge after reading the manifest; deciding for it here
	// would be repeating the package's cut where it costs nothing to avoid.
	memories, err := s.repo.SearchMemory(ctx, MemoryQuery{
		AccountID: accountID, ProjectID: dc.ProjectID, Limit: libraryMemoryCap,
	})
	if err != nil {
		return nil, err
	}

	docs := make([]Document, 0, len(rules)+len(index)+len(memories)+2)
	// ResolveRules applies the inheritance: the project's rule replaces the
	// account's of the same name. What reaches the shelf is what APPLIES, and
	// the origin says where each one came from.
	for _, a := range ResolveRuleArtifacts(rules) {
		docs = append(docs, documentOf(a, LibraryRules))
	}
	for _, a := range index {
		docs = append(docs, documentOf(a, LibraryIndex))
	}
	for _, m := range memories {
		docs = append(docs, documentOf(m.Artifact, LibraryMemory))
	}

	// The demand's own. The statement is what the person asked for, and it is
	// the one document the agent should never have to go looking for.
	if body := strings.TrimSpace(dc.Spec); body != "" {
		docs = append(docs, Document{
			Section: LibraryDemand, Name: "spec.md",
			Title: dc.Title, Body: body, Origin: "the demand's statement",
		})
	}
	return Library(docs), nil
}

// libraryMemoryCap is the ceiling on memories that reach the shelf. It is not a
// budget — the shelf has none — it is a guard against a project with ten
// thousand findings turning provisioning into a database dump.
const libraryMemoryCap = 500

func documentOf(a Artifact, section string) Document {
	return Document{
		Section: section,
		Name:    FileName(a.Name),
		Title:   a.Name,
		Body:    a.Body,
		Origin:  string(a.Scope.Level),
	}
}

const (
	searchLimitDefault = 10
	searchLimitMax     = 50
)

// SearchMemory is the query the agent makes DURING execution: what did not fit
// in the package comes in through here (ADR-0009 §3).
//
// An empty projectID searches account-scoped memory — the memory that applies to
// every project. The inverse does not exist: one project's memory never appears
// in another's search, nor in another account's.
func (s *Service) SearchMemory(ctx context.Context, projectID, query string, limit int) ([]ScoredArtifact, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, errs.Invalid("memory search with no query")
	}
	switch {
	case limit <= 0:
		limit = searchLimitDefault
	case limit > searchLimitMax:
		limit = searchLimitMax
	}
	return s.searchMemory(ctx, accountID, projectID, query, limit)
}

// searchMemory picks the search path. Semantic when there is an Embedder;
// lexical when there is not — see MemoryQuery.
func (s *Service) searchMemory(ctx context.Context, accountID, projectID, text string, limit int) ([]ScoredArtifact, error) {
	q := MemoryQuery{AccountID: accountID, ProjectID: projectID, Text: text, Limit: limit}
	if s.embedder != nil && strings.TrimSpace(text) != "" {
		vec, err := s.embedder.Embed(ctx, text)
		if err != nil {
			return nil, errs.Wrap(errs.KindUnavailable, err, "failed to vectorize the query")
		}
		// The wrong dimension is not a detail: searching with a different embedder
		// from the one that generated the vectors returns a PLAUSIBLE and wrong
		// result, which is a search's worst failure mode. Better to refuse.
		if len(vec) != EmbeddingDim {
			return nil, errs.Internal(
				"the embedder returned %d dimensions; memory was indexed with %d", len(vec), EmbeddingDim)
		}
		q.Embedding = vec
	}
	return s.repo.SearchMemory(ctx, q)
}

// ReadIndex returns a repository's map. A missing index is NotFound on purpose:
// the agent needs to know there is no map — a stale index is worse than a
// missing one because it lies with confidence (spec §5, R-2), and a silently
// empty index would be the same lie in another shape.
func (s *Service) ReadIndex(ctx context.Context, projectID, repo string) (*Artifact, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, errs.Invalid("project not provided")
	}
	if strings.TrimSpace(repo) == "" {
		return nil, errs.Invalid("repository not provided")
	}
	a, err := s.repo.IndexOf(ctx, accountID, projectID, repo)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errs.NotFound("index of repository %q", repo)
	}
	return a, nil
}

// ListRules returns the rules that APPLY to the project, with the hierarchy's
// inheritance already resolved (account → workspace → project).
func (s *Service) ListRules(ctx context.Context, projectID string) ([]string, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, errs.Invalid("project not provided")
	}
	rules, err := s.repo.RulesFor(ctx, accountID, projectID)
	if err != nil {
		return nil, err
	}
	return ResolveRules(rules), nil
}

// PutInput is a write into the knowledge base. Empty WorkspaceID and ProjectID
// mean ACCOUNT scope — the rule that applies everywhere.
type PutInput struct {
	Kind           Kind
	WorkspaceID    string
	ProjectID      string
	Name           string
	Content        []byte
	Meta           map[string]any
	IdempotencyKey string
}

// PutArtifact writes knowledge — it is the WRITE-BACK side of the cycle
// (ADR-0009 §4): today's finding is tomorrow's demand's context.
//
// The decision that structures the method: where the content lives.
//
//   - small goes to Postgres, because that is where it is indexable (vector and
//     trigram) and readable without a second round trip;
//   - large goes to the ObjectStore through the port, and THE ROW KEEPS ONLY THE
//     REFERENCE. Keeping a 4 MB map in a column would turn every read of the
//     table into a 4 MB read.
//
// The order is storage first, row second — the same logic as the credential: if
// the row fails, an orphan object is left behind (inert, and replaced on the
// next attempt through the derived key); the reverse order would leave the row
// claiming content that does not exist, and the failure would surface far from
// here.
func (s *Service) PutArtifact(ctx context.Context, in PutInput) (*Artifact, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if !ValidKind(in.Kind) {
		return nil, errs.Invalid("unknown knowledge kind: %q", in.Kind)
	}
	name := strings.TrimSpace(in.Name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if len(in.Content) == 0 {
		return nil, errs.Invalid("knowledge artifact with no content")
	}

	scope := Scope{
		Level:       ScopeAccount,
		AccountID:   accountID,
		WorkspaceID: strings.TrimSpace(in.WorkspaceID),
		ProjectID:   strings.TrimSpace(in.ProjectID),
	}
	switch {
	case scope.ProjectID != "":
		scope.Level = ScopeProject
	case scope.WorkspaceID != "":
		scope.Level = ScopeWorkspace
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}

	// A rule is text the agent reads WHOLE, in every package of every project in
	// scope. If it does not fit inline, it is not a rule — it is a document, and
	// a document is memory or index.
	if in.Kind == KindRule && len(in.Content) > InlineMaxBytes {
		return nil, errs.Invalid(
			"the rule exceeds %d bytes; content that size is memory or index, not a rule", InlineMaxBytes)
	}

	a := &Artifact{
		Scope:     scope,
		Kind:      in.Kind,
		Name:      name,
		SizeBytes: len(in.Content),
		EstTokens: EstimateTokens(string(in.Content)),
		Meta:      in.Meta,
		CreatedBy: call.ActorID,
	}

	if len(in.Content) > InlineMaxBytes {
		ref := ObjectRefFor(scope, in.Kind, name)
		if err := s.objects.Put(ctx, ref, in.Content, ArtifactContentType); err != nil {
			return nil, errs.Wrap(errs.KindUnavailable, err, "failed to store the artifact content")
		}
		a.ObjectRef = ref.Bucket + "/" + ref.Key
	} else {
		a.Body = string(in.Content)
	}

	// Only memory is vectorized: rules and index are looked up by identity
	// (repository name, scope), not by similarity. Vectorizing all three would
	// spend embeddings answering a question nobody asks.
	if in.Kind == KindMemory && s.embedder != nil {
		vec, err := s.embedder.Embed(ctx, name+"\n"+embedText(in.Content))
		if err != nil {
			return nil, errs.Wrap(errs.KindUnavailable, err, "failed to vectorize the artifact")
		}
		if len(vec) != EmbeddingDim {
			return nil, errs.Internal(
				"the embedder returned %d dimensions; the column is vector(%d)", len(vec), EmbeddingDim)
		}
		a.Embedding = vec
	}

	// The request's signature covers the IDENTITY and the CONTENT: repeating the
	// key with different content is a conflict, not a repeat (ADR-0017).
	sum := sha256.Sum256(in.Content)
	return s.repo.Put(ctx, a, Idempotency{
		Key: strings.TrimSpace(in.IdempotencyKey),
		RequestHash: idem.Hash(accountID, string(scope.Level), scope.WorkspaceID, scope.ProjectID,
			string(in.Kind), name, hex.EncodeToString(sum[:])),
	})
}

// embedMaxBytes bounds the text sent to the Embedder. A long document is not
// vectorized whole by any useful model, and sending 4 MB to find that out costs
// money and latency. The start of a document is where the summary lives.
const embedMaxBytes = 8 * 1024

func embedText(content []byte) string {
	if len(content) > embedMaxBytes {
		return string(content[:embedMaxBytes])
	}
	return string(content)
}
