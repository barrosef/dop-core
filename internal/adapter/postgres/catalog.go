package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/domain/catalog"
)

// CatalogRepo implements catalog.Repository over plan_catalog and
// provider_catalog. Both are read-only from the process: the rows arrive with
// the migrations (0027) and change with the next one.
type CatalogRepo struct{ pool *pgxpool.Pool }

func NewCatalogRepo(pool *pgxpool.Pool) *CatalogRepo { return &CatalogRepo{pool: pool} }

const planCols = `key, name, tagline, features, sort, active`

func scanPlan(row pgx.Row) (*catalog.Plan, error) {
	var p catalog.Plan
	var features []byte
	if err := row.Scan(&p.Key, &p.Name, &p.Tagline, &features, &p.Sort, &p.Active); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(features, &p.Features); err != nil {
		return nil, err
	}
	if p.Features == nil {
		p.Features = []string{}
	}
	return &p, nil
}

func (r *CatalogRepo) Plans(ctx context.Context) ([]catalog.Plan, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+planCols+` FROM plan_catalog WHERE active ORDER BY sort, key`)
	if err != nil {
		return nil, Translate(err, "plans")
	}
	defer rows.Close()
	var out []catalog.Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *CatalogRepo) PlanByKey(ctx context.Context, key string) (*catalog.Plan, error) {
	p, err := scanPlan(r.pool.QueryRow(ctx,
		`SELECT `+planCols+` FROM plan_catalog WHERE active AND key = $1`, key))
	if err != nil {
		return nil, Translate(err, "plan")
	}
	return p, nil
}

const providerCols = `key, category, name, credential_kind, permissions,
	needs_base_url, operated, docs_url, brand_color, sort, active`

func scanProvider(row pgx.Row) (*catalog.Provider, error) {
	var p catalog.Provider
	if err := row.Scan(&p.Key, &p.Category, &p.Name, &p.CredentialKind, &p.Permissions,
		&p.NeedsBaseURL, &p.Operated, &p.DocsURL, &p.BrandColor, &p.Sort, &p.Active); err != nil {
		return nil, err
	}
	if p.Permissions == nil {
		p.Permissions = []string{}
	}
	return &p, nil
}

func (r *CatalogRepo) Providers(ctx context.Context) ([]catalog.Provider, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+providerCols+` FROM provider_catalog WHERE active ORDER BY category, sort, key`)
	if err != nil {
		return nil, Translate(err, "providers")
	}
	defer rows.Close()
	var out []catalog.Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *CatalogRepo) ProviderByKey(ctx context.Context, key string) (*catalog.Provider, error) {
	p, err := scanProvider(r.pool.QueryRow(ctx,
		`SELECT `+providerCols+` FROM provider_catalog WHERE active AND key = $1`, key))
	if err != nil {
		return nil, Translate(err, "provider")
	}
	return p, nil
}
