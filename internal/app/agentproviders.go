package app

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/adapter/agentprovider"
	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/resource"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// agentProviders resolves WHICH agent provider serves a turn, and with which
// credential. It is `gitProviders`'s twin, and on purpose: the path is the same.
//
// It is the only place in the system that knows all three ends — the turn, the
// integration and the vault — and that is why it lives here, in the composition
// root. The provider adapter does not know the vault; the agent domain does not
// know Anthropic or OpenAI; the resource domain does not know turns exist.
//
// ── AND IT IS HERE THAT ADR-0023 HAPPENS ────────────────────────────────────
//
// The credential is read from the vault and handed to the adapter WITHIN THE
// SAME PROCESS. It does not become an RPC response, does not enter an event
// envelope, does not go through the BFF. While the runtime lived on the other
// side (ADR-0016), there was no honest path: either the BFF got a vault of its
// own — and compromising the layer exposed to the internet would start handing
// over the agent credentials of ALL the accounts — or the core got an RPC that
// returns a secret, undoing the isolation the whole platform had built. Both were
// refused; this function is the third way out.
//
// The choice is PER REQUEST, not at boot (ADR-0013): an agent provider is an
// account resource, and several accounts coexist in the same process. A single
// provider chosen by configuration would make multi-tenancy impossible —
// silently, which is the worst way.
type agentProviders struct {
	resources *resource.Service
	secrets   ports.SecretStore
}

var _ agent.Providers = agentProviders{}

// For returns the requested resource's adapter, or the account's only agent
// provider when the caller did not choose.
func (g agentProviders) For(ctx context.Context, resourceID string) (agent.AgentProvider, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}

	// 1. Which resource. With no choice from the caller, the account's default
	// — and the default only exists when it is the ONLY one (see
	// accountDefault).
	res := (*resource.Resource)(nil)
	if resourceID != "" {
		res, err = g.resources.Get(ctx, resourceID)
	} else {
		res, err = g.accountDefault(ctx)
	}
	if err != nil {
		return nil, err
	}

	// 2. The integration gives the provider and the API's base.
	spec, err := resource.ParseIntegration(res.Config)
	if err != nil {
		return nil, err
	}
	if spec.Category != resource.CategoryAgent {
		// An explicit refusal: using a git integration as an agent provider
		// would send GitHub's credential to a model's API.
		return nil, errs.Precondition(
			"integration %q is of category %q, not agent", res.Name, spec.Category)
	}

	// 3. The vault gives the credential — and it is HERE that it is read, in
	// the core, which is the one that has the vault. The adapter receives the
	// key ready-made and never knew a vault exists.
	value, err := g.secrets.Get(ctx, resource.SecretRefFor(accountID, res.ID))
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		// A missing credential is a PRECONDITION with the resource's name —
		// never with the value, which does not exist, nor with the vault's
		// reference, which is a path.
		return nil, agent.Unavailability(spec.Provider, agent.ReasonMissingCredential,
			"integration "+res.Name+" has no credential configured")
	}

	switch spec.Provider {
	case agentprovider.NameAnthropic:
		return agentprovider.NewAnthropic(agentprovider.AnthropicConfig{
			APIBase: spec.BaseURL,
			APIKey:  string(value),
			Catalog: resourceCatalog(res.Config, agentprovider.CatalogAnthropic()),
		}), nil
	case agentprovider.NameOpenAI:
		return agentprovider.NewOpenAI(agentprovider.OpenAIConfig{
			APIBase: spec.BaseURL,
			APIKey:  string(value),
			Catalog: resourceCatalog(res.Config, agentprovider.CatalogOpenAI()),
		}), nil
	}
	// An unknown provider is an explicit refusal, NEVER a fallback to the
	// default: falling back to another provider without warning would change the
	// model, the price and the cache semantics of a whole demand, and the only
	// place it would show up is the invoice.
	return nil, agent.Unavailability(spec.Provider, agent.ReasonUnknownProvider, "")
}

// accountDefault is the agent provider when the caller did not choose.
//
// It only exists when it is the ONLY one. Two agent integrations and no choice
// is AMBIGUITY, and choosing on our own would swap a demand's provider midway —
// along with the cached prefix, the price and the cache semantics. Refusing with
// the list is the only honest answer.
func (g agentProviders) accountDefault(ctx context.Context) (*resource.Resource, error) {
	all, err := g.resources.List(ctx, resource.KindIntegration)
	if err != nil {
		return nil, err
	}
	var candidatos []resource.Resource
	for _, r := range all {
		spec, err := resource.ParseIntegration(r.Config)
		if err != nil {
			// A badly configured integration does not bring the search down: it
			// simply is not a candidate. Failing here would let one bad row of
			// another category block the whole turn.
			continue
		}
		if spec.Category == resource.CategoryAgent {
			candidatos = append(candidatos, r)
		}
	}
	switch len(candidatos) {
	case 0:
		return nil, errs.Precondition(
			"this account has no 'agent'-category integration (ADR-0013): " +
				"connect an agent provider before running a turn")
	case 1:
		return &candidatos[0], nil
	default:
		nomes := make([]string, 0, len(candidatos))
		for _, c := range candidatos {
			nomes = append(nomes, c.Name)
		}
		return nil, errs.Invalid(
			"this account has %d agent providers (%v): say which one should serve the turn",
			len(candidatos), nomes)
	}
}

// resourceCatalog reads the menu declared in the integration's configuration.
//
// It is what ADR-0013 promises: the model menu belongs to the RESOURCE, not to
// the code. An absent or malformed configuration falls back to the adapter's
// starting catalog — never to an empty catalog, which would make `ResolveModel`
// return an empty name and the provider refuse the call for a reason that is not
// the real one.
func resourceCatalog(config map[string]any, defaults map[agent.ModelClass]string) map[agent.ModelClass]string {
	raw, ok := config["catalog"].(map[string]any)
	if !ok || len(raw) == 0 {
		return defaults
	}
	out := make(map[agent.ModelClass]string, len(defaults))
	for class, name := range defaults {
		out[class] = name
	}
	for _, class := range []agent.ModelClass{agent.ClassCheap, agent.ClassMedium, agent.ClassStrong} {
		if name, ok := raw[string(class)].(string); ok && name != "" {
			out[class] = name
		}
	}
	return out
}
