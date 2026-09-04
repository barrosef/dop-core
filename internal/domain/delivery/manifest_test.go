package delivery_test

import (
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

const good = `
dependencies:
  - name: db
    image: postgres:16
    port: 5432
    env: { POSTGRES_PASSWORD: test }
build: "go build ./..."
start: { command: "./bin/api", port: 8080 }
checks:
  - { kind: aaa, command: "go test ./..." }
  - { kind: integration, command: "go test -tags=integration ./..." }
cache: ["/root/.cache/go-build"]
`

func TestTheManifestIsWhatTheProjectDeclares(t *testing.T) {
	t.Run("a project with no manifest is refused BY NAME", func(t *testing.T) {
		_, err := delivery.ParseManifest(nil, false)
		if errs.KindOf(err) != errs.KindPrecondition {
			t.Fatalf("expected a precondition, got %v", err)
		}
		// The refusal has to say which file to write. "Precondition failed" with
		// no file name sends the reader to the documentation, or to us.
		if !strings.Contains(err.Error(), delivery.ManifestPath) {
			t.Fatalf("the refusal does not name the file: %v", err)
		}
	})

	t.Run("a complete manifest parses and maps the kinds", func(t *testing.T) {
		m, err := delivery.ParseManifest([]byte(good), true)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Checks[0].KindOf(); got != delivery.CheckUnit {
			t.Fatalf("aaa has to become unit, it became %q", got)
		}
		if got := m.Checks[1].KindOf(); got != delivery.CheckIntegration {
			t.Fatalf("integration has to survive as integration, it became %q", got)
		}
		if !m.SupportsSession() {
			t.Fatal("a project that declares start can host a dev session")
		}
	})

	t.Run("a dependency that declares build is REFUSED, not ignored", func(t *testing.T) {
		// The field exists on the struct only so this refusal can happen. Without
		// it the parser would drop the key silently and the author would spend an
		// afternoon wondering why their change had no effect.
		_, err := delivery.ParseManifest([]byte(`
dependencies:
  - name: db
    image: postgres:16
    port: 5432
    build: "docker build ."
start: { command: "./api", port: 8080 }
`), true)
		if errs.KindOf(err) != errs.KindInvalid || !strings.Contains(err.Error(), "published image") {
			t.Fatalf("a dependency with build has to be refused saying why: %v", err)
		}
	})

	t.Run("an unknown key is a typo, and a typo is not silently accepted", func(t *testing.T) {
		_, err := delivery.ParseManifest([]byte(`
start: { command: "./api", port: 8080 }
chekcs:
  - { kind: aaa, command: "go test ./..." }
`), true)
		if errs.KindOf(err) != errs.KindInvalid {
			t.Fatalf("a misspelt key has to be refused: %v", err)
		}
	})

	t.Run("what a run cannot work without is refused", func(t *testing.T) {
		for name, yml := range map[string]string{
			"nothing to run":                              ``,
			"a dependency with no port":                   "dependencies:\n  - {name: db, image: postgres:16}\nstart: {command: ./api, port: 8080}\n",
			"a dependency named for a human, not for DNS": "dependencies:\n  - {name: \"My DB\", image: postgres:16, port: 5432}\nstart: {command: ./api, port: 8080}\n",
			"two dependencies with one name":              "dependencies:\n  - {name: db, image: postgres:16, port: 5432}\n  - {name: db, image: redis:7, port: 6379}\nstart: {command: ./api, port: 8080}\n",
			"a start with no port":                        "start: { command: \"./api\" }\n",
			"an unknown check kind":                       "start: {command: ./api, port: 8080}\nchecks:\n  - {kind: smoke, command: \"true\"}\n",
			"a check with no command":                     "start: {command: ./api, port: 8080}\nchecks:\n  - {kind: aaa, command: \"\"}\n",
			"a relative cache path":                       "start: {command: ./api, port: 8080}\ncache: [\".cache\"]\n",
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := delivery.ParseManifest([]byte(yml), true); errs.KindOf(err) != errs.KindInvalid {
					t.Fatalf("expected a refusal, got %v", err)
				}
			})
		}
	})

	t.Run("a project with only checks needs no application", func(t *testing.T) {
		m, err := delivery.ParseManifest([]byte(
			"checks:\n  - {kind: aaa, command: \"go test ./...\"}\n"), true)
		if err != nil {
			t.Fatalf("a project with only unit tests is legitimate: %v", err)
		}
		if m.SupportsSession() {
			t.Fatal("with no application there is nothing to click through: a dev session has to be refused")
		}
	})
}
