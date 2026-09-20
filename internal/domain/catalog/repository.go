package catalog

import "context"

// Repository answers with ACTIVE rows only, in display order. An inactive key
// is NotFound: the catalogue retired it, and a choice pointing at it is a
// choice nobody can make any more.
type Repository interface {
	Plans(ctx context.Context) ([]Plan, error)
	Providers(ctx context.Context) ([]Provider, error)
	PlanByKey(ctx context.Context, key string) (*Plan, error)
	ProviderByKey(ctx context.Context, key string) (*Provider, error)
}
