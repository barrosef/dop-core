package migrations

import (
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The architecture rules of ADR-0024 §2, as a test: numbers are contiguous
// from 1, every file has an Up and a Down, and nothing is embedded that is
// not a migration.
func TestMigrationsAreContiguousAndReversible(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)
	var versions []int
	for _, e := range entries {
		m := name.FindStringSubmatch(e.Name())
		if m == nil {
			t.Errorf("%s does not look like NNNN_snake_case.sql", e.Name())
			continue
		}
		v, _ := strconv.Atoi(m[1])
		versions = append(versions, v)

		body, err := fs.ReadFile(FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		s := string(body)
		if !strings.Contains(s, "-- +goose Up") {
			t.Errorf("%s has no '-- +goose Up'", e.Name())
		}
		if !strings.Contains(s, "-- +goose Down") {
			t.Errorf("%s has no '-- +goose Down': a migration with no way back is a migration nobody can roll back", e.Name())
		}
		if strings.Index(s, "-- +goose Up") > strings.Index(s, "-- +goose Down") {
			t.Errorf("%s has Down before Up", e.Name())
		}
	}
	sort.Ints(versions)
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("migration numbers are not contiguous: expected %04d, found %04d", i+1, v)
		}
	}
	if len(versions) == 0 {
		t.Fatal("no migrations embedded")
	}
}
