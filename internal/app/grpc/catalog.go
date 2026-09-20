package grpc

import (
	"context"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/catalog"
)

type CatalogServer struct {
	dopv1.UnimplementedCatalogServiceServer
	svc *catalog.Service
}

func NewCatalogServer(svc *catalog.Service) *CatalogServer { return &CatalogServer{svc: svc} }

func (s *CatalogServer) ListPlans(ctx context.Context, _ *dopv1.ListPlansRequest) (*dopv1.ListPlansResponse, error) {
	plans, err := s.svc.Plans(ctx)
	if err != nil {
		return nil, err
	}
	out := &dopv1.ListPlansResponse{}
	for _, p := range plans {
		out.Plans = append(out.Plans, &dopv1.Plan{
			Key: p.Key, Name: p.Name, Tagline: p.Tagline, Features: p.Features, Sort: int32(p.Sort),
		})
	}
	return out, nil
}

func (s *CatalogServer) ListProviders(ctx context.Context, _ *dopv1.ListProvidersRequest) (*dopv1.ListProvidersResponse, error) {
	providers, err := s.svc.Providers(ctx)
	if err != nil {
		return nil, err
	}
	out := &dopv1.ListProvidersResponse{}
	for _, p := range providers {
		out.Providers = append(out.Providers, &dopv1.Provider{
			Key: p.Key, Category: p.Category, Name: p.Name, CredentialKind: p.CredentialKind,
			Permissions: p.Permissions, NeedsBaseUrl: p.NeedsBaseURL, Operated: p.Operated,
			DocsUrl: p.DocsURL, BrandColor: p.BrandColor, Sort: int32(p.Sort),
		})
	}
	return out, nil
}
