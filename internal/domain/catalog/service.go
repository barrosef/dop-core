package catalog

import "context"

// Service is a pass-through on purpose. The catalogue has no rule beyond
// "active only", and that one belongs to the query; the service exists so the
// composition root and the gRPC layer depend on a domain, not on Postgres.
type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

func (s *Service) Plans(ctx context.Context) ([]Plan, error)         { return s.repo.Plans(ctx) }
func (s *Service) Providers(ctx context.Context) ([]Provider, error) { return s.repo.Providers(ctx) }

func (s *Service) PlanByKey(ctx context.Context, key string) (*Plan, error) {
	return s.repo.PlanByKey(ctx, key)
}

func (s *Service) ProviderByKey(ctx context.Context, key string) (*Provider, error) {
	return s.repo.ProviderByKey(ctx, key)
}

// PlanExists is the narrow question identity asks before recording a choice
// (identity.Plans). It is declared here so identity's port is satisfied by the
// service itself, with no glue type in the composition root.
func (s *Service) PlanExists(ctx context.Context, key string) (bool, error) {
	if _, err := s.repo.PlanByKey(ctx, key); err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
