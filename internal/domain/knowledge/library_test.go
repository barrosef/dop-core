package knowledge_test

import (
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
)

func TestTheManifestComesFirstAndIsReadable(t *testing.T) {
	// It is the only file the agent is guaranteed to read. If it is not the
	// first, whoever assembles the mount has to remember to put it first — and
	// remembering is what fails.
	files := knowledge.Library([]knowledge.Document{
		{Section: knowledge.LibraryMemory, Name: "a.md", Title: "A finding", Body: "x"},
		{Section: knowledge.LibraryRules, Name: "b.md", Title: "Never merge develop", Body: "y"},
	})
	if len(files) != 3 {
		t.Fatalf("%d files, want 3", len(files))
	}
	if files[0].Path != knowledge.LibraryManifest {
		t.Fatalf("the first file is %q", files[0].Path)
	}
	// Rules before memory: the order is what matters to whoever is about to
	// work, not the alphabet.
	if files[1].Path != "rules/b.md" || files[2].Path != "memory/a.md" {
		t.Errorf("out of order: %q then %q", files[1].Path, files[2].Path)
	}
	manifest := string(files[0].Content)
	for _, want := range []string{"rules/b.md", "memory/a.md", "Never merge develop"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the manifest does not mention %q", want)
		}
	}
}

func TestTheManifestSaysHowBigEachDocumentIs(t *testing.T) {
	// Choosing what to open is a decision about budget (ADR-0011). Without the
	// number it is a guess.
	files := knowledge.Library([]knowledge.Document{
		{Section: knowledge.LibraryIndex, Name: "api.md", Title: "The API repo",
			Body: strings.Repeat("x", 2048)},
	})
	if !strings.Contains(string(files[0].Content), "2.0 KiB") {
		t.Errorf("the manifest carries no size:\n%s", files[0].Content)
	}
}

func TestTwoDocumentsWithTheSameTitleDoNotOverwriteEachOther(t *testing.T) {
	// A rule of the account and one of the project may share a title. Silently,
	// the second would win and nobody would know which one the agent read.
	files := knowledge.Library([]knowledge.Document{
		{Section: knowledge.LibraryRules, Name: "branches.md", Title: "Branches",
			Body: "the account's", Origin: "account"},
		{Section: knowledge.LibraryRules, Name: "branches.md", Title: "Branches",
			Body: "the project's", Origin: "project"},
	})
	paths := map[string]string{}
	for _, f := range files[1:] {
		if _, dup := paths[f.Path]; dup {
			t.Fatalf("two documents at the same path: %q", f.Path)
		}
		paths[f.Path] = string(f.Content)
	}
	if len(paths) != 2 {
		t.Fatalf("%d documents, want 2", len(paths))
	}
	manifest := string(files[0].Content)
	if !strings.Contains(manifest, "[account]") || !strings.Contains(manifest, "[project]") {
		t.Error("the manifest does not say where each one came from")
	}
}

func TestAFileNameSurvivesTheTitleSomebodyTyped(t *testing.T) {
	// A slash is the one that actually breaks: it would create a directory
	// nobody asked for, and on the mount that is a path that escapes.
	for _, c := range []struct{ title, want string }{
		{"Never merge develop", "never-merge-develop.md"},
		{"CI/CD: the rules", "ci-cd-the-rules.md"},
		{"  Padrões de código  ", "padroes-de-codigo.md"},
		{"../escape", "escape.md"},
		{"🎯", "untitled.md"},
		{"already.md", "already.md"},
	} {
		if got := knowledge.FileName(c.title); got != c.want {
			t.Errorf("FileName(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestAnEmptyLibraryStillSaysSo(t *testing.T) {
	// A manifest with nothing in it and no sentence would leave the agent
	// wondering whether the mount failed.
	files := knowledge.Library(nil)
	if len(files) != 1 {
		t.Fatalf("%d files, want only the manifest", len(files))
	}
	if !strings.Contains(string(files[0].Content), "no documents recorded yet") {
		t.Errorf("the empty manifest says nothing:\n%s", files[0].Content)
	}
}
