package app

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/barrosef/dop-core/internal/adapter/postgres"
	"github.com/barrosef/dop-core/internal/platform/config"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/logging"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunMigrate is the `migrate` mode (ADR-0024 §2): `up`, `status`, and the
// one-time `baseline <version>` for a database migrated by hand before the
// runner existed. It is what an operator runs; the worker runs `up` itself.
func RunMigrate(ctx context.Context, cfg *config.Config, args []string) error {
	log := logging.From(ctx)
	schema := postgres.NewSchema(cfg.DatabaseURL)
	cmd := "up"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "up":
		if err := schema.Up(ctx); err != nil {
			return err
		}
		v, err := schema.Current(ctx)
		if err != nil {
			return err
		}
		log.Info("migrations applied", "version", v)
		return nil
	case "status":
		list, err := schema.Status(ctx)
		if err != nil {
			return err
		}
		for _, m := range list {
			fmt.Fprintln(os.Stdout, m.String())
		}
		return nil
	case "baseline":
		if len(args) < 2 {
			return errs.Invalid("usage: dop-core migrate baseline <version>")
		}
		v, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || v <= 0 {
			return errs.Invalid("baseline needs a positive version, got %q", args[1])
		}
		if err := schema.Baseline(ctx, v); err != nil {
			return err
		}
		log.Info("baseline recorded", "version", v)
		return nil
	}
	return errs.Invalid("unknown migrate command %q (use up, status, baseline <version>)", cmd)
}

// RunSeed is the `seed` mode (ADR-0024 §3): the root seeds and the profile's,
// applied in file order. The worker runs it after migrating; an operator runs
// it to re-apply an edited seed without a restart.
func RunSeed(ctx context.Context, cfg *config.Config) error {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to open the Postgres pool")
	}
	defer pool.Close()
	return postgres.Seed(ctx, pool, cfg.SeedProfile)
}
