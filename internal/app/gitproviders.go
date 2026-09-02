package app

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/gitprovider"
	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/resource"
	"github.com/Digital-Business-One/dop-core/internal/platform/config"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// gitProviders resolves WHICH provider serves each repository, and with which
// credential.
//
// It is the only place in the system that knows all three ends — repository,
// integration and vault — and that is why it lives here, in the composition
// root. The git adapter does not know the vault; the delivery domain does not
// know GitHub or GitLab; the resource domain does not know PRs exist.
//
// The choice is PER REPOSITORY, not at boot (ADR-0013): that is what
// `ProjectRepo.IntegrationID` exists for. A project with one repo on GitHub and
// another on GitLab has to work, and a single provider chosen by configuration
// would make that impossible — silently, which is the worst way.
type gitProviders struct {
	pool      *pgxpool.Pool
	resources *resource.Service
	secrets   ports.SecretStore
	cfg       *config.Config
}

func (g gitProviders) For(ctx context.Context, accountID, repoID string) (delivery.GitProvider, error) {
	// 1. The repository gives the integration. The query is direct because
	// `project_repos` is not an entity of the delivery domain, and making the
	// hierarchy domain expose a path just for this would couple the two.
	var integrationID, externalID string
	err := g.pool.QueryRow(ctx, `
		SELECT r.integration_id::text, r.external_id
		  FROM project_repos r
		  JOIN projects p ON p.id = r.project_id
		 WHERE r.id = $1 AND p.account_id = $2`, repoID, accountID).
		Scan(&integrationID, &externalID)
	if err != nil {
		return nil, errs.NotFound("repository %s not found in this account", repoID)
	}

	// 2. The integration gives the provider and the API's base.
	res, err := g.resources.Get(ctx, integrationID)
	if err != nil {
		return nil, err
	}
	spec, err := resource.ParseIntegration(res.Config)
	if err != nil {
		return nil, err
	}
	if spec.Category != resource.CategoryGit {
		return nil, errs.Precondition(
			"the repository's integration is of category %q, not git", spec.Category)
	}

	// 3. The vault gives the credential — and it is HERE that it is read, in
	// the core, which is the one that has the vault. The adapter receives the
	// token ready-made and never knew a vault exists.
	valor, err := g.secrets.Get(ctx, resource.SecretRefFor(accountID, res.ID))
	if err != nil {
		return nil, err
	}
	if len(valor) == 0 {
		return nil, errs.Precondition(
			"integration %q has no credential configured", res.Name)
	}

	switch spec.Provider {
	case "github":
		base := spec.BaseURL
		if base == "" {
			base = g.cfg.GitHubAPI
		}
		return gitprovider.NewGitHub(gitprovider.GitHubConfig{
			APIBase:     base,
			GraphQLURL:  g.cfg.GitHubGraphQL,
			Token:       string(valor),
			ActorID:     externalID,
			MergeMethod: g.cfg.GitMergeMethod,
		}), nil
	case "gitlab":
		base := spec.BaseURL
		if base == "" {
			base = g.cfg.GitLabAPI
		}
		return gitprovider.NewGitLab(gitprovider.GitLabConfig{
			APIBase:     base,
			Token:       string(valor),
			ActorID:     externalID,
			MergeMethod: g.cfg.GitMergeMethod,
		}), nil
	}
	// An unknown provider is an explicit refusal, never a default: opening a PR
	// in the wrong place is worse than not opening one.
	return nil, errs.Invalid("unsupported git provider: %q", spec.Provider)
}
