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

// gitProviders resolve QUAL provedor atende cada repositório, e com qual
// credencial.
//
// É o único lugar do sistema que conhece as três pontas — repositório,
// integração e cofre — e por isso ele mora aqui, no composition root. O
// adaptador de git não conhece o cofre; o domínio de entrega não conhece
// GitHub nem GitLab; o domínio de recursos não sabe que existe PR.
//
// A escolha é POR REPOSITÓRIO, não de boot (ADR-0013): é para isso que
// `ProjectRepo.IntegrationID` existe. Um projeto com um repo no GitHub e outro
// no GitLab tem que funcionar, e um provedor único escolhido por configuração
// tornaria isso impossível — em silêncio, que é o pior jeito.
type gitProviders struct {
	pool      *pgxpool.Pool
	resources *resource.Service
	secrets   ports.SecretStore
	cfg       *config.Config
}

func (g gitProviders) For(ctx context.Context, accountID, repoID string) (delivery.GitProvider, error) {
	// 1. Do repositório sai a integração. A consulta é direta porque
	// `project_repos` não é entidade do domínio de entrega, e fazer o domínio
	// de hierarquia expor um caminho só para isto seria acoplar os dois.
	var integrationID, externalID string
	err := g.pool.QueryRow(ctx, `
		SELECT r.integration_id::text, r.external_id
		  FROM project_repos r
		  JOIN projects p ON p.id = r.project_id
		 WHERE r.id = $1 AND p.account_id = $2`, repoID, accountID).
		Scan(&integrationID, &externalID)
	if err != nil {
		return nil, errs.NotFound("repositório %s não encontrado nesta conta", repoID)
	}

	// 2. Da integração sai o provedor e a base da API.
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
			"a integração do repositório é de categoria %q, não git", spec.Category)
	}

	// 3. Do cofre sai a credencial — e é AQUI que ela é lida, no núcleo, que é
	// quem tem o cofre. O adaptador recebe o token pronto e nunca soube que
	// existe um cofre.
	valor, err := g.secrets.Get(ctx, resource.SecretRefFor(accountID, res.ID))
	if err != nil {
		return nil, err
	}
	if len(valor) == 0 {
		return nil, errs.Precondition(
			"a integração %q não tem credencial configurada", res.Name)
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
	// Provedor desconhecido é recusa explícita, nunca um default: abrir PR no
	// lugar errado é pior que não abrir.
	return nil, errs.Invalid("provedor de git não suportado: %q", spec.Provider)
}
