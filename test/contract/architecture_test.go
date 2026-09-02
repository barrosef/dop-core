package contract_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// The frontier that holds the clean architecture up: internal/domain must not
// import internal/adapter, nor any vendor SDK.
//
// This is a TEST, not a convention in the README — it is what makes the frontier
// survive time. Whoever tries to violate it breaks the build.
func TestTheDomainDoesNotImportInfrastructure(t *testing.T) {
	root := repoRoot(t)
	domainDir := filepath.Join(root, "internal", "domain")

	forbidden := []string{
		"/internal/adapter",      // the central rule
		"google.golang.org/grpc", // the protocol belongs to the edge
		"github.com/jackc/pgx",   // the database is an adapter
		"github.com/nats-io",     // the broker is an adapter
		"cloud.google.com/go",    // a vendor SDK
		"k8s.io/client-go",       // likewise
	}

	var violations []string
	err := filepath.Walk(domainDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, banned := range forbidden {
				if strings.Contains(p, banned) {
					violations = append(violations,
						rel+" imports "+p+" (forbidden: "+banned+")")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("the domain↔infrastructure frontier was violated in %d place(s):", len(violations))
		for _, v := range violations {
			t.Errorf("  • %s", v)
		}
		t.Error("\ninternal/domain declares PORTS; adapters live in internal/adapter " +
			"and are chosen in the composition root (internal/app). See ADR-0001.")
	}
}

// The composition root is the ONLY place allowed to know both ends.
func TestOnlyAppKnowsTheAdapters(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{
		"internal/app":  true,
		"test/contract": true,
		"cmd/dop-core":  true,
	}

	var violations []string
	_ = filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		dir := filepath.Dir(rel)
		if allowed[dir] || strings.HasPrefix(dir, "internal/adapter") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, "/internal/adapter/") {
				violations = append(violations, rel+" imports "+p)
			}
		}
		return nil
	})

	for _, v := range violations {
		t.Errorf("adapter imported outside the composition root: %s", v)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found")
	return ""
}

// The agent domain REPEATS the workspace's path instead of importing it from
// `ports` — importing the infrastructure package just for a string would put the
// infra port inside the runtime, and the house rule (verified by the two tests
// above) forbids that.
//
// The repetition is acceptable; the silent DIVERGENCE is not. If the two values
// drift apart, the tool keeps working: the command runs, the exit code comes
// back, no test turns red — and the agent reads in the prompt that the workspace
// is at a path where it is not. It starts looking for files in the wrong place
// and concluding the repository is empty.
//
// This test lives here because this is the only package allowed to see both
// sides (see TestOnlyAppKnowsTheAdapters) — and it is the same mechanics by
// which `agent.Micros` does not import `cost`.
func TestTheWorkspacePathIsTheSameOnBothSides(t *testing.T) {
	if agent.SandboxWorkspaceHint != ports.SandboxWorkspacePath {
		t.Fatalf("the runtime tells the agent the workspace is at %q and the substrate mounts "+
			"it at %q: the agent will look for files where they are not and conclude the "+
			"repository is empty — with nothing failing",
			agent.SandboxWorkspaceHint, ports.SandboxWorkspacePath)
	}
}
