package migrations

import (
	"bufio"
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

// goose splits a migration on ';' and keeps a dollar-quoted body whole only
// when it sits between '-- +goose StatementBegin' and '-- +goose StatementEnd'.
// A '$$' outside such a block applies fine by hand (psql) and breaks the first
// time goose runs the file against an empty database — which is how 0001,
// 0005, 0009 and 0010 hid until a fresh cluster had to migrate from zero.
func TestDollarQuotedBodiesAreInsideStatementBlocks(t *testing.T) {
	files, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations embedded")
	}
	for _, name := range files {
		body, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatal(err)
		}
		inside := false
		sc := bufio.NewScanner(bytes.NewReader(body))
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSpace(sc.Text())
			switch {
			case strings.HasPrefix(line, "-- +goose StatementBegin"):
				if inside {
					t.Errorf("%s:%d: StatementBegin inside an open block", name, n)
				}
				inside = true
			case strings.HasPrefix(line, "-- +goose StatementEnd"):
				if !inside {
					t.Errorf("%s:%d: StatementEnd with no open block", name, n)
				}
				inside = false
			case strings.Contains(line, "$$") && !inside:
				t.Errorf("%s:%d: '$$' outside a '-- +goose StatementBegin/StatementEnd' block: goose will split the body on ';'", name, n)
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		if inside {
			t.Errorf("%s: StatementBegin never closed", name)
		}
	}
}
