package app

import (
	"context"

	"github.com/barrosef/dop-core/internal/adapter/gitprovider"
	"github.com/barrosef/dop-core/internal/domain/resource"
	"github.com/barrosef/dop-core/internal/platform/config"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// integrationCheckers is resource.CredentialChecker: it knows which adapter
// probes which provider, and it lives here for the same reason gitProviders
// does — it is the one place that may know GitHub, GitLab and the resource
// vocabulary at once. The secret arrives already resolved by the resource
// service; this type never touches the vault.
//
// A provider with no adapter answers Operated=false and nothing else. That is
// the honest state the onboarding journey shows as "registered — operation
// coming", and it must never be dressed up as a passed check.
type integrationCheckers struct{ cfg *config.Config }

func (c integrationCheckers) Check(ctx context.Context, spec resource.IntegrationSpec, secret []byte) (resource.CheckResult, error) {
	var whoami func(context.Context) (string, error)
	switch spec.Provider {
	case "github":
		base := spec.BaseURL
		if base == "" {
			base = c.cfg.GitHubAPI
		}
		whoami = gitprovider.NewGitHub(gitprovider.GitHubConfig{
			APIBase: base, GraphQLURL: c.cfg.GitHubGraphQL, Token: string(secret),
		}).Whoami
	case "gitlab", "gitlab_self_hosted":
		base := spec.BaseURL
		if base == "" {
			base = c.cfg.GitLabAPI
		}
		whoami = gitprovider.NewGitLab(gitprovider.GitLabConfig{
			APIBase: base, Token: string(secret),
		}).Whoami
	default:
		return resource.CheckResult{
			Operated: false,
			Message:  "this provider is registered but not operated yet; the credential is stored",
		}, nil
	}

	identity, err := whoami(ctx)
	switch errs.KindOf(err) {
	case "":
		return resource.CheckResult{Operated: true, OK: true, Identity: identity}, nil
	case errs.KindUnauthorized:
		// The provider refused the token. A result, not an error: the person
		// asked "does it work" and the answer is no.
		return resource.CheckResult{Operated: true, OK: false, Message: "the provider refused this token"}, nil
	case errs.KindPermission:
		return resource.CheckResult{Operated: true, OK: false, Message: "the token works but lacks the permissions asked for"}, nil
	}
	// Unavailable, internal: the check could not be made, which is different
	// from a token that does not work.
	return resource.CheckResult{}, err
}
