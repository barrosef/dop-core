package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver goose drives
	"github.com/pressly/goose/v3"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/logging"
	"github.com/barrosef/dop-core/migrations"
	"github.com/barrosef/dop-core/seed"
)

// The schema's lifecycle (ADR-0024 §2–3): the binary applies its own
// migrations and seeds, and records what it applied. goose is the library
// behind it; its vocabulary (goose_db_version, the Up/Down markers) stays in
// this file.

// Schema is the migration and seed runner over ONE database.
type Schema struct {
	url string
}

func NewSchema(databaseURL string) *Schema { return &Schema{url: databaseURL} }

func (s *Schema) open() (*sql.DB, error) {
	db, err := goose.OpenDBWithDriver("pgx", s.url)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to open the database for migrations")
	}
	return db, nil
}

func init() {
	goose.SetBaseFS(migrations.FS)
	goose.SetTableName("goose_db_version")
	goose.SetLogger(goose.NopLogger())
}

// Up applies every embedded migration the database has not seen.
func (s *Schema) Up(ctx context.Context) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return errs.Wrap(errs.KindInternal, err, "migration failed")
	}
	return nil
}

// Latest is the highest embedded version — what the binary knows.
func Latest() (int64, error) {
	ms, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		return 0, err
	}
	if len(ms) == 0 {
		return 0, nil
	}
	return ms[len(ms)-1].Version, nil
}

// Current is the database's version, 0 when the version table does not exist
// yet (a database migrated by hand, before ADR-0024 — see Baseline).
func (s *Schema) Current(ctx context.Context) (int64, error) {
	db, err := s.open()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'goose_db_version')`).
		Scan(&exists); err != nil {
		return 0, errs.Wrap(errs.KindUnavailable, err, "failed to read the schema version")
	}
	if !exists {
		return 0, nil
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return 0, errs.Wrap(errs.KindUnavailable, err, "failed to read the schema version")
	}
	return v, nil
}

// Status lists every embedded migration with whether the database applied it.
func (s *Schema) Status(ctx context.Context) ([]MigrationStatus, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ms, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		return nil, err
	}
	applied := map[int64]bool{}
	if v, _ := s.Current(ctx); v > 0 {
		rows, err := db.QueryContext(ctx, `SELECT version_id FROM goose_db_version WHERE is_applied`)
		if err != nil {
			return nil, errs.Wrap(errs.KindUnavailable, err, "failed to read applied migrations")
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			applied[id] = true
		}
	}
	out := make([]MigrationStatus, 0, len(ms))
	for _, m := range ms {
		out = append(out, MigrationStatus{Version: m.Version, Source: path.Base(m.Source), Applied: applied[m.Version]})
	}
	return out, nil
}

type MigrationStatus struct {
	Version int64
	Source  string
	Applied bool
}

// Baseline records every embedded migration up to `version` as applied
// WITHOUT running it. It is the one-time bootstrap of a database that was
// migrated by hand before the runner existed, and it refuses to run on a
// database that already has a version table — that one is not a baseline
// case, and pretending would hide a real gap.
func (s *Schema) Baseline(ctx context.Context, version int64) error {
	if cur, err := s.Current(ctx); err != nil {
		return err
	} else if cur > 0 {
		return errs.Precondition("the database already records version %d; baseline is only for a database with no version table", cur)
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	// An EMPTY database is not a baseline case either: marking migrations as
	// applied on a database that never ran them is how a fresh environment
	// ends up with a version and no tables. 0001 creates `users`; its absence
	// means "run up", never "baseline".
	var hasUsers bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'users')`).
		Scan(&hasUsers); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to inspect the database")
	}
	if !hasUsers {
		return errs.Precondition("the database is empty; run `migrate up`, not baseline")
	}
	ms, err := goose.CollectMigrations(".", 0, version)
	if err != nil {
		return err
	}
	// goose creates its table on the first EnsureDBVersion; the row for
	// version 0 is its own marker.
	if _, err := goose.EnsureDBVersionContext(ctx, db); err != nil {
		return errs.Wrap(errs.KindInternal, err, "failed to create the version table")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rolled back only on the error path
	for _, m := range ms {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES ($1, true, $2)`,
			m.Version, time.Now().UTC()); err != nil {
			return errs.Wrap(errs.KindInternal, err, "failed to record version %d", m.Version)
		}
	}
	return tx.Commit()
}

// WaitFor blocks until the database's version reaches the binary's, or the
// deadline passes. `serve` calls it at boot (ADR-0024 §2): a schema older
// than the binary is a deploy that outran its worker, and waiting is the
// honest answer; a schema NEWER than the binary is a rollback that outran
// its database, and that one is refused at once.
func (s *Schema) WaitFor(ctx context.Context, clock ports.Clock, timeout time.Duration) error {
	want, err := Latest()
	if err != nil {
		return err
	}
	log := logging.From(ctx)
	deadline := clock.Now().Add(timeout)
	for {
		have, err := s.Current(ctx)
		if err != nil {
			return err
		}
		switch {
		case have == want:
			return nil
		case have > want:
			return errs.Precondition("the database is at schema %d, newer than this binary's %d", have, want)
		}
		if clock.Now().After(deadline) {
			return errs.Precondition("the database is at schema %d and this binary needs %d; the worker has not migrated", have, want)
		}
		log.Info("waiting for the schema", "have", have, "want", want)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Seed applies the root seeds and, when profile is set, the profile's, each
// in file order. Every seed is one statement batch inside one transaction:
// a seed that fails half way leaves nothing behind.
func Seed(ctx context.Context, pool *pgxpool.Pool, profile string) error {
	files, err := seedFiles(profile)
	if err != nil {
		return err
	}
	log := logging.From(ctx)
	for _, f := range files {
		body, err := fs.ReadFile(seed.FS, f)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(body)) == "" {
			continue
		}
		if err := InTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, string(body))
			return err
		}); err != nil {
			return errs.Wrap(errs.KindInternal, err, "seed %s failed", f)
		}
		log.Info("seed applied", "file", f)
	}
	return nil
}

// seedFiles lists the root *.sql and, for a profile, its <profile>/*.sql —
// sorted, so the order is the file name's and nothing else. An unknown
// profile is a refusal: a typo must not silently load nothing.
func seedFiles(profile string) ([]string, error) {
	var out []string
	root, err := fs.ReadDir(seed.FS, ".")
	if err != nil {
		return nil, err
	}
	for _, e := range root {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	if profile == "" {
		return out, nil
	}
	entries, err := fs.ReadDir(seed.FS, profile)
	if err != nil {
		return nil, errs.Invalid("unknown seed profile %q", profile)
	}
	var prof []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			prof = append(prof, profile+"/"+e.Name())
		}
	}
	sort.Strings(prof)
	return append(out, prof...), nil
}

// String renders a status line for the CLI.
func (m MigrationStatus) String() string {
	mark := "pending"
	if m.Applied {
		mark = "applied"
	}
	return fmt.Sprintf("%05d  %-8s  %s", m.Version, mark, m.Source)
}
