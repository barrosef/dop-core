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

// agentProviders resolve QUAL provedor de agente atende um turno, e com qual
// credencial. É o gêmeo de `gitProviders`, e de propósito: o caminho é o mesmo.
//
// É o único lugar do sistema que conhece as três pontas — o turno, a integração
// e o cofre — e por isso mora aqui, no composition root. O adaptador de provedor
// não conhece o cofre; o domínio de agente não conhece Anthropic nem OpenAI; o
// domínio de recursos não sabe que existe turno.
//
// ── E É AQUI QUE A ADR-0023 ACONTECE ────────────────────────────────────────
//
// A credencial é lida do cofre e entregue ao adaptador DENTRO DO MESMO PROCESSO.
// Ela não vira resposta de RPC, não entra em envelope de evento, não passa pelo
// BFF. Enquanto o runtime vivia do outro lado (ADR-0016), não havia caminho
// honesto: ou o BFF ganhava cofre próprio — e comprometer a camada exposta à
// internet passaria a entregar as credenciais de agente de TODAS as contas —, ou
// o núcleo ganhava uma RPC que devolve segredo, desfazendo o isolamento que a
// plataforma inteira construiu. As duas foram recusadas; esta função é a terceira
// saída.
//
// A escolha é POR REQUISIÇÃO, não de boot (ADR-0013): provedor de agente é
// recurso de conta, e várias contas convivem no mesmo processo. Um provedor único
// escolhido por configuração tornaria multi-tenant impossível — em silêncio, que
// é o pior jeito.
type agentProviders struct {
	resources *resource.Service
	secrets   ports.SecretStore
}

var _ agent.Providers = agentProviders{}

// For devolve o adaptador do recurso pedido, ou o único provedor de agente da
// conta quando o chamador não escolheu.
func (g agentProviders) For(ctx context.Context, resourceID string) (agent.AgentProvider, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}

	// 1. Qual recurso. Sem escolha do chamador, o padrão da conta — e o padrão
	// só existe quando é ÚNICO (ver padraoDaConta).
	res := (*resource.Resource)(nil)
	if resourceID != "" {
		res, err = g.resources.Get(ctx, resourceID)
	} else {
		res, err = g.padraoDaConta(ctx)
	}
	if err != nil {
		return nil, err
	}

	// 2. Da integração sai o provedor e a base da API.
	spec, err := resource.ParseIntegration(res.Config)
	if err != nil {
		return nil, err
	}
	if spec.Category != resource.CategoryAgent {
		// Recusa explícita: usar uma integração de git como provedor de agente
		// mandaria a credencial do GitHub para a API de um modelo.
		return nil, errs.Precondition(
			"a integração %q é de categoria %q, não agent", res.Name, spec.Category)
	}

	// 3. Do cofre sai a credencial — e é AQUI que ela é lida, no núcleo, que é
	// quem tem o cofre. O adaptador recebe a chave pronta e nunca soube que
	// existe um cofre.
	valor, err := g.secrets.Get(ctx, resource.SecretRefFor(accountID, res.ID))
	if err != nil {
		return nil, err
	}
	if len(valor) == 0 {
		// Credencial ausente é PRECONDIÇÃO com nome de recurso — nunca com o
		// valor, que não existe, nem com a referência do cofre, que é caminho.
		return nil, agent.Unavailability(spec.Provider, agent.ReasonMissingCredential,
			"a integração "+res.Name+" não tem credencial configurada")
	}

	switch spec.Provider {
	case agentprovider.NameAnthropic:
		return agentprovider.NewAnthropic(agentprovider.AnthropicConfig{
			APIBase: spec.BaseURL,
			APIKey:  string(valor),
			Catalog: catalogoDoRecurso(res.Config, agentprovider.CatalogAnthropic()),
		}), nil
	case agentprovider.NameOpenAI:
		return agentprovider.NewOpenAI(agentprovider.OpenAIConfig{
			APIBase: spec.BaseURL,
			APIKey:  string(valor),
			Catalog: catalogoDoRecurso(res.Config, agentprovider.CatalogOpenAI()),
		}), nil
	}
	// Provedor desconhecido é recusa explícita, NUNCA uma queda para o padrão:
	// cair para outro fornecedor sem avisar trocaria o modelo, o preço e a
	// semântica de cache de uma demanda inteira, e o único lugar onde isso
	// apareceria seria a fatura.
	return nil, agent.Unavailability(spec.Provider, agent.ReasonUnknownProvider, "")
}

// padraoDaConta é o provedor de agente quando o chamador não escolheu.
//
// Só existe quando é ÚNICO. Duas integrações de agente e nenhuma escolha é
// AMBIGUIDADE, e escolher por conta própria trocaria o fornecedor de uma demanda
// no meio do caminho — com o prefixo cacheado, o preço e a semântica de cache
// junto. Recusar com a lista é a única resposta honesta.
func (g agentProviders) padraoDaConta(ctx context.Context) (*resource.Resource, error) {
	todos, err := g.resources.List(ctx, resource.KindIntegration)
	if err != nil {
		return nil, err
	}
	var candidatos []resource.Resource
	for _, r := range todos {
		spec, err := resource.ParseIntegration(r.Config)
		if err != nil {
			// Integração mal configurada não derruba a busca: ela apenas não é
			// candidata. Derrubar aqui faria uma linha ruim de outra categoria
			// impedir o turno inteiro.
			continue
		}
		if spec.Category == resource.CategoryAgent {
			candidatos = append(candidatos, r)
		}
	}
	switch len(candidatos) {
	case 0:
		return nil, errs.Precondition(
			"esta conta não tem integração de categoria 'agent' (ADR-0013): " +
				"conecte um provedor de agente antes de rodar um turno")
	case 1:
		return &candidatos[0], nil
	default:
		nomes := make([]string, 0, len(candidatos))
		for _, c := range candidatos {
			nomes = append(nomes, c.Name)
		}
		return nil, errs.Invalid(
			"esta conta tem %d provedores de agente (%v): informe qual deve atender o turno",
			len(candidatos), nomes)
	}
}

// catalogoDoRecurso lê o cardápio declarado na configuração da integração.
//
// É o que a ADR-0013 promete: o cardápio de modelos é do RECURSO, não do código.
// Configuração ausente ou malformada cai no catálogo de partida do adaptador —
// nunca em catálogo vazio, que faria `ResolveModel` devolver nome vazio e o
// fornecedor recusar a chamada por um motivo que não é o real.
func catalogoDoRecurso(config map[string]any, padrao map[agent.ModelClass]string) map[agent.ModelClass]string {
	bruto, ok := config["catalog"].(map[string]any)
	if !ok || len(bruto) == 0 {
		return padrao
	}
	out := make(map[agent.ModelClass]string, len(padrao))
	for classe, nome := range padrao {
		out[classe] = nome
	}
	for _, classe := range []agent.ModelClass{agent.ClassCheap, agent.ClassMedium, agent.ClassStrong} {
		if nome, ok := bruto[string(classe)].(string); ok && nome != "" {
			out[classe] = nome
		}
	}
	return out
}
