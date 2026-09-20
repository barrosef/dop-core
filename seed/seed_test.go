package seed

import (
	"io/fs"
	"strings"
	"testing"
)

// A seed is idempotent BY CONTRACT (ADR-0024 §3). The contract cannot be
// proven statically, but its most common violation can be caught: an INSERT
// with no ON CONFLICT clause runs once and duplicates forever after.
func TestEverySeedInsertHandlesConflict(t *testing.T) {
	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".sql") {
			return err
		}
		body, err := fs.ReadFile(FS, p)
		if err != nil {
			return err
		}
		s := strings.ToUpper(string(body))
		inserts := strings.Count(s, "INSERT INTO")
		conflicts := strings.Count(s, "ON CONFLICT")
		if inserts > conflicts {
			t.Errorf("%s: %d INSERT(s) but only %d ON CONFLICT — a seed must be safe to re-run", p, inserts, conflicts)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
