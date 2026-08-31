// Package agent é o RUNTIME do agente: o turno de conversa com o modelo, do
// contexto ao achado publicado (ADR-0023).
//
// Ele viveu no BFF (ADR-0016) e voltou para cá por uma razão de segurança e uma
// de desenho. A de segurança: o runtime precisa da credencial do provedor, que
// mora no cofre, no núcleo, e o núcleo NUNCA devolve segredo — dar cofre ao BFF
// foi vetado, porque ele é a camada exposta à internet. Com o runtime aqui, a
// credencial não atravessa a rede. A de desenho: as seis coisas que um turno faz
// — montar contexto, rotear modelo, registrar consumo, postar mensagem, publicar
// achado, respeitar orçamento — já eram todas operações do núcleo, feitas de fora
// por gRPC. O runtime estava do lado errado da fronteira.
//
// Regra da casa, valendo aqui como em todo domínio: este pacote não conhece
// Postgres, gRPC, SDK de fornecedor nem `ports.SecretStore`. Ele declara o que
// precisa como PORTA (repository.go) e o composition root liga. Em especial: ele
// não conhece `knowledge`, `cost` nem `demand` — fala com os três por portas
// estreitas declaradas aqui, no vocabulário DELE.
//
// Dois módulos da implementação Python NÃO atravessaram, e a ADR-0023 explica
// por quê: `credentials.py` existia contornando um cofre inacessível (aqui a
// credencial sai de `ports.SecretStore` no composition root, como no git — ver
// internal/app/agentproviders.go), e `catalog.py` existia refazendo o caminho do
// NOME do modelo de volta à CLASSE, porque a decisão de roteamento cruzava a rede
// e perdia a classe no caminho. Em processo, `Decision.Class` chega inteira e o
// caminho de volta deixa de existir.
//
// ════════════════════════════════════════════════════════════════════════════
// ONDE OS FORNECEDORES DIVERGEM  (o trabalho de verdade — ADR-0022)
// ════════════════════════════════════════════════════════════════════════════
//
// Cada divergência abaixo tem subteste próprio em test/contract/agentprovider.go,
// rodando contra TODOS os adaptadores. Elas mudaram de linguagem, não de desenho.
//
// D1 — SEMÂNTICA DE CACHE DE PREFIXO. É a mais cara, porque a ADR-0012 depende
// dela. A Anthropic tem cache EXPLÍCITO: um `cache_control` marca o fim do
// prefixo, TTL de 5 min, leitura a ~0,1× do input e escrita a 1,25×, e a resposta
// separa `cache_read_input_tokens` de `cache_creation_input_tokens`. A OpenAI tem
// cache AUTOMÁTICO: sem breakpoint, sem TTL controlável, e a resposta informa só
// `cached_tokens` (leitura) — CRIAÇÃO de cache não é reportada. Portanto
// `Usage.CacheCreationTokens` é sempre 0 no adaptador OpenAI, e isso NÃO significa
// "nada foi escrito no cache": significa "não dá para saber". O alerta de
// invalidador silencioso da ADR-0012 §1 (leitura de cache zerada em prefixo
// estável — ver `cost.UsageEvent.SuspectCacheMiss`) só é FIEL sob um provedor com
// contabilidade explícita. `CapCacheCreationAccounting` diz quem tem, e a
// capacidade viaja no `Reply` para que quem lê a telemetria saiba o que o número
// significa em vez de descobrir na conciliação.
//
// D2 — CONTAGEM DE TOKENS: A ARMADILHA DA DUPLA CONTAGEM. Na Anthropic,
// `input_tokens` EXCLUI o que veio do cache — as três parcelas de entrada são
// disjuntas e somam o total. Na OpenAI, `prompt_tokens` INCLUI os cacheados e
// `cached_tokens` é um SUBCONJUNTO dele. Somar os campos da OpenAI como se fossem
// disjuntos infla a medição da ADR-0011 sem que nada falhe. A porta NORMALIZA
// para a semântica disjunta, que é a que `cost.UsageEvent` assume: o adaptador
// OpenAI subtrai, e essa é a linha mais importante daquele arquivo.
//
// D3 — CANAL DO OPERADOR. A intervenção do operador precisa entrar no meio da
// conversa sem reescrever o topo do prompt (ADR-0012 §1). A Anthropic tem
// mensagem `role:"system"` no meio de `messages` — nos modelos Opus 5/4.8 e
// Fable/Mythos, e NÃO no Sonnet 5, que responde 400. A OpenAI tem
// `role:"developer"`, aceito em qualquer posição. A porta expõe `RoleOperator` e
// cada adaptador resolve — inclusive o RECUO para bloco marcado dentro do turno
// do usuário quando o modelo recusa o canal, sempre com aviso. O que a porta
// GARANTE é que a instrução do operador nunca é atribuída ao usuário: é ela que
// autoriza, e achatar as duas é abrir a porta para injeção de prompt.
//
// D4 — EFFORT. O vocabulário do núcleo é `low|medium|high|xhigh|max` (ADR-0011
// §3, espelhado em `cost.Effort`). A Anthropic aceita os cinco; a OpenAI aceita
// três. O adaptador MAPEIA e declara o que aplicou em `Reply.EffortApplied` —
// nunca finge que aplicou `max`. Numa tarefa crítica (ADR-0007: não se economiza
// no crítico) essa diferença é decisão de produto, não detalhe, e por isso sai
// também como aviso legível em `Reply.Warnings`.
//
// D5 — MOTIVO DE PARADA. Anthropic: `end_turn | max_tokens | stop_sequence |
// tool_use | pause_turn | refusal | model_context_window_exceeded`. OpenAI:
// `stop | length | tool_calls | content_filter`. Normalizados em `StopReason` —
// o domínio precisa distinguir "terminou", "foi cortado" e "recusou", e só isso.
//
// D6 — INDISPONIBILIDADE. As duas famílias de erro não se parecem em nada. As
// duas viram `*Unavailable` (ver abaixo), porque indisponibilidade de terceiro
// não pode chegar ao cliente como erro nosso: um erro interno genérico manda a
// equipe errada investigar e manda o usuário esperar um conserto que não existe.
//
// ════════════════════════════════════════════════════════════════════════════
// FERRAMENTAS: OS CINCO EIXOS EM QUE OS DOIS DIVERGEM  (D7–D11)
// ════════════════════════════════════════════════════════════════════════════
//
// Ferramentas ficaram FORA da porta na entrega anterior, e a razão registrada
// era boa: "os formatos divergem em três eixos ao mesmo tempo, e um laço escrito
// sobre a média dos dois seria um laço que nenhum dos dois executa bem". A razão
// continua verdadeira — o que mudou é que os eixos foram nomeados um a um, cada
// um ganhou subteste de contrato nos DOIS adaptadores, e a normalização deixou
// de ser média para virar tradução. Eram três; olhando de perto, são cinco.
//
// D7 — DECLARAÇÃO. A Anthropic põe o schema NO TOPO do objeto da ferramenta:
// `{name, description, input_schema}`. A OpenAI aninha: `{type:"function",
// function:{name, description, parameters, strict}}` — nome diferente para o
// mesmo campo (`parameters`, não `input_schema`), um nível a mais de aninhamento
// e uma chave `type` que só existe lá. O terreno comum é NOME + DESCRIÇÃO +
// SCHEMA, e é exatamente isso que `ToolSpec` carrega; cada adaptador monta a
// casca dele. Consequência que vale registrar: a declaração é PREFIXO — muda por
// thread, não por turno —, e por isso ela vai ANTES das mensagens no corpo, do
// mesmo jeito que o prefixo estável (garantia 15 da porta).
//
// D8 — A CHAMADA NA RESPOSTA. Aqui a diferença não é de nome, é de TIPO. Na
// Anthropic a chamada é um bloco `tool_use` DENTRO de `content`, ao lado dos
// blocos de texto, e `input` já é um OBJETO JSON decodificado. Na OpenAI a
// chamada vem num array `tool_calls` FORA de `content`, e `function.arguments` é
// uma STRING que ainda precisa de `json.Unmarshal`. A armadilha é o caso
// degenerado: o modelo pode emitir uma string que não é JSON válido. Ali, falhar
// o turno seria a resposta errada — quem consegue corrigir o argumento é o
// MODELO, e ele só corrige se receber o erro de volta. Por isso a porta manda a
// chamada para cima com `Input` nulo e um aviso, e o laço responde com um
// resultado de erro. É o gêmeo exato da regra do D2: o adaptador absorve a forma
// do fornecedor e entrega a semântica do domínio.
//
// D9 — O RESULTADO DE VOLTA. A Anthropic recebe o resultado como bloco
// `tool_result` dentro de uma mensagem `role:"user"` — vários resultados cabem
// na MESMA mensagem, e há um booleano `is_error`. A OpenAI recebe UMA mensagem
// `role:"tool"` POR resultado, com `tool_call_id`, e NÃO tem campo de erro:
// falha de ferramenta é texto como qualquer outro. Duas consequências: o número
// de mensagens no corpo difere entre os fornecedores para o mesmo turno do
// domínio (por isso a suíte compara conteúdo, nunca contagem), e onde o booleano
// não existe o adaptador PREFIXA o conteúdo de forma legível — perder a marca de
// erro faria o modelo ler uma falha como saída normal e seguir em frente.
//
// D10 — PARALELISMO. Os dois emitem várias chamadas por volta, e por padrão. A
// Anthropic desliga por `tool_choice.disable_parallel_tool_use`; a OpenAI por um
// campo de topo, `parallel_tool_calls`. O padrão é o que queremos — várias
// chamadas numa volta é uma volta a menos, e volta custa o turno inteiro
// reenviado —, e nenhum dos dois adaptadores manda a flag. O que a porta garante
// é a ORDEM: as chamadas chegam a `Reply.ToolCalls` na ordem em que o fornecedor
// as emitiu, e os resultados voltam na ordem das chamadas. Reordenar é o que
// transforma "rodei o teste e depois li o log" em "li o log e depois rodei o
// teste" na leitura do modelo.
//
// D11 — FERRAMENTA NÃO DECLARADA. Nenhum dos dois impede o modelo de chamar um
// nome que não foi declarado — os dois validam o CORPO que enviamos, não a
// imaginação do modelo. E os dois recusam com 400 o inverso: um resultado cujo
// id de chamada eles não conhecem (`tool_use_id` órfão na Anthropic,
// `tool_call_id` sem par na OpenAI). Daí as duas regras do laço, que são
// simétricas: chamada de ferramenta desconhecida vira RESULTADO DE ERRO com o
// id que o fornecedor mandou — nunca erro do turno, nunca resultado silencioso —,
// e resultado nenhum é inventado sem uma chamada que o justifique.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── vocabulário do domínio ───────────────────────────────────────────────────

// ModelClass é CLASSE de modelo, não nome — espelha `cost.ModelClass`.
//
// Nome de modelo muda de líder por semestre (ADR-0001) e não sobrevive a uma
// tabela de política; a classe é o que a decisão significa. Quem resolve classe →
// nome concreto é o CATÁLOGO, e catálogo é POR FORNECEDOR: a classe forte da
// Anthropic e a da OpenAI são modelos diferentes, com preços e limites
// diferentes, e a política de `cost/router.go` não deveria nem saber disso.
type ModelClass string

const (
	ClassCheap  ModelClass = "cheap"
	ClassMedium ModelClass = "medium"
	ClassStrong ModelClass = "strong"
)

// Effort é o esforço de raciocínio, no vocabulário do núcleo (ADR-0011 §3).
// Os cinco valores são os mesmos de `cost.Effort` — string a string, de
// propósito: a cola entre os dois domínios é troca de tipo nomeado, não
// tradução, e no dia em que divergirem é melhor que quebre na cola.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// ValidEffort responde se o valor está no vocabulário. Valor de fora NÃO para
// trabalho — ver NormalizeEffort.
func ValidEffort(e Effort) bool {
	switch e {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return true
	}
	return false
}

// NormalizeEffort cai no ALTO quando não reconhece, e não no baixo: a ADR-0011
// §3 já decidiu que, na dúvida, não se economiza — errar para caro aparece na
// medição, errar para barato aparece em retrabalho, que não aparece em lugar
// nenhum.
func NormalizeEffort(e Effort) Effort {
	if ValidEffort(e) {
		return e
	}
	return EffortHigh
}

// Role é quem fala. `RoleOperator` é canal PRÓPRIO, e a distinção é de
// segurança: instrução de operador tem autoridade, texto de usuário não tem.
// Achatar as duas num só papel é o caminho clássico de injeção de prompt — quem
// escreve numa entrada de usuário passa a poder forjar instrução. Ver D3.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleOperator  Role = "operator"
	// RoleToolResult é o que a FERRAMENTA devolveu. Papel próprio, e não fala
	// de usuário, pela mesma razão de segurança do RoleOperator invertida: a
	// saída de um comando é conteúdo NÃO CONFIÁVEL (spec do substrato §6) e não
	// pode chegar ao modelo com a autoridade de quem pediu o trabalho. Um
	// `README` malicioso lido por `cat` não pode virar instrução.
	RoleToolResult Role = "tool_result"
)

// StopReason é por que o modelo parou, no vocabulário do DOMÍNIO (ver D5).
//
// Quatro coisas o domínio precisa distinguir: terminou, foi cortado, recusou e
// PEDIU FERRAMENTA. O quarto já existia aqui antes de ferramentas entrarem na
// porta — foi escrito para que o dia da entrada não mudasse este vocabulário, e
// esse dia chegou sem mudá-lo. `StopUnknown` existe para que motivo novo de
// fornecedor não vire erro — parar de trabalhar por causa de uma string
// desconhecida seria pior que registrar que ela apareceu.
type StopReason string

const (
	StopCompleted StopReason = "completed"
	StopMaxTokens StopReason = "max_tokens"
	StopRefused   StopReason = "refused"
	StopToolUse   StopReason = "tool_use"
	StopUnknown   StopReason = "unknown"
)

// Capability é o que ESTE adaptador consegue cumprir — as divergências, como
// DADO. Existe para que quem lê a telemetria saiba o que o número significa.
type Capability string

const (
	// CapExplicitPrefixCache: o prefixo cacheado é marcado explicitamente
	// (breakpoint), e não adivinhado pelo fornecedor.
	CapExplicitPrefixCache Capability = "explicit_prefix_cache"
	// CapCacheCreationAccounting: a resposta separa CRIAÇÃO de cache de
	// LEITURA de cache (ADR-0012 §1). Sem ela, zero em CacheCreationTokens
	// significa "não dá para saber", nunca "nada foi escrito".
	CapCacheCreationAccounting Capability = "cache_creation_accounting"
	// CapOperatorChannel: instrução de operador tem canal próprio no
	// protocolo do fornecedor (D3).
	CapOperatorChannel Capability = "operator_channel"
	// CapFullEffortRange: os cinco níveis da ADR-0011 §3, sem rebaixamento.
	CapFullEffortRange Capability = "full_effort_range"
	// CapStructuredOutput: saída estruturada validada pelo fornecedor —
	// o achado terso da ADR-0012 §2, sem re-parse do nosso lado.
	CapStructuredOutput Capability = "structured_output"
	// CapToolUse: o adaptador declara ferramentas, lê a chamada e devolve o
	// resultado (D7–D11). Os DOIS fornecedores a têm — e ela existe mesmo
	// assim, porque capacidade é DADO: um terceiro adaptador que não
	// implementasse o laço precisa poder dizer isso em vez de deixar o runtime
	// declarar ferramentas que ninguém executa.
	CapToolUse Capability = "tool_use"
)

// Capabilities é o conjunto, como slice ordenado e não como mapa: ele viaja no
// `Reply`, entra em log e em telemetria, e mapa em Go itera aleatoriamente —
// saída que muda de ordem entre execuções é impossível de casar em teste e
// irritante de ler.
type Capabilities []Capability

func (c Capabilities) Has(want Capability) bool {
	for _, got := range c {
		if got == want {
			return true
		}
	}
	return false
}

// ── ferramentas (D7–D11) ─────────────────────────────────────────────────────

// ToolSpec é a DECLARAÇÃO de uma ferramenta, no terreno comum dos dois
// fornecedores (D7): nome, descrição e schema de entrada.
//
// Não há campo para a casca de cada fornecedor — nem `type:"function"`, nem
// `strict` — de propósito: o dia em que um deles aparecer aqui, a porta terá
// virado o formato de um fornecedor com outro nome.
type ToolSpec struct {
	Name        string
	Description string
	// InputSchema é JSON Schema. Mapa, e não struct, porque o schema é dado do
	// catálogo de ferramentas e não vocabulário desta porta — e `json.Marshal`
	// ordena chave de mapa alfabeticamente, o que mantém a serialização
	// determinística (garantia 4).
	InputSchema map[string]any
}

// ToolCall é o que o modelo PEDIU.
type ToolCall struct {
	// ID é opaco e vem do fornecedor. Ele é o que liga a chamada ao resultado
	// (D11): inventar um id aqui faria os dois fornecedores recusarem o turno
	// seguinte com 400.
	ID   string
	Name string
	// Input é o argumento JÁ DECODIFICADO. Nulo significa que o fornecedor
	// mandou algo que não decodifica — na OpenAI, uma string que não é JSON
	// (D8). Nulo NÃO é "sem argumentos": `map[string]any{}` é isso.
	Input map[string]any
	// RawInput é o argumento como veio, para quando `Input` é nulo. Ele entra
	// no resultado de erro devolvido ao modelo — sem ele, o modelo receberia
	// "seu argumento é inválido" sem saber qual argumento.
	RawInput string
}

// ToolResult é o que a execução DEVOLVEU, pronto para voltar ao modelo.
type ToolResult struct {
	// CallID casa com ToolCall.ID.
	CallID string
	Name   string
	// Content é o que o modelo vai LER. Texto, sempre: os dois fornecedores
	// aceitam texto no resultado, e só um aceita estrutura.
	Content string
	// IsError diz que a ferramenta FALHOU. É campo próprio, e não um prefixo
	// no texto, porque um dos fornecedores tem o booleano nativo — jogar fora a
	// distinção para caber no menor dos dois seria o denominador comum que a
	// ADR-0001 recusa. Quem não tem o campo escreve a marca no texto (D9).
	IsError bool
}

// ── a conversa ───────────────────────────────────────────────────────────────

// Message é uma fala: a parte VOLÁTIL da conversa, que vai depois do breakpoint.
//
// Os três campos extras são exclusivos por papel, e essa exclusividade é o
// contrato: `ToolCalls` só em RoleAssistant, `ToolResults` só em RoleToolResult.
// Um struct com os três em vez de três tipos é escolha deliberada — a alternativa
// (interface + type switch) espalharia o conhecimento da forma da conversa por
// todos os adaptadores, que é justamente o que a porta existe para evitar.
type Message struct {
	Role Role
	Text string
	// ToolCalls é o que o modelo pediu na volta ANTERIOR, reenviado como
	// história. Sem ele, o fornecedor recebe um resultado sem a chamada que o
	// justifica e recusa o turno (D11).
	ToolCalls []ToolCall
	// ToolResults são os resultados daquelas chamadas, na ORDEM delas (D10).
	ToolResults []ToolResult
}

// Micros é valor monetário em 10^-6 da unidade da moeda — espelha `cost.Micros`.
//
// Dinheiro não é float aqui pela mesma razão aritmética de lá: soma de milhões
// de linhas em ponto flutuante acumula erro, e orçamento que erra não é
// orçamento. Repetir o tipo — em vez de importar `cost` — é o preço de o runtime
// não conhecer o domínio de custo; a cola converte, e a conversão é troca de tipo
// nomeado, não cálculo.
type Micros int64

// Usage é o consumo de UMA chamada, com as quatro parcelas DISJUNTAS (ver D2).
//
// Disjuntas é o CONTRATO: InputTokens não inclui o que veio do cache.
// `cost.UsageEvent` assume isso — `PromptTokens()` lá soma as três parcelas de
// entrada —, e um adaptador que devolvesse a contagem inclusiva inflaria o
// orçamento da ADR-0011 em silêncio.
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// Price é o preço de um modelo, em MICROS por 1.000 tokens, com a moeda junto.
//
// Por 1.000 e não por token: a leitura de cache custa 0,1× do input, e um preço
// por token viraria fração — que em inteiro some e em float mente. Toda a
// aritmética daqui é inteira, do começo ao fim.
//
// Por que o preço mora no ADAPTADOR: é ele que sabe qual fornecedor cobrou o quê.
// `cost.UsageEvent.CostMicros` é preenchido por quem chamou o modelo justamente
// por isso — o domínio de custo não recalcula preço de terceiro, ele registra o
// que foi gasto.
type Price struct {
	Currency           string
	InputPer1k         Micros
	OutputPer1k        Micros
	CacheReadPer1k     Micros
	CacheCreationPer1k Micros
}

// CostMicros é o custo desta chamada. Divisão inteira no FIM, e só uma vez:
// arredondar parcela a parcela jogaria fora centésimos de micro em cada linha, e
// o acumulado do mês ficaria sistematicamente abaixo da fatura.
func (p Price) CostMicros(u Usage) Micros {
	total := Micros(u.InputTokens)*p.InputPer1k +
		Micros(u.OutputTokens)*p.OutputPer1k +
		Micros(u.CacheReadTokens)*p.CacheReadPer1k +
		Micros(u.CacheCreationTokens)*p.CacheCreationPer1k
	return total / 1000
}

// Turn é a conversa a enviar: prefixo estável primeiro, volátil depois.
//
// A separação é a ADR-0012 §1 virada TIPO. Enquanto StablePrefix for um campo
// próprio, ninguém interpola o texto do turno lá dentro por distração — que é o
// pior jeito de perder a economia de cache, porque não falha, só fica caro.
type Turn struct {
	StablePrefix string
	Messages     []Message
	// OutputSchema é o schema JSON da resposta: achado terso e validado pelo
	// fornecedor (ADR-0012 §2). Nulo = texto livre.
	OutputSchema map[string]any
	// Tools são as ferramentas DECLARADAS neste turno (D7). Vazio = o agente só
	// conversa, e o adaptador NÃO manda o campo — array vazio no corpo é
	// diferente de campo ausente, e mandá-lo convidaria o modelo a chamar o que
	// não existe (garantia 16).
	//
	// A ordem importa e é do CATÁLOGO, já ordenada por nome (ver tools.go): a
	// declaração é parte estável do prompt, e uma lista que muda de ordem entre
	// turnos invalida o prefixo cacheado do mesmo jeito que um mapa iterado.
	Tools           []ToolSpec
	MaxOutputTokens int
}

// Fingerprint é a impressão digital do PREFIXO, e só dele.
//
// Dois turnos da mesma thread têm de dar a mesma impressão, mesmo com mensagens
// diferentes: é isso que significa "o prefixo está estável", e é o que a suíte de
// contrato compara entre turnos.
func (t Turn) Fingerprint() string {
	h := sha256.Sum256([]byte(t.StablePrefix))
	return hex.EncodeToString(h[:])
}

// Reply é a resposta, em tipos do DOMÍNIO. Nenhum objeto de fornecedor
// atravessa daqui para cima.
type Reply struct {
	Text       string
	Usage      Usage
	Model      string
	Provider   string
	StopReason StopReason
	// Data é a saída estruturada já decodificada, quando houve OutputSchema.
	Data map[string]any
	// ToolCalls são as ferramentas que o modelo pediu, NA ORDEM em que o
	// fornecedor as emitiu (D10). Vazio quando ele só falou.
	ToolCalls []ToolCall
	// EffortApplied é o effort REALMENTE aplicado. Pode ser menor que o
	// pedido — ver D4. Nunca é o pedido "por educação".
	EffortApplied Effort
	// Capabilities é o que este provedor cumpre. Viaja junto para que a
	// telemetria seja legível sem consultar o código do adaptador.
	Capabilities Capabilities
	// Warnings são avisos legíveis: effort rebaixado, canal de operador
	// recuado, contabilidade de cache ausente.
	Warnings []string
}

// ProviderInfo é a ficha do adaptador — o que a borda mostra e o relatório
// audita. Sem I/O: é ficha, não consulta.
type ProviderInfo struct {
	Name         string
	Catalog      map[ModelClass]string
	Capabilities Capabilities
	// Prices é por nome CONCRETO de modelo. Modelo ausente = preço
	// DESCONHECIDO, e desconhecido não vira zero em silêncio — ver PriceFor.
	Prices map[string]Price
}

// ResolveModel traduz CLASSE → nome concreto pelo catálogo DESTE fornecedor.
//
// Classe fora do catálogo não para trabalho: cai na classe forte. É a mesma
// escolha do `Router.Route` do domínio de custo — na dúvida não se economiza,
// porque o gasto aparece na medição e o retrabalho não.
func (p ProviderInfo) ResolveModel(c ModelClass) string {
	if nome, ok := p.Catalog[c]; ok && nome != "" {
		return nome
	}
	return p.Catalog[ClassStrong]
}

func (p ProviderInfo) Supports(c Capability) bool { return p.Capabilities.Has(c) }

// PriceFor devolve o preço deste modelo e se ele é CONHECIDO.
//
// O booleano, e não um preço zerado: zero afirmaria que a chamada foi de graça, e
// um orçamento alimentado com zeros é exatamente a ficção que a ADR-0011 §2
// existe para impedir. Quem recebe `false` precisa DIZER que não sabe — é o que o
// ciclo do turno faz, com aviso legível na saída.
func (p ProviderInfo) PriceFor(model string) (Price, bool) {
	pr, ok := p.Prices[model]
	return pr, ok
}

// ── indisponibilidade do fornecedor (D6) ─────────────────────────────────────

// UnavailableReason é por que o provedor não atendeu. Vocabulário FECHADO de
// propósito: o cliente decide o que fazer a partir DAQUI, e um vocabulário aberto
// viraria `strings.Contains` espalhado por três consumidores.
type UnavailableReason string

const (
	// ReasonMissingCredential: não há credencial configurada para este
	// recurso de categoria `agent` (ADR-0013).
	ReasonMissingCredential UnavailableReason = "missing_credential"
	// ReasonRejectedCredential: a credencial existe e foi RECUSADA (401/403).
	ReasonRejectedCredential UnavailableReason = "rejected_credential"
	// ReasonUnreachable: rede — DNS, prazo esgotado, conexão recusada,
	// egress bloqueado.
	ReasonUnreachable UnavailableReason = "unreachable"
	// ReasonProviderError: o fornecedor respondeu erro (5xx, sobrecarga,
	// limite de taxa).
	ReasonProviderError UnavailableReason = "provider_error"
	// ReasonUnknownModel: o modelo roteado não existe no catálogo do
	// fornecedor.
	ReasonUnknownModel UnavailableReason = "unknown_model"
	// ReasonUnknownProvider: não há adaptador para este provedor nesta
	// instalação.
	ReasonUnknownProvider UnavailableReason = "unknown_provider"
)

// orientacao é o que o humano FAZ com cada razão. Escrito uma vez, aqui: três
// consumidores redigindo a mesma orientação é como dois deles a escrevem errado.
var orientacao = map[UnavailableReason]string{
	ReasonMissingCredential: "configure a credencial do recurso de categoria 'agent' " +
		"(ADR-0013) — ela é lida do cofre, no núcleo, e nunca trafega",
	ReasonRejectedCredential: "a credencial do provedor foi recusada; renove-a",
	ReasonUnreachable:        "o provedor de agente não respondeu; verifique rede e allowlist de egress",
	ReasonProviderError:      "o provedor de agente falhou; tente de novo mais tarde",
	ReasonUnknownModel: "o modelo roteado não existe no catálogo deste provedor; " +
		"ajuste o catálogo do adaptador ou a política de roteamento (ADR-0011 §3)",
	ReasonUnknownProvider: "não há adaptador para este provedor de agente nesta instalação",
}

// Unavailable é indisponibilidade do PROVEDOR — distinguível de erro nosso.
//
// Carrega três coisas legíveis por máquina: QUEM (o provedor), POR QUÊ (a razão,
// vocabulário fechado) e o que o humano deve fazer. E `Error()` NUNCA inclui a
// mensagem crua do fornecedor: corpo de erro de API carrega URL, cabeçalho e, em
// alguns casos, um prefixo da chave. O detalhe cru fica em `detail`, que é campo
// NÃO exportado e que só sai por `Detail()` — quem quiser logar precisa pedir.
type Unavailable struct {
	Provider string
	Reason   UnavailableReason
	detail   string
}

// Unavailability monta o erro. `detail` entra por parâmetro e não por campo
// público para que ninguém o embuta numa mensagem por engano ao construir.
func Unavailability(provider string, reason UnavailableReason, detail string) *Unavailable {
	return &Unavailable{Provider: provider, Reason: reason, detail: detail}
}

func (e *Unavailable) Error() string {
	return fmt.Sprintf("provedor de agente %q indisponível (%s): %s",
		e.Provider, e.Reason, orientacao[e.Reason])
}

// Detail é o detalhe CRU do fornecedor, para log. Fora de `Error()` de
// propósito: o que sobe para o cliente é a frase acima, sempre a mesma.
func (e *Unavailable) Detail() string { return e.detail }

// kind traduz a razão para o vocabulário de erro da casa.
//
// A tradução não é decorativa: ela decide o status gRPC e, portanto, quem é
// mandado investigar. Credencial ausente é PRECONDIÇÃO — falta configurar, e é
// exatamente o que `internal/app/gitproviders.go` devolve no caso gêmeo do git.
// Credencial recusada é NÃO AUTENTICADO, como o 401 do adaptador de git. Modelo
// inexistente é NÃO ENCONTRADO. Rede e erro do fornecedor são INDISPONÍVEL, que é
// o que separa "o terceiro caiu" de "temos um defeito".
func (e *Unavailable) kind() errs.Kind {
	switch e.Reason {
	case ReasonMissingCredential:
		return errs.KindPrecondition
	case ReasonRejectedCredential:
		return errs.KindUnauthorized
	case ReasonUnknownModel:
		return errs.KindNotFound
	case ReasonUnknownProvider:
		return errs.KindInvalid
	default:
		return errs.KindUnavailable
	}
}

// Registro do classificador: `errs.KindOf` precisa saber traduzir este erro sem
// que o pacote `errs` conheça o pacote `agent` (o que criaria ciclo). Mesma
// mecânica de `ctxutil`.
func init() {
	errs.RegisterClassifier(func(err error) (errs.Kind, bool) {
		var u *Unavailable
		if errors.As(err, &u) {
			return u.kind(), true
		}
		return "", false
	})
}
