// Adaptador de agent.AgentProvider sobre a API da Anthropic.
//
// O que ESTE adaptador cumpre e o outro não (ver as divergências em
// internal/domain/agent/entity.go):
//
//   - CACHE DE PREFIXO EXPLÍCITO (D1): o prefixo estável vai como bloco de
//     `system` com `cache_control: ephemeral`. O breakpoint é o FIM DO PREFIXO, e
//     não o fim do prompt — pôr o marcador depois da mensagem do turno escreveria
//     uma entrada de cache nova a cada turno e não leria nenhuma. É o erro que
//     não falha, só cobra;
//   - CONTABILIDADE DE CRIAÇÃO DE CACHE (D1): `cache_creation_input_tokens` e
//     `cache_read_input_tokens` vêm separados, que é o que a telemetria da
//     ADR-0012 §1 precisa para acusar invalidador silencioso;
//   - OS CINCO NÍVEIS DE EFFORT (D4), incluindo o `max` que a ADR-0007 exige no
//     crítico.
//
// FERRAMENTAS, como este fornecedor as trata (D7–D11):
//
//   - D7: `tools: [{name, description, input_schema}]` — schema no TOPO do
//     objeto, sem casca. E a declaração vai ANTES de `system` no corpo, que é a
//     ordem canônica de cache deste fornecedor (tools → system → messages);
//   - D8: a chamada é um bloco `tool_use` DENTRO de `content`, ao lado dos
//     blocos de texto, e `input` já é OBJETO. Nada a decodificar — é o outro
//     adaptador que tem esse trabalho;
//   - D9: o resultado volta como bloco `tool_result` numa mensagem
//     `role:"user"`, VÁRIOS na mesma mensagem, com `is_error` booleano nativo;
//   - D10: paralelo por padrão, e o adaptador não manda flag nenhuma;
//   - D11: `tool_use_id` órfão é 400. Por isso a fala do assistente com as
//     chamadas é REENVIADA junto com os resultados, sempre.
//
// O que ele faz de diferente do óbvio:
//
//   - `thinking: adaptive`. O orçamento fixo de tokens de raciocínio
//     (`budget_tokens`) foi removido nos modelos atuais e devolve 400. Quem
//     controla profundidade é `output_config.effort`, que é justamente o segundo
//     eixo da ADR-0011 §3 — as duas decisões encaixam sem tradução;
//   - MENSAGEM DE OPERADOR COM RECUO. `role:"system"` no meio de `messages` é o
//     canal não-forjável e preserva o prefixo, mas não existe em todo modelo (o
//     Sonnet 5 responde 400). Em vez de manter uma lista de modelos que envelhece
//     em silêncio, o adaptador TENTA e, no 400 específico, refaz a chamada com a
//     instrução marcada dentro do turno do usuário — e AVISA. Recuar sem avisar
//     seria entregar como dado do usuário algo que autoriza.
package agentprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

const (
	NomeAnthropic       = "anthropic"
	BaseAnthropic       = "https://api.anthropic.com/v1"
	rotaAnthropic       = "/messages"
	versaoAPIAnthropic  = "2023-06-01"
	cabecalhoAnthropicK = "x-api-key"
)

// CatalogoAnthropic é o catálogo DESTE fornecedor: classe → nome concreto.
// Muda quando a Anthropic lança modelo; a política da ADR-0011 §3 não muda
// junto — é a separação política × catálogo de `cost/router.go`, continuada aqui.
func CatalogoAnthropic() map[agent.ModelClass]string {
	return map[agent.ModelClass]string{
		agent.ClassCheap:  "claude-haiku-4-5",
		agent.ClassMedium: "claude-sonnet-5",
		agent.ClassStrong: "claude-opus-5",
	}
}

// PrecosAnthropic é a tabela de preço, em MICROS por 1.000 tokens (USD).
//
// Leitura de cache a 0,1× do input e escrita a 1,25× — é essa razão que faz a
// economia da ADR-0012 valer a disciplina do prefixo estável, e é ela que precisa
// aparecer na medição.
//
// É tabela de PARTIDA e ENVELHECE: preço muda, e quando mudar é aqui que se mexe.
// Modelo fora dela devolve "desconhecido" em `PriceFor`, e o ciclo do turno DIZ
// que não sabe — nunca grava zero (ver `agent.ProviderInfo.PriceFor`).
func PrecosAnthropic() map[string]agent.Price {
	return map[string]agent.Price{
		"claude-opus-5":    {Currency: "USD", InputPer1k: 5_000, OutputPer1k: 25_000, CacheReadPer1k: 500, CacheCreationPer1k: 6_250},
		"claude-sonnet-5":  {Currency: "USD", InputPer1k: 3_000, OutputPer1k: 15_000, CacheReadPer1k: 300, CacheCreationPer1k: 3_750},
		"claude-haiku-4-5": {Currency: "USD", InputPer1k: 1_000, OutputPer1k: 5_000, CacheReadPer1k: 100, CacheCreationPer1k: 1_250},
	}
}

// paradaAnthropic traduz o motivo do fornecedor para o vocabulário do domínio (D5).
var paradaAnthropic = map[string]agent.StopReason{
	"end_turn":                      agent.StopCompleted,
	"stop_sequence":                 agent.StopCompleted,
	"max_tokens":                    agent.StopMaxTokens,
	"model_context_window_exceeded": agent.StopMaxTokens,
	"refusal":                       agent.StopRefused,
	"tool_use":                      agent.StopToolUse,
	// `pause_turn` é o modelo pedindo para continuar depois de uma ferramenta
	// de servidor. Sem ferramentas na porta ele não deveria aparecer; mapeado
	// para TOOL_USE porque é o que ele significa, e o domínio precisa saber
	// que a resposta NÃO terminou.
	"pause_turn": agent.StopToolUse,
}

type AnthropicConfig struct {
	// APIBase permite apontar para um gateway corporativo ou para um duplo de
	// teste sem tocar no ciclo do turno.
	APIBase string
	// APIKey é o valor JÁ RESOLVIDO da credencial de recurso (ADR-0013). Este
	// pacote não conhece `ports.SecretStore`.
	APIKey string
	// Catalog vazio usa CatalogoAnthropic(). Existe porque, quando o provedor
	// vier configurado no recurso da conta, o catálogo vem de lá.
	Catalog map[agent.ModelClass]string
	Timeout time.Duration
	Client  httpDoer
}

type Anthropic struct {
	c       *client
	catalog map[agent.ModelClass]string
}

func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	base := cfg.APIBase
	if base == "" {
		base = BaseAnthropic
	}
	autorizar := func(r *http.Request) {
		if cfg.APIKey != "" {
			r.Header.Set(cabecalhoAnthropicK, cfg.APIKey)
		}
		// Versão FIXADA no adaptador, não configurável: é a promessa de que o
		// formato da resposta não muda debaixo de nós. Deixá-la de fora
		// significa aceitar a versão default, que muda sozinha.
		r.Header.Set("anthropic-version", versaoAPIAnthropic)
		r.Header.Set("User-Agent", "dop-core")
	}
	cat := cfg.Catalog
	if len(cat) == 0 {
		cat = CatalogoAnthropic()
	}
	return &Anthropic{
		c:       newClient(base, NomeAnthropic, cfg.Client, cfg.Timeout, autorizar, cfg.APIKey),
		catalog: cat,
	}
}

var _ agent.AgentProvider = (*Anthropic)(nil)

// String: receptor por VALOR, para valer também em `%+v` de um valor.
func (a Anthropic) String() string { return "agentprovider.Anthropic{}" }

func (a *Anthropic) Info() agent.ProviderInfo {
	return agent.ProviderInfo{
		Name:    NomeAnthropic,
		Catalog: a.catalog,
		Capabilities: agent.Capabilities{
			agent.CapExplicitPrefixCache,
			agent.CapCacheCreationAccounting,
			agent.CapOperatorChannel,
			agent.CapFullEffortRange,
			agent.CapStructuredOutput,
			agent.CapToolUse,
		},
		Prices: PrecosAnthropic(),
	}
}

// ── formas do fornecedor (só o que a porta usa) ─────────────────────────────
//
// A ORDEM DOS CAMPOS destes structs é significativa: `encoding/json` serializa
// na ordem de declaração, e é ela que põe o PREFIXO antes das mensagens no corpo
// (garantia 3 da porta). Reordenar `System` para depois de `Messages` não
// quebraria nenhum teste de comportamento e custaria 10× na fatura — por isso a
// suíte de contrato verifica a ordem no corpo serializado.

type antBlocoTexto struct {
	Type         string       `json:"type"`
	Text         string       `json:"text"`
	CacheControl *antCacheCtl `json:"cache_control,omitempty"`
}

// antBloco é um bloco de `content` que pode ser texto, chamada ou resultado.
//
// Um struct com `omitempty` em tudo, e não três tipos com uma interface: os
// campos são disjuntos por `type`, e a alternativa faria cada montagem deste
// arquivo virar um type switch. `Input` é `any` porque o bloco de saída leva
// objeto e o de entrada não leva nada.
type antBloco struct {
	Type string `json:"type"`
	// texto
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// antFerramenta é a DECLARAÇÃO (D7): schema no topo, sem casca de "function".
type antFerramenta struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type antCacheCtl struct {
	Type string `json:"type"`
}

type antMensagem struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type antThinking struct {
	Type string `json:"type"`
}

type antFormato struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type antOutputConfig struct {
	Effort string      `json:"effort"`
	Format *antFormato `json:"format,omitempty"`
}

type antPedido struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// Tools vem ANTES de System, e não é estética: a ordem canônica de cache
	// deste fornecedor é tools → system → messages, e a suíte de contrato
	// audita a ordem no corpo SERIALIZADO (garantia 15). Declaração depois da
	// conversa não erra — só sai do trecho cacheável e cobra por isso.
	//
	// `omitempty` é a garantia 16: turno sem ferramenta não manda o campo.
	// Array vazio ocupa lugar no prompt e convida o modelo a chamar o que não
	// existe.
	Tools        []antFerramenta `json:"tools,omitempty"`
	System       []antBlocoTexto `json:"system"`
	Messages     []antMensagem   `json:"messages"`
	Thinking     antThinking     `json:"thinking"`
	OutputConfig antOutputConfig `json:"output_config"`
}

type antResposta struct {
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// D8: a chamada vem DENTRO de content, e `input` já é objeto.
		// `json.RawMessage` e não `map[string]any` porque um `input` que não
		// seja objeto (o caso degenerado) precisa chegar como texto cru ao
		// modelo, e não sumir num erro de decodificação.
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// ── montagem ────────────────────────────────────────────────────────────────

func (a *Anthropic) Render(t agent.Turn, model string, effort agent.Effort) ([]byte, []string, error) {
	return a.render(t, model, effort, false)
}

// render monta a requisição. A CREDENCIAL NÃO ENTRA AQUI: ela vai em cabeçalho,
// pelo closure de autorização — o que faz de `render` uma superfície segura para
// log e para a suíte de contrato inspecionar.
//
// `operatorInline` é o RECUO do D3, e é interno de propósito: quem chama a porta
// não escolhe canal de operador, ele é consequência do modelo.
func (a *Anthropic) render(t agent.Turn, model string, effort agent.Effort,
	operatorInline bool) ([]byte, []string, error) {

	var avisos []string
	var conteudoUsuario []antBlocoTexto
	var mensagens []antMensagem

	for _, m := range t.Messages {
		switch {
		case m.Role == agent.RoleOperator && !operatorInline:
			// Canal próprio: `role:"system"` DEPOIS da história preserva o
			// prefixo cacheado (D3). NUNCA `role:"user"`.
			mensagens = append(mensagens, antMensagem{Role: "system", Content: m.Text})
		case m.Role == agent.RoleOperator:
			conteudoUsuario = append(conteudoUsuario, antBlocoTexto{
				Type: "text",
				Text: "<intervencao-do-operador>\n" + m.Text + "\n</intervencao-do-operador>",
			})
			avisos = append(avisos,
				"modelo sem canal de operador nativo: a instrução foi marcada dentro "+
					"do turno do usuário (D3)")
		case m.Role == agent.RoleToolResult:
			// D9: os resultados vão como blocos `tool_result` numa mensagem
			// `role:"user"` — VÁRIOS na mesma, que é o formato deste
			// fornecedor. Mensagem PRÓPRIA e não misturada ao turno do
			// usuário: saída de ferramenta é conteúdo não confiável (spec do
			// substrato §6) e não pode ser confundida com a pergunta de quem
			// pediu o trabalho.
			mensagens = append(mensagens, antMensagem{
				Role: "user", Content: antResultados(m.ToolResults),
			})
		case m.Role == agent.RoleAssistant:
			// D11: a fala do assistente REENVIA as chamadas junto. Sem elas, o
			// `tool_use_id` do resultado seguinte fica órfão e o fornecedor
			// devolve 400.
			mensagens = append(mensagens, antMensagem{
				Role: "assistant", Content: antConteudoAssistente(m),
			})
		default:
			conteudoUsuario = append(conteudoUsuario, antBlocoTexto{Type: "text", Text: m.Text})
		}
	}

	if len(conteudoUsuario) > 0 {
		// O turno do usuário entra ANTES de qualquer mensagem de operador já
		// enfileirada: a mensagem `system` do meio precisa seguir um turno de
		// usuário, e é a última entrada de `messages`.
		mensagens = append([]antMensagem{{Role: "user", Content: conteudoUsuario}}, mensagens...)
	}

	pedido := antPedido{
		Model:     model,
		MaxTokens: t.MaxOutputTokens,
		Tools:     antFerramentas(t.Tools),
		// O PREFIXO, com o breakpoint no FIM DELE — e não no fim do prompt.
		System: []antBlocoTexto{{
			Type:         "text",
			Text:         t.StablePrefix,
			CacheControl: &antCacheCtl{Type: "ephemeral"},
		}},
		Messages:     mensagens,
		Thinking:     antThinking{Type: "adaptive"},
		OutputConfig: antOutputConfig{Effort: string(effort)},
	}
	if t.OutputSchema != nil {
		pedido.OutputConfig.Format = &antFormato{Type: "json_schema", Schema: t.OutputSchema}
	}

	// json.Marshal ordena as chaves de MAPA alfabeticamente e mantém os campos
	// de STRUCT na ordem de declaração — as duas coisas são determinísticas, que
	// é a garantia 4. É por isso que o schema pode ser mapa sem custar o cache.
	corpo, err := json.Marshal(pedido)
	if err != nil {
		return nil, nil, agent.Unavailability(NomeAnthropic, agent.ReasonProviderError,
			"pedido ilegível: "+err.Error())
	}
	return corpo, avisos, nil
}

// antFerramentas traduz a declaração do domínio para a forma deste fornecedor
// (D7). Lista vazia devolve nil, e o `omitempty` do pedido faz o resto: campo
// AUSENTE, não array vazio (garantia 16).
func antFerramentas(specs []agent.ToolSpec) []antFerramenta {
	if len(specs) == 0 {
		return nil
	}
	out := make([]antFerramenta, 0, len(specs))
	for _, s := range specs {
		out = append(out, antFerramenta{
			Name: s.Name, Description: s.Description, InputSchema: s.InputSchema,
		})
	}
	return out
}

// antConteudoAssistente monta a fala do assistente com as chamadas junto (D11).
//
// O texto vem ANTES das chamadas porque é a ordem em que o modelo os produziu, e
// remontar a história fora de ordem faz o modelo ler o próprio raciocínio ao
// contrário.
func antConteudoAssistente(m agent.Message) []antBloco {
	blocos := make([]antBloco, 0, len(m.ToolCalls)+1)
	if strings.TrimSpace(m.Text) != "" {
		blocos = append(blocos, antBloco{Type: "text", Text: m.Text})
	}
	for _, c := range m.ToolCalls {
		entrada := c.Input
		if entrada == nil {
			// Chamada que veio ilegível (D8) é reenviada com objeto VAZIO, e
			// não omitida: o `tool_use_id` precisa existir para o resultado de
			// erro correspondente ter par (D11). Omitir a chamada e mandar o
			// resultado é o 400 clássico.
			entrada = map[string]any{}
		}
		blocos = append(blocos, antBloco{
			Type: "tool_use", ID: c.ID, Name: c.Name, Input: entrada,
		})
	}
	return blocos
}

// antResultados monta os blocos `tool_result` (D9). O `is_error` é NATIVO aqui —
// é o fornecedor que tem o booleano, e usá-lo é a metade fácil da garantia 21.
func antResultados(rs []agent.ToolResult) []antBloco {
	blocos := make([]antBloco, 0, len(rs))
	for _, r := range rs {
		blocos = append(blocos, antBloco{
			Type: "tool_result", ToolUseID: r.CallID, Content: r.Content, IsError: r.IsError,
		})
	}
	return blocos
}

// ── envio ───────────────────────────────────────────────────────────────────

func (a *Anthropic) Send(ctx context.Context, t agent.Turn, model string,
	effort agent.Effort) (*agent.Reply, error) {

	if !a.c.temCredencial {
		// Sem chave não se gasta uma ida à rede: o erro fala de configuração,
		// que é o que é.
		return nil, agent.Unavailability(NomeAnthropic, agent.ReasonMissingCredential, "")
	}

	corpo, avisos, err := a.render(t, model, effort, false)
	if err != nil {
		return nil, err
	}
	status, resp, err := a.c.post(ctx, rotaAnthropic, corpo)
	if err != nil {
		return nil, err
	}

	if status == http.StatusBadRequest && ehCanalDeOperador(resp) {
		// Recuo documentado (D3): este modelo não aceita `role:"system"` no
		// meio. Refaz com a instrução marcada no turno do usuário. UMA vez —
		// um segundo 400 é 400 de verdade.
		corpo, avisos, err = a.render(t, model, effort, true)
		if err != nil {
			return nil, err
		}
		status, resp, err = a.c.post(ctx, rotaAnthropic, corpo)
		if err != nil {
			return nil, err
		}
	}
	if status >= 400 {
		return nil, a.c.falha(status, resp)
	}

	var out antResposta
	if err := json.Unmarshal(resp, &out); err != nil {
		// Resposta ilegível é INDISPONIBILIDADE, não defeito nosso: quase
		// sempre é um proxy ou portal de autenticação respondendo HTML no
		// lugar do fornecedor.
		return nil, agent.Unavailability(NomeAnthropic, agent.ReasonProviderError,
			"resposta ilegível: "+a.c.redact(err.Error()))
	}

	var texto strings.Builder
	var chamadas []agent.ToolCall
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			texto.WriteString(b.Text)
		case "tool_use":
			// D8: aqui o `input` já é objeto — a decodificação é uma linha, e
			// não a armadilha que ela é no outro adaptador. O caso degenerado
			// existe mesmo assim (um `input` que não é objeto), e a resposta é
			// a mesma dos dois lados: a chamada SOBE com Input nulo e um aviso,
			// para o laço devolver o erro ao modelo em vez de matar o turno.
			c := agent.ToolCall{ID: b.ID, Name: b.Name, RawInput: string(b.Input)}
			if err := json.Unmarshal(b.Input, &c.Input); err != nil || c.Input == nil {
				c.Input = nil
				avisos = append(avisos,
					"o modelo mandou argumentos ilegíveis para a ferramenta '"+b.Name+
						"': a chamada foi devolvida a ele como erro (D8)")
			}
			chamadas = append(chamadas, c)
		}
	}

	parada, ok := paradaAnthropic[out.StopReason]
	if !ok {
		// Motivo novo do fornecedor NÃO vira erro: parar de trabalhar por causa
		// de uma string desconhecida seria pior que registrar que ela apareceu.
		parada = agent.StopUnknown
	}

	return &agent.Reply{
		Text: texto.String(),
		// As três parcelas de entrada são DISJUNTAS neste fornecedor (D2):
		// `input_tokens` já EXCLUI o que veio do cache. Nada a subtrair aqui —
		// e é justamente o adaptador OpenAI que precisa subtrair.
		Usage: agent.Usage{
			InputTokens:         out.Usage.InputTokens,
			OutputTokens:        out.Usage.OutputTokens,
			CacheReadTokens:     out.Usage.CacheReadInputTokens,
			CacheCreationTokens: out.Usage.CacheCreationInputTokens,
		},
		Model:      out.Model,
		Provider:   NomeAnthropic,
		StopReason: parada,
		Data:       decodificarTexto(texto.String()),
		ToolCalls:  chamadas,
		// Os cinco níveis existem aqui: o effort pedido é o aplicado (D4).
		EffortApplied: effort,
		Capabilities:  a.Info().Capabilities,
		Warnings:      avisos,
	}, nil
}

// ehCanalDeOperador reconhece o 400 específico de "este modelo não aceita
// mensagem de sistema no meio".
//
// É uma APOSTA sobre um texto que a Anthropic não publica, e está escrito aqui
// para que fique explícito onde ela está. O custo de errar para MENOS é um turno
// que falha com 400 em vez de recuar; o custo de errar para MAIS é uma segunda
// chamada desnecessária que provavelmente falha igual. Nenhum dos dois corrompe
// nada — e é por isso que a heurística é aceitável aqui e não seria, por exemplo,
// para decidir se um merge conflitou.
func ehCanalDeOperador(corpo []byte) bool {
	t := strings.ToLower(string(corpo))
	return strings.Contains(t, "role") && strings.Contains(t, "system")
}
