// Adaptador de agent.AgentProvider sobre a API da OpenAI/Codex — o SEGUNDO
// adaptador.
//
// Ele existe por disciplina da ADR-0001: *uma porta com um adaptador só é
// palpite*. Enquanto só houvesse o adaptador Anthropic, "porta de provedor de
// agente" seria a API da Anthropic com outro nome — e as diferenças que a
// plataforma vai pagar (cache, contagem, effort) só apareceriam no dia da troca.
//
// AS DIVERGÊNCIAS, como este adaptador as trata (numeração de agent/entity.go):
//
// D1 (cache) — Aqui o cache de prefixo é AUTOMÁTICO: não há breakpoint para
// marcar, não há TTL para escolher e, sobretudo, NÃO HÁ CONTABILIDADE DE CRIAÇÃO
// de cache. A resposta informa só `prompt_tokens_details.cached_tokens`
// (leitura). Por isso `Usage.CacheCreationTokens` é sempre 0 aqui, e o adaptador
// NÃO anuncia `CapCacheCreationAccounting`. A consequência é de produto: o alerta
// de invalidador silencioso da ADR-0012 §1 ("leitura de cache zerada em prefixo
// que deveria estar estável", que é o `cost.UsageEvent.SuspectCacheMiss`)
// continua funcionando, mas a outra metade — "escreveu cache e nunca leu" — é
// INVISÍVEL sob este provedor. Quem lê a telemetria precisa ver a capacidade
// junto do número, e é por isso que ela viaja no `Reply`.
//
// D2 (contagem) — AQUI ESTÁ A ARMADILHA DA DUPLA CONTAGEM. Neste fornecedor
// `prompt_tokens` INCLUI os tokens servidos do cache; `cached_tokens` é um
// SUBCONJUNTO dele. Somar os dois campos como se fossem parcelas disjuntas — que
// é o que `cost.UsageEvent` assume, porque é a semântica da Anthropic — inflaria
// a medição da ADR-0011 sem erro nenhum aparecendo. Este adaptador SUBTRAI, e a
// subtração é a linha mais importante do arquivo.
//
// D3 (operador) — `role:"developer"` é o canal de autoridade deste fornecedor, e
// é aceito em qualquer posição. O prefixo estável e a intervenção do operador vão
// os DOIS como `developer`, o que é correto: os dois são autoria da plataforma. O
// que a porta garante e este adaptador cumpre é que intervenção de operador nunca
// sai como `role:"user"`.
//
// D4 (effort) — Aqui só existem `low|medium|high`. `xhigh` e `max` são REBAIXADOS
// para `high`, com aviso legível no `Reply`. Numa tarefa crítica (ADR-0007: não
// se economiza no crítico) isso é decisão de produto, não detalhe de adaptador:
// quem roteia para `max` e recebe `high` precisa saber que recebeu.
//
// D5 (parada) — `stop | length | tool_calls | content_filter`, normalizados.
package agentprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

const (
	NomeOpenAI = "openai"
	BaseOpenAI = "https://api.openai.com/v1"
	rotaOpenAI = "/chat/completions"
)

// CatalogoOpenAI é o catálogo de PARTIDA deste fornecedor — a mesma natureza do
// `cost.DefaultCatalog()`: nomes substituíveis, não afirmação sobre o catálogo
// vigente da OpenAI. Quando o provedor vier configurado no recurso da conta
// (ADR-0013), o catálogo vem de lá e isto fica só como padrão.
func CatalogoOpenAI() map[agent.ModelClass]string {
	return map[agent.ModelClass]string{
		agent.ClassCheap:  "gpt-5-mini",
		agent.ClassMedium: "gpt-5",
		agent.ClassStrong: "gpt-5-codex",
	}
}

// PrecosOpenAI é VAZIO de propósito.
//
// Preencher esta tabela com números que ninguém conferiu seria pior que
// deixá-la vazia: um preço inventado alimenta o orçamento da ADR-0011 com ficção
// convincente, e ninguém confere um número plausível. Vazia, `PriceFor` devolve
// "desconhecido" e o ciclo do turno DIZ que não sabe calcular o custo deste
// provedor — em vez de gravar zero, que afirmaria que foi de graça.
//
// Preencher é trabalho de quem tem a tabela do contrato, e o lugar certo é a
// configuração do recurso de categoria `agent` (ADR-0013).
func PrecosOpenAI() map[string]agent.Price { return map[string]agent.Price{} }

// effortOpenAI: os cinco níveis do núcleo → os três daqui (D4). `xhigh` e `max`
// caem em `high`, que é o teto REAL — fingir que aplicou `max` seria pior que
// rebaixar, porque quem confia no `max` do crítico não teria como descobrir.
var effortOpenAI = map[agent.Effort]agent.Effort{
	agent.EffortLow:    agent.EffortLow,
	agent.EffortMedium: agent.EffortMedium,
	agent.EffortHigh:   agent.EffortHigh,
	agent.EffortXHigh:  agent.EffortHigh,
	agent.EffortMax:    agent.EffortHigh,
}

var paradaOpenAI = map[string]agent.StopReason{
	"stop":           agent.StopCompleted,
	"length":         agent.StopMaxTokens,
	"tool_calls":     agent.StopToolUse,
	"content_filter": agent.StopRefused,
}

type OpenAIConfig struct {
	APIBase string
	// APIKey é o valor JÁ RESOLVIDO da credencial de recurso (ADR-0013).
	APIKey  string
	Catalog map[agent.ModelClass]string
	Timeout time.Duration
	Client  httpDoer
}

type OpenAI struct {
	c       *client
	catalog map[agent.ModelClass]string
}

func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	base := cfg.APIBase
	if base == "" {
		base = BaseOpenAI
	}
	autorizar := func(r *http.Request) {
		if cfg.APIKey != "" {
			r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}
		r.Header.Set("User-Agent", "dop-core")
	}
	cat := cfg.Catalog
	if len(cat) == 0 {
		cat = CatalogoOpenAI()
	}
	return &OpenAI{
		c:       newClient(base, NomeOpenAI, cfg.Client, cfg.Timeout, autorizar, cfg.APIKey),
		catalog: cat,
	}
}

var _ agent.AgentProvider = (*OpenAI)(nil)

func (o OpenAI) String() string { return "agentprovider.OpenAI{}" }

func (o *OpenAI) Info() agent.ProviderInfo {
	return agent.ProviderInfo{
		Name:    NomeOpenAI,
		Catalog: o.catalog,
		Capabilities: agent.Capabilities{
			// Sem CapExplicitPrefixCache: o cache é automático (D1).
			// Sem CapCacheCreationAccounting: criação não é reportada (D1).
			// Sem CapFullEffortRange: xhigh e max não existem (D4).
			agent.CapOperatorChannel,
			agent.CapStructuredOutput,
		},
		Prices: PrecosOpenAI(),
	}
}

// ── formas do fornecedor ────────────────────────────────────────────────────
//
// A ordem de declaração é a ordem do corpo serializado, e `Messages` vem cedo
// porque a única alavanca de cache DESTE fornecedor é a ORDEM (D1): estável
// primeiro, volátil depois. Aqui não há breakpoint para marcar — o que existe é
// a disciplina de não deixar nada volátil antes do prefixo.

type oaiMensagem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type oaiFormato struct {
	Type       string    `json:"type"`
	JSONSchema oaiSchema `json:"json_schema"`
}

type oaiPedido struct {
	Model               string        `json:"model"`
	Messages            []oaiMensagem `json:"messages"`
	MaxCompletionTokens int           `json:"max_completion_tokens"`
	ReasoningEffort     string        `json:"reasoning_effort"`
	ResponseFormat      *oaiFormato   `json:"response_format,omitempty"`
}

type oaiResposta struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// ── montagem ────────────────────────────────────────────────────────────────

func (o *OpenAI) Render(t agent.Turn, model string, effort agent.Effort) ([]byte, []string, error) {
	var avisos []string

	// O prefixo estável é a PRIMEIRA mensagem. Não há breakpoint para marcar
	// (D1): a única alavanca de cache é a ordem, e é ela que este adaptador
	// respeita.
	mensagens := []oaiMensagem{{Role: "developer", Content: t.StablePrefix}}
	for _, m := range t.Messages {
		switch m.Role {
		case agent.RoleOperator:
			// Canal de autoridade — NUNCA `user` (D3).
			mensagens = append(mensagens, oaiMensagem{Role: "developer", Content: m.Text})
		case agent.RoleAssistant:
			mensagens = append(mensagens, oaiMensagem{Role: "assistant", Content: m.Text})
		default:
			mensagens = append(mensagens, oaiMensagem{Role: "user", Content: m.Text})
		}
	}

	aplicado, conhecido := effortOpenAI[effort]
	if !conhecido {
		// Effort fora do vocabulário do núcleo cai no teto real deste
		// fornecedor, não no piso: na dúvida não se economiza (ADR-0011 §3).
		aplicado = agent.EffortHigh
	}
	if aplicado != effort {
		avisos = append(avisos,
			"effort '"+string(effort)+"' não existe neste provedor: aplicado '"+
				string(aplicado)+"' (D4). Em trabalho crítico, isso é decisão de produto.")
	}

	pedido := oaiPedido{
		Model:               model,
		Messages:            mensagens,
		MaxCompletionTokens: t.MaxOutputTokens,
		ReasoningEffort:     string(aplicado),
	}
	if t.OutputSchema != nil {
		pedido.ResponseFormat = &oaiFormato{
			Type:       "json_schema",
			JSONSchema: oaiSchema{Name: "dop_turn", Strict: true, Schema: t.OutputSchema},
		}
	}

	corpo, err := json.Marshal(pedido)
	if err != nil {
		return nil, nil, agent.Unavailability(NomeOpenAI, agent.ReasonProviderError,
			"pedido ilegível: "+err.Error())
	}
	return corpo, avisos, nil
}

// ── envio ───────────────────────────────────────────────────────────────────

func (o *OpenAI) Send(ctx context.Context, t agent.Turn, model string,
	effort agent.Effort) (*agent.Reply, error) {

	if !o.c.temCredencial {
		return nil, agent.Unavailability(NomeOpenAI, agent.ReasonMissingCredential, "")
	}

	corpo, avisos, err := o.Render(t, model, effort)
	if err != nil {
		return nil, err
	}
	status, resp, err := o.c.post(ctx, rotaOpenAI, corpo)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, o.c.falha(status, resp)
	}

	var out oaiResposta
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, agent.Unavailability(NomeOpenAI, agent.ReasonProviderError,
			"resposta ilegível: "+o.c.redact(err.Error()))
	}

	var texto, motivo string
	if len(out.Choices) > 0 {
		texto = out.Choices[0].Message.Content
		motivo = out.Choices[0].FinishReason
	}

	cacheados := out.Usage.PromptTokensDetails.CachedTokens
	// ── D2, a linha que impede a dupla contagem ─────────────────────────────
	// `prompt_tokens` INCLUI `cached_tokens` neste fornecedor; o domínio de
	// custo espera parcelas DISJUNTAS. O `max(…, 0)` não é paranoia: se um dia
	// o fornecedor mudar a semântica, o pior caso passa a ser subestimar a
	// entrada — e não gravar um número impossível no orçamento.
	entrada := out.Usage.PromptTokens - cacheados
	if entrada < 0 {
		entrada = 0
	}

	parada, ok := paradaOpenAI[motivo]
	if !ok {
		parada = agent.StopUnknown
	}

	aplicado, conhecido := effortOpenAI[effort]
	if !conhecido {
		aplicado = agent.EffortHigh
	}

	return &agent.Reply{
		Text: texto,
		Usage: agent.Usage{
			InputTokens:     entrada,
			OutputTokens:    out.Usage.CompletionTokens,
			CacheReadTokens: cacheados,
			// NÃO é zero-afirmação: é "não dá para saber" (D1). A capacidade
			// AUSENTE em Capabilities é o que diz isso a quem lê.
			CacheCreationTokens: 0,
		},
		Model:         out.Model,
		Provider:      NomeOpenAI,
		StopReason:    parada,
		Data:          decodificarTexto(texto),
		EffortApplied: aplicado,
		Capabilities:  o.Info().Capabilities,
		Warnings:      avisos,
	}, nil
}
