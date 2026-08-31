package contract_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fronteira que sustenta a arquitetura limpa: internal/domain não pode
// importar internal/adapter, nem SDK de fornecedor.
//
// Isto é um TESTE, não uma convenção no README — é o que faz a fronteira
// sobreviver ao tempo. Quem tentar violar quebra o build.
func TestDominioNaoImportaInfra(t *testing.T) {
	root := repoRoot(t)
	domainDir := filepath.Join(root, "internal", "domain")

	proibido := []string{
		"/internal/adapter",              // a regra central
		"google.golang.org/grpc",         // protocolo é da borda
		"github.com/jackc/pgx",           // banco é adaptador
		"github.com/nats-io",             // broker é adaptador
		"cloud.google.com/go",            // SDK de fornecedor
		"k8s.io/client-go",               // idem
	}

	var violacoes []string
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
			for _, banido := range proibido {
				if strings.Contains(p, banido) {
					violacoes = append(violacoes,
						rel+" importa "+p+" (proibido: "+banido+")")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("varredura falhou: %v", err)
	}

	if len(violacoes) > 0 {
		t.Errorf("a fronteira domínio↔infra foi violada em %d ponto(s):", len(violacoes))
		for _, v := range violacoes {
			t.Errorf("  • %s", v)
		}
		t.Error("\ninternal/domain declara PORTAS; adaptadores vivem em internal/adapter " +
			"e são escolhidos no composition root (internal/app). Ver ADR-0001.")
	}
}

// O composition root é o ÚNICO lugar autorizado a conhecer as duas pontas.
func TestApenasAppConheceAdaptadores(t *testing.T) {
	root := repoRoot(t)
	permitido := map[string]bool{
		"internal/app":  true,
		"test/contract": true,
		"cmd/dop-core":  true,
	}

	var violacoes []string
	_ = filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		dir := filepath.Dir(rel)
		if permitido[dir] || strings.HasPrefix(dir, "internal/adapter") {
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
				violacoes = append(violacoes, rel+" importa "+p)
			}
		}
		return nil
	})

	for _, v := range violacoes {
		t.Errorf("adaptador importado fora do composition root: %s", v)
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
	t.Fatal("go.mod não encontrado")
	return ""
}
