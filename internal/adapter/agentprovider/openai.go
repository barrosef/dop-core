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
//
// D7 (declaração) — Aqui a ferramenta vem embrulhada: `{type:"function",
// function:{name, description, parameters}}`. Dois nomes diferentes para a mesma
// coisa (`parameters`, não `input_schema`) e um nível a mais de aninhamento. É
// só casca — e é por isso que o terreno comum da porta é nome + descrição +
// schema, e não o formato de nenhum dos dois.
//
// D8 (chamada) — **AQUI ESTÁ A SEGUNDA ARMADILHA DESTE ARQUIVO**, irmã da dupla
// contagem. A chamada vem em `tool_calls`, FORA de `content`, e
// `function.arguments` é uma **STRING**, não um objeto: ela ainda precisa de
// `json.Unmarshal`. E o modelo pode emitir uma string que não é JSON válido —
// argumento cortado no meio, aspas erradas. Ali, falhar o turno seria a resposta
// errada: quem conserta o argumento é o MODELO, e ele só conserta se receber o
// erro de volta. Este adaptador devolve a chamada com `Input` nulo, `RawInput`
// preenchido e um aviso; o laço a transforma em resultado de erro.
//
// D9 (resultado) — Uma mensagem `role:"tool"` POR resultado, com `tool_call_id`,
// e SEM campo de erro. O booleano `IsError` da porta não tem onde caber, então
// ele vira MARCA NO TEXTO. Perder a marca faria o modelo ler uma falha como
// saída normal — que é a diferença entre "o teste passou" e "o teste nem rodou".
//
// D10 (paralelismo) — `parallel_tool_calls` é campo de topo aqui e o default é
// verdadeiro, que é o que queremos; o adaptador não manda a flag.
//
// D11 (não declarada) — `tool_call_id` sem par é 400, igual ao outro. Por isso a
// mensagem do assistente com `tool_calls` é sempre reenviada antes dos
// resultados.
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
			agent.CapToolUse,
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

// oaiChamadaFuncao é o corpo de uma chamada. `Arguments` é STRING — é o D8 no
// tipo, e é por isso que ele aparece tanto na montagem quanto na leitura.
type oaiChamadaFuncao struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiChamada struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function oaiChamadaFuncao `json:"function"`
}

type oaiMensagem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls só em `role:"assistant"`; ToolCallID só em `role:"tool"`. Os
	// dois com `omitempty`: mandar `tool_calls: null` numa mensagem comum é
	// recusado por algumas versões da API.
	ToolCalls  []oaiChamada `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

// oaiFerramentaFuncao é o miolo da declaração (D7): `parameters`, não
// `input_schema`.
type oaiFerramentaFuncao struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oaiFerramenta struct {
	Type     string              `json:"type"`
	Function oaiFerramentaFuncao `json:"function"`
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
	Model string `json:"model"`
	// Tools ANTES de Messages, pela mesma razão da garantia 15: a declaração é
	// estável por thread e o que é estável vem primeiro. Aqui o cache é
	// automático (D1) e a ORDEM é a única alavanca que existe — deixar a
	// declaração depois da conversa a tiraria do prefixo comum sem erro nenhum,
	// só com fatura.
	//
	// `omitempty` é a garantia 16.
	Tools               []oaiFerramenta `json:"tools,omitempty"`
	Messages            []oaiMensagem   `json:"messages"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	ResponseFormat      *oaiFormato     `json:"response_format,omitempty"`
}

type oaiResposta struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// D8: FORA de `content`, e com os argumentos como STRING.
			ToolCalls []oaiChamada `json:"tool_calls"`
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
			// D11: as chamadas voltam junto com a fala. Sem elas, o
			// `tool_call_id` das mensagens `tool` seguintes fica órfão e a API
			// recusa o turno.
			mensagens = append(mensagens, oaiMensagem{
				Role: "assistant", Content: m.Text, ToolCalls: oaiChamadas(m.ToolCalls),
			})
		case agent.RoleToolResult:
			// D9: UMA mensagem por resultado — é a diferença de forma mais
			// visível entre os dois fornecedores para o mesmo turno do
			// domínio, e é por isso que a suíte de contrato compara CONTEÚDO e
			// nunca contagem de mensagens.
			for _, r := range m.ToolResults {
				mensagens = append(mensagens, oaiMensagem{
					Role: "tool", ToolCallID: r.CallID, Content: oaiTextoDeResultado(r),
				})
			}
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
		Tools:               oaiFerramentas(t.Tools),
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

// oaiFerramentas embrulha a declaração do domínio na casca deste fornecedor
// (D7). Lista vazia devolve nil e o `omitempty` cuida do resto (garantia 16).
func oaiFerramentas(specs []agent.ToolSpec) []oaiFerramenta {
	if len(specs) == 0 {
		return nil
	}
	out := make([]oaiFerramenta, 0, len(specs))
	for _, s := range specs {
		out = append(out, oaiFerramenta{
			Type: "function",
			Function: oaiFerramentaFuncao{
				Name: s.Name, Description: s.Description, Parameters: s.InputSchema,
			},
		})
	}
	return out
}

// oaiChamadas reenvia as chamadas na história (D11).
//
// `Arguments` volta a ser STRING, e é aqui que o `RawInput` ganha utilidade:
// quando o argumento veio ilegível, é ele que é reenviado — reserializar o
// `Input` nulo mandaria `null` no lugar do que o modelo escreveu, e o modelo
// perderia a chance de ver o próprio erro.
func oaiChamadas(calls []agent.ToolCall) []oaiChamada {
	if len(calls) == 0 {
		return nil
	}
	out := make([]oaiChamada, 0, len(calls))
	for _, c := range calls {
		args := c.RawInput
		if c.Input != nil {
			if b, err := json.Marshal(c.Input); err == nil {
				args = string(b)
			}
		}
		if args == "" {
			args = "{}"
		}
		out = append(out, oaiChamada{
			ID: c.ID, Type: "function",
			Function: oaiChamadaFuncao{Name: c.Name, Arguments: args},
		})
	}
	return out
}

// oaiTextoDeResultado é a metade DIFÍCIL da garantia 21.
//
// Este fornecedor não tem campo de erro na mensagem `tool` (D9): o booleano
// `IsError` da porta não tem onde caber. Em vez de perdê-lo — o que faria o
// modelo ler uma falha como saída normal e seguir afirmando o contrário do que
// aconteceu —, a marca vai no TEXTO, em maiúsculas e na primeira linha, que é
// onde o modelo a lê antes de qualquer outra coisa.
func oaiTextoDeResultado(r agent.ToolResult) string {
	if !r.IsError {
		return r.Content
	}
	return "ERRO NA FERRAMENTA:\n" + r.Content
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
	var chamadas []agent.ToolCall
	if len(out.Choices) > 0 {
		texto = out.Choices[0].Message.Content
		motivo = out.Choices[0].FinishReason
		for _, tc := range out.Choices[0].Message.ToolCalls {
			// ── D8, a linha que impede o turno de morrer por argumento ruim ──
			// `arguments` é STRING aqui. Ela pode não ser JSON válido, e nesse
			// caso a chamada SOBE mesmo assim, com Input nulo e o texto cru
			// junto: quem corrige o argumento é o modelo, e ele só corrige se
			// receber o erro de volta.
			c := agent.ToolCall{
				ID: tc.ID, Name: tc.Function.Name, RawInput: tc.Function.Arguments,
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &c.Input); err != nil || c.Input == nil {
				c.Input = nil
				avisos = append(avisos,
					"o modelo mandou argumentos ilegíveis para a ferramenta '"+
						tc.Function.Name+"': a chamada foi devolvida a ele como erro (D8)")
			}
			chamadas = append(chamadas, c)
		}
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
		ToolCalls:     chamadas,
		EffortApplied: aplicado,
		Capabilities:  o.Info().Capabilities,
		Warnings:      avisos,
	}, nil
}
