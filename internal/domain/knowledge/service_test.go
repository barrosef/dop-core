package knowledge_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The domain is testable WITHOUT a database, WITHOUT storage and WITHOUT an
// embedding service: all three are ports, and in-memory doubles go in here. It
// is the practical return on hexagonal architecture.
//
// The doubles live in THIS file, and not in internal/adapter: the architecture
// test sweeps every .go under internal/domain, the _test.go files included, and
// importing an adapter from here would break the very boundary it protects.

// ── package selection: the pure function that decides what the agent knows ───

func TestRuleInheritance(t *testing.T) {
	rules := []knowledge.Artifact{
		{Scope: knowledge.AccountScope("a1"), Kind: knowledge.KindRule, Name: "branches", Body: "rule da casa"},
		{Scope: knowledge.AccountScope("a1"), Kind: knowledge.KindRule, Name: "segredos", Body: "nada em text puro"},
		{Scope: knowledge.WorkspaceScope("a1", "w1"), Kind: knowledge.KindRule, Name: "testes", Body: "rule do workspace"},
		{Scope: knowledge.ProjectScope("a1", "p1"), Kind: knowledge.KindRule, Name: "branches", Body: "rule do project"},
		{Scope: knowledge.AccountScope("a1"), Kind: knowledge.KindRule, Name: "vazia", Body: "  "},
	}
	got := knowledge.ResolveRules(rules)

	// The more specific WINS over the more general (the project's "branches" rule
	// replaces the account's), what was not replaced still applies, and the
	// list comes out from specific to general — that is what makes the budget cut
	// sacrifice the generic rule first.
	want := []string{"rule do project", "rule do workspace", "nada em text puro"}
	if len(got) != len(want) {
		t.Fatalf("resolved rules = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d = %q, want %q", i, got[i], want[i])
		}
	}
	for _, r := range got {
		if r == "rule da casa" {
			t.Error("the account rule must not coexist with the project rule of the same name — inheritance is replacement")
		}
	}
}

// TestSelectionRespectsTheBudget is this domain's central test: the package is
// SELECTED, not dumped (ADR-0012).
func TestSelectionRespectsTheBudget(t *testing.T) {
	bud := knowledge.Budget{Total: 400, FindingShare: 0.2, IndexShare: 0.3, MemoryShare: 0.3}

	cand := knowledge.Candidates{
		Rules: []knowledge.Artifact{
			{Scope: knowledge.ProjectScope("a1", "p1"), Kind: knowledge.KindRule,
				Name: "branches", Body: text(200)}, // ~50 tokens
		},
		Findings: []knowledge.Finding{
			{ID: "f1", Title: "achado", Summary: text(100)},
			{ID: "f2", Title: "large finding", Summary: text(4000)}, // does not fit the quota
		},
		Index: []knowledge.Artifact{
			{ID: "i2", Kind: knowledge.KindIndex, Name: "beta", Body: text(120), EstTokens: 30},
			{ID: "i1", Kind: knowledge.KindIndex, Name: "alfa", Body: text(120), EstTokens: 30},
		},
		Memories: []knowledge.ScoredArtifact{
			{Score: 0.4, Artifact: knowledge.Artifact{ID: "m2", Kind: knowledge.KindMemory, Name: "b", EstTokens: 60}},
			{Score: 0.9, Artifact: knowledge.Artifact{ID: "m1", Kind: knowledge.KindMemory, Name: "a", EstTokens: 60}},
			{Score: 0.2, Artifact: knowledge.Artifact{ID: "m3", Kind: knowledge.KindMemory, Name: "c", EstTokens: 60}},
		},
	}

	p := knowledge.SelectPackage(bud, cand)

	if p.EstimatedTokens > bud.Total {
		t.Fatalf("the package blew the budget: %d > %d", p.EstimatedTokens, bud.Total)
	}
	if len(p.Rules) != 1 {
		t.Errorf("the project's rule has to come before everything, got %d", len(p.Rules))
	}
	// A large finding does not fit the layer's quota: it stays out, and what stays
	// out is COUNTED — "it fit" and "it fit by throwing half away" are different
	// diferentes.
	if len(p.Findings) != 1 || p.Dropped.Findings != 1 {
		t.Errorf("findings = %d, dropped = %d; want 1 and 1", len(p.Findings), p.Dropped.Findings)
	}
	// The index comes out in a stable name order, not in the order the query
	// devolveu.
	if len(p.Index) != 2 || p.Index[0].Name != "alfa" {
		t.Errorf("index out of stable order: %+v", p.Index)
	}
	// Memory enters by descending relevance and the quota cuts the rest.
	if len(p.Memories) == 0 || p.Memories[0].ID != "m1" {
		t.Fatalf("the most relevant memory should come first: %+v", p.Memories)
	}
	if !p.Truncated() {
		t.Error("the package was cut and did not declare itself truncated")
	}
	// The cut must not skip the relevant item to squeeze in a less relevant one:
	// curation is not a knapsack problem.
	for i, m := range p.Memories {
		if i > 0 && m.ID < p.Memories[i-1].ID {
			t.Errorf("relevance order violated by packing: %+v", p.Memories)
		}
	}
}

// TestSelectionIsDeterministic protege o prefixo cacheado do prompt: mesma
// same input, same output, byte for byte (ADR-0012 §1).
func TestSelectionIsDeterministic(t *testing.T) {
	cand := knowledge.Candidates{
		Memories: []knowledge.ScoredArtifact{
			{Score: 0.5, Artifact: knowledge.Artifact{ID: "m2", Name: "b", EstTokens: 10}},
			{Score: 0.5, Artifact: knowledge.Artifact{ID: "m1", Name: "a", EstTokens: 10}},
		},
	}
	first := knowledge.SelectPackage(knowledge.Budget{}, cand)
	// Entrada embaralhada, empate de score: o desempate por id tem de mandar.
	cand.Memories[0], cand.Memories[1] = cand.Memories[1], cand.Memories[0]
	segundo := knowledge.SelectPackage(knowledge.Budget{}, cand)

	if len(first.Memories) != 2 || first.Memories[0].ID != "m1" {
		t.Fatalf("id tie-break not applied: %+v", first.Memories)
	}
	for i := range first.Memories {
		if first.Memories[i].ID != segundo.Memories[i].ID {
			t.Fatal("two assemblies of the same demand produced different orders")
		}
	}
	if first.EstimatedTokens != segundo.EstimatedTokens {
		t.Error("the package measurement is not deterministic")
	}
}

func TestAZeroBudgetUsesTheDefault(t *testing.T) {
	// A zero budget means "use the default", never "nothing fits": an agent born
	// blind through a configuration mistake is the worst default possible.
	p := knowledge.SelectPackage(knowledge.Budget{}, knowledge.Candidates{
		Rules: []knowledge.Artifact{{Kind: knowledge.KindRule, Name: "r", Body: "vale"}},
	})
	if len(p.Rules) != 1 {
		t.Fatalf("a zero budget should fall back to the default, and the rule was left out")
	}
	if p.Budget != knowledge.DefaultBudget().Total {
		t.Errorf("ceiling = %d, expected the default %d", p.Budget, knowledge.DefaultBudget().Total)
	}
}

// ── package assembly by the service ──────────────────────────────────────────

func TestBuildContextPackageCutsByBudgetAndMeasures(t *testing.T) {
	repo := newFakeRepo()
	repo.add(rule("a1", "p1", "branches", "PR sempre contra release"))
	repo.add(indexOf("a1", "p1", "dop-core", 40))
	repo.add(indexOf("a1", "p1", "dop-app", 40))
	for _, id := range []string{"m1", "m2", "m3"} {
		m := memory("a1", "p1", id, "lesson about a timeout", 80)
		m.ID = id
		repo.add(m)
	}
	demandas := &demandasFalsas{ctx: &knowledge.DemandContext{
		DemandID: "d1", ProjectID: "p1", Title: "corrigir timeout",
		Repos:    []string{"dop-core"},
		Findings: []knowledge.Finding{{ID: "f1", Title: "o pool estoura", Summary: "no pgbouncer"}},
	}}

	// A deliberately tight budget: the cut is what this test observes.
	svc := knowledge.NewService(repo, newFakeStorage(), demandas, nil, relogioFixo{},
		knowledge.Budget{Total: 200, FindingShare: 0.2, IndexShare: 0.3, MemoryShare: 0.3})

	pkg, err := svc.BuildContextPackage(ctxOf("a1"), "d1", knowledge.Budget{})
	if err != nil {
		t.Fatalf("the assembly failed: %v", err)
	}
	if pkg.EstimatedTokens > 200 {
		t.Fatalf("the package blew the cap: %d", pkg.EstimatedTokens)
	}
	// What grows with the DEMAND: only the index of the repository it touches.
	if len(pkg.Index) != 1 || pkg.Index[0].Name != "dop-core" {
		t.Errorf("the index should hold only the demand's repo, got %+v", pkg.Index)
	}
	// Memory is the layer the ceiling cuts first.
	if len(pkg.Memories) >= 3 {
		t.Errorf("with a 200-token ceiling the three memories do not fit: %+v", pkg.Memories)
	}
	if !pkg.Truncated() || pkg.Dropped.Memories == 0 {
		t.Error("the cut happened and was not accounted for")
	}
	// The assembly's measurement is an EVENT, not an impression (ADR-0009 §3).
	if len(repo.measurements) != 1 {
		t.Fatalf("the assembly should emit exactly one measurement, got %d", len(repo.measurements))
	}
	m := repo.measurements[0]
	if m.EstimatedTokens != pkg.EstimatedTokens || m.Budget != 200 || !m.Dropped.Any() {
		t.Errorf("the measurement does not reflect the assembly: %+v", m)
	}
	// The instant comes from the port's CLOCK, not from a hidden time.Now().
	if !m.At.Equal(instanteFixo) {
		t.Errorf("the measurement was stamped outside the port clock: %v", m.At)
	}
}

func TestThePackageRequiresADemandAndAnAccount(t *testing.T) {
	svc := knowledge.NewService(newFakeRepo(), newFakeStorage(),
		&demandasFalsas{}, nil, relogioFixo{}, knowledge.Budget{})

	if _, err := svc.BuildContextPackage(context.Background(), "d1", knowledge.Budget{}); err == nil {
		t.Error("a request with no active account should be refused")
	}
	if _, err := svc.BuildContextPackage(ctxOf("a1"), "  ", knowledge.Budget{}); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("an empty demand should be an invalid argument, got %v", err)
	}
	if _, err := svc.BuildContextPackage(ctxOf("a1"), "d-inexistente", knowledge.Budget{}); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("a nonexistent demand should be not_found, got %v", err)
	}
}

// ── memory search ────────────────────────────────────────────────────────────

// TestMemorySearchIsolatesByAccount is the test that cannot be missing:
// knowledge leaked between accounts is the worst conceivable defect here.
func TestMemorySearchIsolatesByAccount(t *testing.T) {
	repo := newFakeRepo()
	repo.add(memory("a1", "p1", "nossa", "timeout no pgbouncer", 10))
	repo.add(memory("a2", "p9", "da-outra-account", "timeout no pgbouncer", 10))

	svc := knowledge.NewService(repo, newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})

	hits, err := svc.SearchMemory(ctxOf("a1"), "p1", "timeout no pgbouncer", 10)
	if err != nil {
		t.Fatalf("the search failed: %v", err)
	}
	for _, h := range hits {
		if h.Artifact.AccountID() != "a1" {
			t.Fatalf("memory of account %q leaked into account a1", h.Artifact.AccountID())
		}
	}
	if len(hits) != 1 {
		t.Fatalf("expected only this account's memory, got %d", len(hits))
	}
	// The query's account comes from the CONTEXT, never from the caller: a service
	// that accepted the account as a parameter would pass this scenario and fail in
	// mundo real.
	if repo.lastSearch.AccountID != "a1" {
		t.Errorf("the search was issued with account %q", repo.lastSearch.AccountID)
	}
}

func TestSemanticSearchOnlyWhenThereIsAnEmbedder(t *testing.T) {
	repo := newFakeRepo()
	repo.add(memory("a1", "p1", "lesson", "the pool kept overflowing", 10))

	// With no Embedder: the LEXICAL path. Not the intended one, but returning
	// nothing would be worse — the agent would start from zero.
	semEmbedder := knowledge.NewService(repo, newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})
	if _, err := semEmbedder.SearchMemory(ctxOf("a1"), "p1", "pool", 5); err != nil {
		t.Fatalf("the lexical search failed: %v", err)
	}
	if len(repo.lastSearch.Embedding) != 0 {
		t.Error("with no Embedder the query must not carry a vector")
	}

	comEmbedder := knowledge.NewService(repo, newFakeStorage(), &demandasFalsas{},
		fakeEmbedder{dim: knowledge.EmbeddingDim}, relogioFixo{}, knowledge.Budget{})
	if _, err := comEmbedder.SearchMemory(ctxOf("a1"), "p1", "pool", 5); err != nil {
		t.Fatalf("semantic search failed: %v", err)
	}
	if len(repo.lastSearch.Embedding) != knowledge.EmbeddingDim {
		t.Errorf("the query should carry the vector, got %d dimensions", len(repo.lastSearch.Embedding))
	}
	if repo.lastSearch.Text == "" {
		t.Error("the text travels with the vector: the adapter needs both paths")
	}
}

// Searching with an embedder different from the one that produced the vectors
// returns a result that is PLAUSIBLE and wrong — a search's worst failure mode. Better to refuse.
func TestAnEmbedderWithTheWrongDimensionIsRefused(t *testing.T) {
	repo := newFakeRepo()
	svc := knowledge.NewService(repo, newFakeStorage(), &demandasFalsas{},
		fakeEmbedder{dim: 768}, relogioFixo{}, knowledge.Budget{})
	if _, err := svc.SearchMemory(ctxOf("a1"), "p1", "pool", 5); err == nil {
		t.Fatal("a vector of incompatible dimension should be refused")
	}
}

func TestASearchWithNoQueryIsRefused(t *testing.T) {
	svc := knowledge.NewService(newFakeRepo(), newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})
	if _, err := svc.SearchMemory(ctxOf("a1"), "p1", "   ", 5); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("an empty search should be an invalid argument, got %v", err)
	}
	if _, err := svc.SearchMemory(context.Background(), "p1", "x", 5); err == nil {
		t.Error("a search with no active account should be refused")
	}
}

// ── writes: where the content lives ──────────────────────────────────────────

// TestALargeArtifactGoesToTheObjectStore proves the split that keeps the table's
// read cost down: the row keeps the REFERENCE, never the bytes.
func TestALargeArtifactGoesToTheObjectStore(t *testing.T) {
	repo, storage := newFakeRepo(), newFakeStorage()
	svc := knowledge.NewService(repo, storage, &demandasFalsas{}, nil, relogioFixo{}, knowledge.Budget{})

	grande := []byte(text(knowledge.InlineMaxBytes + 1))
	a, err := svc.PutArtifact(ctxOf("a1"), knowledge.PutInput{
		Kind: knowledge.KindIndex, ProjectID: "p1", Name: "dop-core", Content: grande,
	})
	if err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	if a.Body != "" {
		t.Error("the row stored the content: a megabyte map in the column makes EVERY read of the table expensive")
	}
	if !a.Externalized() || a.ObjectRef == "" {
		t.Fatal("the row should store the ObjectStore reference")
	}
	if a.SizeBytes != len(grande) {
		t.Errorf("size stored = %d, want %d", a.SizeBytes, len(grande))
	}
	if len(storage.puts) != 1 {
		t.Fatalf("the content should have gone to storage, there were %d writes", len(storage.puts))
	}
	put := storage.puts[0]
	if string(put.conteudo) != string(grande) {
		t.Error("storage received content different from what was sent")
	}
	// The local environment's Storage emulator HANGS on application/json
	// (P-13). This test exists so that changing the type does not slip through.
	if put.contentType != knowledge.ArtifactContentType {
		t.Errorf("Content-Type = %q, want %q", put.contentType, knowledge.ArtifactContentType)
	}
	if strings.HasPrefix(put.contentType, "application/json") {
		t.Error("application/json trava o emulador local sem mensagem de err — ver P-13")
	}
	// The key carries the account: one account's reference never coincides with
	// another's, not even for the same artifact.
	if !strings.HasPrefix(put.ref.Key, "a1/") {
		t.Errorf("a key with no account in the prefix: %q", put.ref.Key)
	}

	// A small artifact takes the opposite path: it stays in the row, indexable.
	pequeno, err := svc.PutArtifact(ctxOf("a1"), knowledge.PutInput{
		Kind: knowledge.KindMemory, ProjectID: "p1", Name: "lesson", Content: []byte("the pool kept overflowing"),
	})
	if err != nil {
		t.Fatalf("the small write failed: %v", err)
	}
	if pequeno.Body == "" || pequeno.Externalized() {
		t.Error("a small artifact should stay in the row, where it is indexable")
	}
	if len(storage.puts) != 1 {
		t.Error("a small artifact must not go to storage")
	}
}

func TestALargeRuleIsRefused(t *testing.T) {
	svc := knowledge.NewService(newFakeRepo(), newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})
	// A rule is text the agent reads WHOLE in every package: if it does not fit
	// inline, it is not a rule.
	_, err := svc.PutArtifact(ctxOf("a1"), knowledge.PutInput{
		Kind: knowledge.KindRule, ProjectID: "p1", Name: "manual",
		Content: []byte(text(knowledge.InlineMaxBytes + 1)),
	})
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("a giant rule should be refused, got %v", err)
	}
}

func TestAWriteCarriesAnIdempotencyKey(t *testing.T) {
	repo := newFakeRepo()
	svc := knowledge.NewService(repo, newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})

	in := knowledge.PutInput{Kind: knowledge.KindMemory, ProjectID: "p1",
		Name: "lesson", Content: []byte("finding"), IdempotencyKey: "k-1"}
	if _, err := svc.PutArtifact(ctxOf("a1"), in); err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	if len(repo.idems) != 1 || repo.idems[0].Key != "k-1" {
		t.Fatalf("the key did not reach the repository: %+v", repo.idems)
	}
	first := repo.idems[0].RequestHash
	if first == "" {
		t.Fatal("the key travels with the content signature, otherwise a repeat and corruption become the same thing")
	}
	// The same content means the same signature; different content means a
	// different signature, and that is what the database turns into a conflict.
	if _, err := svc.PutArtifact(ctxOf("a1"), in); err != nil {
		t.Fatalf("the repeat failed: %v", err)
	}
	if repo.idems[1].RequestHash != first {
		t.Error("a mesma escrita produziu assinaturas diferentes")
	}
	in.Content = []byte("outro achado")
	if _, err := svc.PutArtifact(ctxOf("a1"), in); err != nil {
		t.Fatalf("the altered write failed: %v", err)
	}
	if repo.idems[2].RequestHash == first {
		t.Error("different content under the same key should change the signature")
	}
}

func TestAWriteValidatesKindScopeAndContent(t *testing.T) {
	svc := knowledge.NewService(newFakeRepo(), newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})
	casos := map[string]knowledge.PutInput{
		"tipo desconhecido": {Kind: "diagrama", Name: "x", Content: []byte("c")},
		"sem name":          {Kind: knowledge.KindMemory, Name: "  ", Content: []byte("c")},
		"no content":        {Kind: knowledge.KindMemory, Name: "x"},
	}
	for name, in := range casos {
		if _, err := svc.PutArtifact(ctxOf("a1"), in); errs.KindOf(err) != errs.KindInvalid {
			t.Errorf("%s should be an invalid argument, got %v", name, err)
		}
	}
	if _, err := svc.PutArtifact(context.Background(), knowledge.PutInput{
		Kind: knowledge.KindMemory, Name: "x", Content: []byte("c")}); err == nil {
		t.Error("a write with no active account should be refused")
	}
}

func TestScopeIsDerivedFromWhatArrived(t *testing.T) {
	repo := newFakeRepo()
	svc := knowledge.NewService(repo, newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})

	noProject, err := svc.PutArtifact(ctxOf("a1"), knowledge.PutInput{
		Kind: knowledge.KindRule, Name: "branches", Content: []byte("rule da casa")})
	if err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	if noProject.Scope.Level != knowledge.ScopeAccount {
		t.Errorf("with no project the scope is the account's, got %q", noProject.Scope.Level)
	}
	comProjeto, err := svc.PutArtifact(ctxOf("a1"), knowledge.PutInput{
		Kind: knowledge.KindRule, ProjectID: "p1", Name: "branches", Content: []byte("rule do project")})
	if err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	if comProjeto.Scope.Level != knowledge.ScopeProject || comProjeto.Scope.ProjectID != "p1" {
		t.Errorf("scopeOf for a project derived wrongly: %+v", comProjeto.Scope)
	}
	// The account ALWAYS comes from the context — never from what the caller sent.
	if noProject.AccountID() != "a1" || comProjeto.AccountID() != "a1" {
		t.Error("a account do artifact tem de vir do context da chamada")
	}
}

// ── reading the index and the rules ──────────────────────────────────────────

func TestReadIndexAndListRules(t *testing.T) {
	repo := newFakeRepo()
	repo.add(indexOf("a1", "p1", "dop-core", 30))
	repo.add(rule("a1", "", "branches", "rule da casa"))
	repo.add(rule("a1", "p1", "branches", "rule do project"))
	repo.add(rule("a2", "p9", "branches", "rule de outra account"))

	svc := knowledge.NewService(repo, newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})

	a, err := svc.ReadIndex(ctxOf("a1"), "p1", "dop-core")
	if err != nil || a == nil {
		t.Fatalf("the repository index should be found: %v", err)
	}
	// A missing index is NotFound: an index that lies with confidence is worse
	// than one that does not exist (R-2), and a silent empty is the same lie.
	if _, err := svc.ReadIndex(ctxOf("a1"), "p1", "dop-app"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("a missing index should be not_found, got %v", err)
	}
	// Isolation on the read side too.
	if _, err := svc.ReadIndex(ctxOf("a2"), "p1", "dop-core"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("another account's index must not be readable, got %v", err)
	}

	rules, err := svc.ListRules(ctxOf("a1"), "p1")
	if err != nil {
		t.Fatalf("listing the rules failed: %v", err)
	}
	if len(rules) != 1 || rules[0] != "rule do project" {
		t.Errorf("inheritance resolved wrongly: %v", rules)
	}
	if _, err := svc.ListRules(ctxOf("a1"), ""); errs.KindOf(err) != errs.KindInvalid {
		t.Error("rules de um project exigem o project")
	}
}

// ── service assembly ─────────────────────────────────────────────────────────

// A WIRING error shows up at boot, not at three in the morning.
func TestTheServiceRefusesMissingRequiredPorts(t *testing.T) {
	casos := map[string]func(){
		"no repository": func() {
			knowledge.NewService(nil, newFakeStorage(), &demandasFalsas{}, nil, relogioFixo{}, knowledge.Budget{})
		},
		"sem ObjectStore": func() {
			knowledge.NewService(newFakeRepo(), nil, &demandasFalsas{}, nil, relogioFixo{}, knowledge.Budget{})
		},
		"sem demandas": func() {
			knowledge.NewService(newFakeRepo(), newFakeStorage(), nil, nil, relogioFixo{}, knowledge.Budget{})
		},
		"no clock": func() {
			knowledge.NewService(newFakeRepo(), newFakeStorage(), &demandasFalsas{}, nil, nil, knowledge.Budget{})
		},
	}
	for name, build := range casos {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("assembling the service %s should fail at boot", name)
				}
			}()
			build()
		})
	}
	// The Embedder is the ONLY optional one: with no embedding service the search
	// falls back to lexical, and the rest of the domain stays standing.
	if svc := knowledge.NewService(newFakeRepo(), newFakeStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{}); svc == nil {
		t.Error("a nil Embedder is acceptable and documented")
	}
}

// ═════════════════════════ duplos de teste ═══════════════════════════════════

var instanteFixo = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

type relogioFixo struct{}

func (relogioFixo) Now() time.Time { return instanteFixo }

func ctxOf(accountID string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: accountID, ActorID: "u1", ActorKind: ctxutil.ActorAgent})
}

// text produces content of a known size — the budget is measured in bytes.
func text(n int) string { return strings.Repeat("a", n) }

func rule(account, project, name, body string) knowledge.Artifact {
	return knowledge.Artifact{
		Scope: scopeOf(account, project), Kind: knowledge.KindRule,
		Name: name, Body: body, EstTokens: knowledge.EstimateTokens(body),
	}
}

func indexOf(account, project, repo string, tokens int) knowledge.Artifact {
	return knowledge.Artifact{
		Scope: scopeOf(account, project), Kind: knowledge.KindIndex,
		Name: repo, Body: text(tokens * 4), EstTokens: tokens,
	}
}

func memory(account, project, name, body string, tokens int) knowledge.Artifact {
	return knowledge.Artifact{
		Scope: scopeOf(account, project), Kind: knowledge.KindMemory,
		Name: name, Body: body, EstTokens: tokens,
	}
}

func scopeOf(account, project string) knowledge.Scope {
	if project == "" {
		return knowledge.AccountScope(account)
	}
	return knowledge.ProjectScope(account, project)
}

// fakeRepo reproduces what the SQL does: it filters by account ALWAYS, resolves
// scopeOf's reach and returns the candidates. Without that, the isolation test
// would be testing the double, not the service.
type fakeRepo struct {
	arts         []knowledge.Artifact
	idems        []knowledge.Idempotency
	measurements []knowledge.PackageMetrics
	lastSearch   knowledge.MemoryQuery
	seq          int
}

func newFakeRepo() *fakeRepo { return &fakeRepo{} }

func (r *fakeRepo) add(a knowledge.Artifact) {
	r.seq++
	if a.ID == "" {
		a.ID = "art-" + string(rune('a'+r.seq))
	}
	a.Version = 1
	r.arts = append(r.arts, a)
}

func (r *fakeRepo) alcanca(a knowledge.Artifact, accountID, projectID string) bool {
	if a.Scope.AccountID != accountID {
		return false
	}
	switch a.Scope.Level {
	case knowledge.ScopeAccount:
		return true
	case knowledge.ScopeProject:
		return projectID != "" && a.Scope.ProjectID == projectID
	}
	return false
}

func (r *fakeRepo) Put(_ context.Context, a *knowledge.Artifact, id knowledge.Idempotency) (*knowledge.Artifact, error) {
	r.idems = append(r.idems, id)
	r.seq++
	saved := *a
	saved.ID = "newOne-" + string(rune('a'+r.seq))
	saved.Version = 1
	saved.CreatedAt, saved.UpdatedAt = instanteFixo, instanteFixo
	r.arts = append(r.arts, saved)
	return &saved, nil
}

func (r *fakeRepo) IndexOf(_ context.Context, accountID, projectID, repo string) (*knowledge.Artifact, error) {
	for i := range r.arts {
		a := r.arts[i]
		if a.Kind == knowledge.KindIndex && a.Name == repo && r.alcanca(a, accountID, projectID) {
			return &a, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) IndexFor(_ context.Context, accountID, projectID string, repos []string) ([]knowledge.Artifact, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	querido := map[string]bool{}
	for _, n := range repos {
		querido[n] = true
	}
	var out []knowledge.Artifact
	for i := range r.arts {
		a := r.arts[i]
		if a.Kind == knowledge.KindIndex && querido[a.Name] && r.alcanca(a, accountID, projectID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *fakeRepo) RulesFor(_ context.Context, accountID, projectID string) ([]knowledge.Artifact, error) {
	var out []knowledge.Artifact
	for i := range r.arts {
		a := r.arts[i]
		if a.Kind == knowledge.KindRule && r.alcanca(a, accountID, projectID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *fakeRepo) SearchMemory(_ context.Context, q knowledge.MemoryQuery) ([]knowledge.ScoredArtifact, error) {
	r.lastSearch = q
	var out []knowledge.ScoredArtifact
	for i := range r.arts {
		a := r.arts[i]
		if a.Kind != knowledge.KindMemory || !r.alcanca(a, q.AccountID, q.ProjectID) {
			continue
		}
		out = append(out, knowledge.ScoredArtifact{Artifact: a, Score: 1 / float32(len(out)+1)})
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (r *fakeRepo) RecordContextBuild(_ context.Context, _, _ string, m knowledge.PackageMetrics) error {
	r.measurements = append(r.measurements, m)
	return nil
}

// fakeStorage satisfies ports.ObjectStore and records what it received —
// including
// the Content-Type, which is the detail that freezes the local environment if it changes (P-13).
type fakeStorage struct {
	puts []escritaNoStorage
	obj  map[string][]byte
}

type escritaNoStorage struct {
	ref         ports.ObjectRef
	conteudo    []byte
	contentType string
}

func newFakeStorage() *fakeStorage { return &fakeStorage{obj: map[string][]byte{}} }

func (s *fakeStorage) Put(_ context.Context, ref ports.ObjectRef, content []byte, ct string) error {
	s.puts = append(s.puts, escritaNoStorage{ref: ref, conteudo: content, contentType: ct})
	s.obj[ref.Bucket+"/"+ref.Key] = content
	return nil
}

func (s *fakeStorage) Get(_ context.Context, ref ports.ObjectRef) ([]byte, error) {
	c, ok := s.obj[ref.Bucket+"/"+ref.Key]
	if !ok {
		return nil, errs.NotFound("objeto")
	}
	return c, nil
}

func (s *fakeStorage) Delete(_ context.Context, ref ports.ObjectRef) error {
	delete(s.obj, ref.Bucket+"/"+ref.Key)
	return nil
}

func (s *fakeStorage) Stat(_ context.Context, ref ports.ObjectRef) (*ports.ObjectMeta, error) {
	c, ok := s.obj[ref.Bucket+"/"+ref.Key]
	if !ok {
		return nil, errs.NotFound("objeto")
	}
	return &ports.ObjectMeta{Size: int64(len(c)), ContentType: knowledge.ArtifactContentType, UpdatedAt: instanteFixo}, nil
}

func (s *fakeStorage) SignedPutURL(context.Context, ports.ObjectRef, time.Duration) (string, error) {
	return "", errs.New(errs.KindUnavailable, "sem assinatura no double")
}

func (s *fakeStorage) SignedGetURL(context.Context, ports.ObjectRef, time.Duration) (string, error) {
	return "", errs.New(errs.KindUnavailable, "sem assinatura no double")
}

// fakeEmbedder is DETERMINISTIC: the same text produces the same vector, always.
// There is no external call in a domain test — and a random vector would make
// the ordering of the results irreproducible.
type fakeEmbedder struct{ dim int }

func (e fakeEmbedder) Dimensions() int { return e.dim }

func (e fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	sum := sha256.Sum256([]byte(text))
	v := make([]float32, e.dim)
	for i := range v {
		v[i] = float32(sum[i%len(sum)]) / 255
	}
	return v, nil
}

type demandasFalsas struct{ ctx *knowledge.DemandContext }

func (d *demandasFalsas) ContextOf(_ context.Context, _, demandID string) (*knowledge.DemandContext, error) {
	if d.ctx == nil || d.ctx.DemandID != demandID {
		return nil, nil
	}
	return d.ctx, nil
}
