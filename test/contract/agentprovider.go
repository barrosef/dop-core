package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

// ════════════════════════════════════════════════════════════════════════════
// A suíte de contrato da porta agent.AgentProvider.
//
// Disciplina da ADR-0001: uma porta com um adaptador só é palpite. Anthropic e
// OpenAI não têm UMA linha em comum — cache explícito contra automático,
// contagem disjunta contra inclusiva, cinco níveis de effort contra três,
// `role:"system"` contra `role:"developer"` — e é só passando os dois por esta
// mesma régua que "trocar de fornecedor é fiação" deixa de ser promessa.
//
// A API do fornecedor NUNCA é chamada de verdade aqui: o duplo está do outro
// lado do FIO (httptest), e o adaptador sob teste é o REAL, com o seu próprio
// `net/http`, os seus cabeçalhos e a sua decodificação. É a diferença entre
// testar o adaptador e testar um mock do adaptador — a segunda coisa passa
// mesmo quando o adaptador está errado.
//
// Campo de função vazio no Env quer dizer "este ambiente não sabe produzir esse
// caso": o subteste é PULADO com registro, nunca em silêncio.
// ════════════════════════════════════════════════════════════════════════════

// RespostaProgramada é o que o duplo deve responder, descrito no vocabulário do
// DOMÍNIO — cada duplo traduz para o formato do fornecedor dele.
//
// `Uso` é DISJUNTO aqui, sempre. É essa escolha que torna a garantia 6
// verificável de fora: o duplo da OpenAI recebe parcelas disjuntas e as reescreve
// no formato INCLUSIVO daquele fornecedor; se o adaptador não subtrair, a suíte
// vê a entrada inflada e reprova. Um duplo que falasse em vocabulário de
// fornecedor não conseguiria fazer essa pergunta.
type RespostaProgramada struct {
	Texto        string
	Uso          agent.Usage
	ParadaNativa string // motivo de parada no vocabulário do FORNECEDOR
	Status       int    // 0 = 200
	Corpo        string // corpo cru quando Status >= 400

	// Ferramentas é o que o modelo PEDE nesta resposta, descrito no vocabulário
	// do domínio. Cada duplo escreve isso no dialeto dele — objeto dentro de
	// `content` num, string em `tool_calls` no outro (D8) —, e é essa diferença
	// que torna a garantia 17 verificável de fora: um adaptador que não
	// normalize entrega um `Input` nulo onde o outro entrega o mapa.
	Ferramentas []agent.ToolCall
	// ArgumentoIlegivel faz o duplo escrever, no lugar dos argumentos, algo que
	// NÃO decodifica para objeto. É o caso degenerado do D8, e o que se exige
	// dele é que o turno SOBREVIVA: quem conserta o argumento é o modelo, e ele
	// só conserta se receber o erro de volta.
	ArgumentoIlegivel bool
}

// AgentProviderEnv é o que ESTE fornecedor oferece para a suíte trabalhar.
type AgentProviderEnv struct {
	// Conectar monta o adaptador REAL, apontado para o duplo, com a credencial
	// sentinela.
	Conectar func(t *testing.T) agent.AgentProvider
	// SemCredencial monta o adaptador sem chave nenhuma — o caso da razão
	// MISSING_CREDENTIAL, que precisa ser decidido SEM tocar na rede.
	SemCredencial func(t *testing.T) agent.AgentProvider
	// Inalcancavel monta o adaptador apontado para um endereço que não atende.
	Inalcancavel func(t *testing.T) agent.AgentProvider

	// TokenSentinela é a chave EXATA que `Conectar` carrega. A suíte varre toda
	// saída atrás dela (garantia 10). Vazio = varredura pulada com aviso
	// GRITADO: é a garantia cuja falha custa a conta inteira.
	TokenSentinela string

	// Programar diz ao duplo o que responder na PRÓXIMA chamada.
	Programar func(t *testing.T, r RespostaProgramada)
	// UltimoCorpo devolve o corpo que o adaptador REALMENTE enviou. É o que
	// permite conferir que `Send` manda o mesmo que `Render` mostra — sem isso,
	// `Render` poderia ser uma vitrine bonita ao lado de um envio diferente.
	UltimoCorpo func() []byte
	// Chamadas conta as requisições recebidas pelo duplo.
	Chamadas func() int

	// Paradas mapeia motivo nativo do fornecedor → o que o domínio deve
	// enxergar (D5).
	Paradas map[string]agent.StopReason

	// EffortAplicado mapeia cada um dos CINCO níveis do núcleo para o que ESTE
	// fornecedor de fato aplica (D4). Quem tem os cinco mapeia cada um em si
	// mesmo; quem tem três rebaixa — e a suíte exige o aviso.
	EffortAplicado map[agent.Effort]agent.Effort

	// ModeloSemCanalDeOperador é um modelo que o fornecedor RECUSA (400) quando
	// recebe a instrução de operador pelo canal próprio. Vazio = este
	// fornecedor não tem esse caso, e o subteste do recuo é pulado.
	ModeloSemCanalDeOperador string

	// ModeloComPreco é um nome que ESTE adaptador tem na tabela de preço. Vazio
	// = o adaptador não publica preços, e o subteste inverte: `PriceFor` tem de
	// dizer que NÃO SABE, em vez de devolver zero.
	ModeloComPreco string

	// MarcadorDeCache é o trecho que, no corpo DESTE fornecedor, marca o
	// breakpoint do prefixo cacheado — "cache_control" na Anthropic. Vazio
	// quando o cache é automático (OpenAI), e aí a suíte só exige coerência
	// com a capacidade declarada.
	//
	// Este campo nasceu de uma sonda: apagar o breakpoint da Anthropic passava
	// na suíte inteira. O prefixo continuava no lugar certo, a ordem continuava
	// certa, e a conta passaria a chegar ~10× maior sem um único teste vermelho
	// — que é exatamente a falha silenciosa que a ADR-0012 §1 descreve.
	MarcadorDeCache string

	// MarcadorDeFerramenta é o trecho que, no corpo DESTE fornecedor, marca a
	// DECLARAÇÃO de uma ferramenta: "input_schema" na Anthropic, "parameters"
	// na OpenAI (D7). Vazio = este adaptador não implementa ferramentas, e a
	// suíte exige que ele também NÃO anuncie CapToolUse — capacidade é dado que
	// a telemetria lê, e declarar o que não se faz é pior que não declarar.
	MarcadorDeFerramenta string
}

const (
	sentinelaPrefixo  = "SENTINELA-PREFIXO-ESTAVEL-NAO-PODE-SAIR-DO-TOPO"
	sentinelaTurno    = "SENTINELA-TEXTO-VOLATIL-DO-TURNO"
	sentinelaOperador = "SENTINELA-INSTRUCAO-DO-OPERADOR"
	// sentinelaSchema é um campo plantado no schema de saída: é como a suíte
	// pergunta "o schema chegou ao FIO?" sem conhecer o formato de fornecedor
	// nenhum. Sem ele, um adaptador que parasse de enviar o schema passava — a
	// decodificação acontece do nosso lado e continuava funcionando.
	sentinelaSchema = "SENTINELA_CAMPO_DO_SCHEMA"
	// tetoDeSaidaDoTeste é um número improvável de aparecer por acaso no corpo.
	tetoDeSaidaDoTeste = 4097
	// As sentinelas do laço de ferramenta. Cada uma responde a uma pergunta que
	// a suíte não conseguiria fazer conhecendo o formato de um fornecedor só:
	// a declaração chegou? o resultado voltou ligado à chamada? a marca de erro
	// sobreviveu ao fornecedor que não tem campo para ela?
	sentinelaFerramenta         = "sentinela_ferramenta_do_contrato"
	sentinelaSchemaDeFerramenta = "SENTINELA_CAMPO_DO_SCHEMA_DA_FERRAMENTA"
	sentinelaIDDeChamada        = "SENTINELA-ID-DE-CHAMADA-0001"
	sentinelaResultado          = "SENTINELA-CONTEUDO-DO-RESULTADO"
)

// ferramentaDeTeste é a declaração que a suíte manda pelo fio.
func ferramentaDeTeste() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        sentinelaFerramenta,
		Description: "ferramenta de contrato, não existe fora daqui",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				sentinelaSchemaDeFerramenta: map[string]any{"type": "string"},
			},
			"additionalProperties": false,
		},
	}
}

// turnoDeTeste monta um turno com sentinelas em cada fatia, para que a suíte
// possa perguntar "onde isto foi parar?" sem conhecer o formato de fornecedor
// nenhum.
func turnoDeTeste(comOperador bool) agent.Turn {
	msgs := []agent.Message{{Role: agent.RoleUser, Text: sentinelaTurno}}
	if comOperador {
		msgs = append(msgs, agent.Message{Role: agent.RoleOperator, Text: sentinelaOperador})
	}
	esquema := agent.OutputSchema()
	if props, ok := esquema["properties"].(map[string]any); ok {
		props[sentinelaSchema] = map[string]any{"type": "string"}
	}
	return agent.Turn{
		StablePrefix:    "contrato\n" + sentinelaPrefixo + "\ncontexto",
		Messages:        msgs,
		OutputSchema:    esquema,
		MaxOutputTokens: tetoDeSaidaDoTeste,
	}
}

// AgentProviderSuite verifica as dez garantias documentadas na porta.
func AgentProviderSuite(t *testing.T, name string, env func(t *testing.T) AgentProviderEnv) {
	t.Run(name, func(t *testing.T) {
		e := env(t)
		ctx := context.Background()

		// erros coleta TODA mensagem de erro produzida pela suíte, para a
		// varredura final da garantia 10. Vazamento de credencial quase nunca
		// aparece no caminho feliz: ele aparece no 401, que é justamente o
		// caminho onde o fornecedor ecoa o que recebeu.
		var erros []string
		anotar := func(err error) error {
			if err != nil {
				erros = append(erros, err.Error())
				var u *agent.Unavailable
				if errors.As(err, &u) {
					erros = append(erros, u.Detail())
				}
			}
			return err
		}

		t.Run("1_ficha_estavel_e_sem_ida_a_rede", func(t *testing.T) {
			// A ficha é consultada no caminho quente de todo turno (catálogo,
			// preço, capacidades). Se ela custasse uma ida à rede, cada turno
			// pagaria uma chamada a mais — e o adaptador apontado para o vazio
			// nem responderia. Este subteste prova as duas coisas de uma vez.
			p := e.Inalcancavel(t)
			a, b := p.Info(), p.Info()
			if a.Name == "" {
				t.Fatal("ficha sem nome de provedor")
			}
			if a.Name != b.Name || len(a.Catalog) != len(b.Catalog) ||
				len(a.Capabilities) != len(b.Capabilities) {
				t.Fatalf("ficha instável entre chamadas: %+v != %+v", a, b)
			}
		})

		t.Run("2_resolve_model_puro_e_total", func(t *testing.T) {
			info := e.Conectar(t).Info()
			for _, c := range []agent.ModelClass{agent.ClassCheap, agent.ClassMedium, agent.ClassStrong} {
				nome := info.ResolveModel(c)
				if strings.TrimSpace(nome) == "" {
					t.Fatalf("classe %q resolveu para nome vazio — o fornecedor recusaria a "+
						"chamada por um motivo que não é o real", c)
				}
				if nome != info.ResolveModel(c) {
					t.Fatalf("classe %q não é determinística", c)
				}
			}
		})

		t.Run("3_prefixo_estavel_vem_antes_das_mensagens", func(t *testing.T) {
			corpo, _, err := e.Conectar(t).Render(turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			iPrefixo := bytes.Index(corpo, []byte(sentinelaPrefixo))
			iTurno := bytes.Index(corpo, []byte(sentinelaTurno))
			if iPrefixo < 0 {
				t.Fatal("o prefixo estável não foi para a requisição")
			}
			if iTurno < 0 {
				t.Fatal("o texto do turno não foi para a requisição")
			}
			if iPrefixo > iTurno {
				t.Fatalf("PREFIXO DEPOIS DO VOLÁTIL (%d > %d): a economia da ADR-0012 §1 "+
					"quebra SEM BARULHO — não erra, só custa ~10× e aparece na fatura",
					iPrefixo, iTurno)
			}
		})

		t.Run("4_render_deterministico", func(t *testing.T) {
			p := e.Conectar(t)
			turno := turnoDeTeste(true)
			primeiro, _, err := p.Render(turno, "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			// Várias vezes: um mapa iterado em ordem aleatória só denuncia
			// depois de algumas tentativas, e é exatamente esse o defeito que
			// invalida o prefixo cacheado a cada turno.
			for i := 0; i < 20; i++ {
				outro, _, err := p.Render(turno, "modelo-x", agent.EffortHigh)
				if err != nil {
					t.Fatalf("Render: %v", anotar(err))
				}
				if !bytes.Equal(primeiro, outro) {
					t.Fatalf("Render NÃO é determinístico (tentativa %d): bytes diferentes "+
						"para o mesmo Turn invalidam o cache de prefixo a cada turno", i)
				}
			}
			// E o mesmo prefixo com mensagens diferentes tem de dar a MESMA
			// impressão digital: é isso que significa "o prefixo está estável".
			outroTurno := turno
			outroTurno.Messages = []agent.Message{{Role: agent.RoleUser, Text: "outra pergunta"}}
			if turno.Fingerprint() != outroTurno.Fingerprint() {
				t.Fatal("Fingerprint mudou com a conversa: ela é do PREFIXO, e só dele")
			}
		})

		t.Run("5_operador_nunca_como_usuario", func(t *testing.T) {
			corpo, avisos, err := e.Conectar(t).Render(turnoDeTeste(true), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			if !bytes.Contains(corpo, []byte(sentinelaOperador)) {
				t.Fatal("a instrução do operador sumiu da requisição")
			}
			if atribuidaAoUsuario(t, corpo, sentinelaOperador) && len(avisos) == 0 {
				t.Fatal("INSTRUÇÃO DE OPERADOR SERIALIZADA COMO FALA DE USUÁRIO, sem aviso: " +
					"é ela que autoriza, e achatar os dois papéis abre a porta para injeção " +
					"de prompt (D3)")
			}
			// E o texto do usuário continua sendo do usuário — a inversa da
			// garantia, que passaria despercebida sem esta linha.
			if !atribuidaAoUsuario(t, corpo, sentinelaTurno) {
				t.Fatal("o texto do turno não foi atribuído ao usuário")
			}
		})

		t.Run("6_usage_disjunto", func(t *testing.T) {
			if e.Programar == nil {
				t.Skip("ambiente não programa resposta")
			}
			p := e.Conectar(t)
			// Números escolhidos para que a soma inclusiva (1000) e a disjunta
			// (700) sejam inconfundíveis: um adaptador que esqueça de subtrair
			// devolve 1000 e a diferença salta.
			programado := agent.Usage{
				InputTokens: 700, OutputTokens: 55,
				CacheReadTokens: 300, CacheCreationTokens: 120,
			}
			e.Programar(t, RespostaProgramada{Texto: `{"reply":"ok"}`, Uso: programado,
				ParadaNativa: paradaConcluida(e)})

			r, err := p.Send(ctx, turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Send: %v", anotar(err))
			}
			esperado := programado
			if !p.Info().Supports(agent.CapCacheCreationAccounting) {
				// D1: este fornecedor não reporta criação de cache. Zero aqui é
				// "não dá para saber", e a capacidade AUSENTE é o que diz isso.
				esperado.CacheCreationTokens = 0
			}
			if r.Usage != esperado {
				t.Fatalf("DUPLA CONTAGEM (D2): esperava parcelas disjuntas %+v, veio %+v — "+
					"somar campos inclusivos infla a medição da ADR-0011 sem nada falhar",
					esperado, r.Usage)
			}
		})

		t.Run("7_stop_reason_no_vocabulario_do_dominio", func(t *testing.T) {
			if e.Programar == nil || len(e.Paradas) == 0 {
				t.Skip("ambiente não programa motivo de parada")
			}
			p := e.Conectar(t)
			for nativo, esperado := range e.Paradas {
				e.Programar(t, RespostaProgramada{Texto: "oi", ParadaNativa: nativo})
				r, err := p.Send(ctx, turnoDeTeste(false), "modelo-x", agent.EffortHigh)
				if err != nil {
					t.Fatalf("Send (%s): %v", nativo, anotar(err))
				}
				if r.StopReason != esperado {
					t.Fatalf("parada %q virou %q, esperava %q", nativo, r.StopReason, esperado)
				}
			}
			// Motivo NOVO do fornecedor não pode virar erro: parar de trabalhar
			// por causa de uma string desconhecida é pior que registrá-la.
			e.Programar(t, RespostaProgramada{Texto: "oi", ParadaNativa: "motivo_que_ainda_nao_existe"})
			r, err := p.Send(ctx, turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("motivo desconhecido virou ERRO: %v", anotar(err))
			}
			if r.StopReason != agent.StopUnknown {
				t.Fatalf("motivo desconhecido virou %q, esperava %q", r.StopReason, agent.StopUnknown)
			}
		})

		t.Run("8_effort_aplicado_nunca_afirma_demais", func(t *testing.T) {
			if e.Programar == nil || len(e.EffortAplicado) == 0 {
				t.Skip("ambiente não declara o mapeamento de effort")
			}
			p := e.Conectar(t)
			for pedido, esperado := range e.EffortAplicado {
				e.Programar(t, RespostaProgramada{Texto: "oi", ParadaNativa: paradaConcluida(e)})
				r, err := p.Send(ctx, turnoDeTeste(false), "modelo-x", pedido)
				if err != nil {
					t.Fatalf("Send (%s): %v", pedido, anotar(err))
				}
				if r.EffortApplied != esperado {
					t.Fatalf("effort %q: adaptador afirmou %q, o fornecedor aplica %q — "+
						"fingir que aplicou é pior que rebaixar (D4)", pedido, r.EffortApplied, esperado)
				}
				if esperado != pedido && len(r.Warnings) == 0 {
					t.Fatalf("effort %q foi REBAIXADO para %q sem aviso: em trabalho crítico "+
						"(ADR-0007) isso é decisão de produto, e quem roteou precisa saber",
						pedido, esperado)
				}
			}
		})

		t.Run("9_indisponibilidade_com_a_razao_certa", func(t *testing.T) {
			casos := []struct {
				nome     string
				provider func(t *testing.T) agent.AgentProvider
				status   int
				corpo    string
				razao    agent.UnavailableReason
			}{
				{nome: "sem_credencial", provider: e.SemCredencial, razao: agent.ReasonMissingCredential},
				{nome: "rede_fora", provider: e.Inalcancavel, razao: agent.ReasonUnreachable},
				{nome: "credencial_recusada", status: 401, razao: agent.ReasonRejectedCredential},
				{nome: "sem_permissao", status: 403, razao: agent.ReasonRejectedCredential},
				{nome: "modelo_inexistente", status: 404, razao: agent.ReasonUnknownModel},
				{nome: "fornecedor_falhou", status: 500, razao: agent.ReasonProviderError},
				{nome: "limite_de_taxa", status: 429, razao: agent.ReasonProviderError},
			}
			for _, c := range casos {
				t.Run(c.nome, func(t *testing.T) {
					p := e.Conectar(t)
					if c.provider != nil {
						p = c.provider(t)
					} else {
						if e.Programar == nil {
							t.Skip("ambiente não programa status")
						}
						// Corpo com a sentinela DENTRO: é o pior caso real —
						// fornecedor ecoando o que recebeu num corpo de erro.
						e.Programar(t, RespostaProgramada{Status: c.status,
							Corpo: `{"error":"detalhe cru com ` + e.TokenSentinela + ` dentro"}`})
					}
					_, err := p.Send(ctx, turnoDeTeste(false), "modelo-x", agent.EffortHigh)
					anotar(err)
					if err == nil {
						t.Fatal("esperava indisponibilidade, veio sucesso")
					}
					var u *agent.Unavailable
					if !errors.As(err, &u) {
						t.Fatalf("erro CRU vazou pela porta (%T): indisponibilidade de terceiro "+
							"não pode chegar como erro nosso (D6): %v", err, err)
					}
					if u.Reason != c.razao {
						t.Fatalf("razão %q, esperava %q", u.Reason, c.razao)
					}
				})
			}
		})

		t.Run("9b_sem_credencial_nao_toca_a_rede", func(t *testing.T) {
			if e.Chamadas == nil {
				t.Skip("ambiente não conta chamadas")
			}
			antes := e.Chamadas()
			_, err := e.SemCredencial(t).Send(ctx, turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			anotar(err)
			if depois := e.Chamadas(); depois != antes {
				t.Fatalf("credencial ausente custou %d ida(s) à rede: quem chama sem chave "+
					"não pode gastar uma chamada para descobrir isso", depois-antes)
			}
		})

		t.Run("10_credencial_nunca_vaza", func(t *testing.T) {
			if e.TokenSentinela == "" {
				t.Log("### AVISO: sem TokenSentinela, a garantia mais cara da porta NÃO foi " +
					"verificada — chave de agente em log é chave em repouso")
				t.Skip("sem sentinela")
			}
			p := e.Conectar(t)
			corpo, _, err := p.Render(turnoDeTeste(true), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			if bytes.Contains(corpo, []byte(e.TokenSentinela)) {
				t.Fatal("A CREDENCIAL FOI PARAR NO CORPO DA REQUISIÇÃO: ela vai em cabeçalho, " +
					"e `Render` precisa ser superfície segura para log")
			}
			// %v e %+v: o fmt lê campos NÃO exportados por reflexão e não
			// consegue chamar o String() deles. É por isso que a chave mora num
			// closure, e é isto que confere que ela continua lá.
			for _, formatado := range []string{fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p)} {
				if strings.Contains(formatado, e.TokenSentinela) {
					t.Fatalf("A CREDENCIAL APARECE NA FORMATAÇÃO DO ADAPTADOR: %s", formatado)
				}
			}
			for _, msg := range erros {
				if strings.Contains(msg, e.TokenSentinela) {
					t.Fatalf("A CREDENCIAL APARECE NUMA MENSAGEM DE ERRO: %s", msg)
				}
			}
			if len(erros) == 0 {
				t.Fatal("nenhuma mensagem de erro foi coletada: a varredura passaria vazia, " +
					"que é pior que não varrer — ela afirmaria uma garantia que não testou")
			}
		})

		t.Run("11_preco_desconhecido_nao_vira_zero", func(t *testing.T) {
			info := e.Conectar(t).Info()
			if _, ok := info.PriceFor("modelo-que-nao-existe-em-catalogo-nenhum"); ok {
				t.Fatal("PriceFor inventou preço para modelo desconhecido")
			}
			if e.ModeloComPreco == "" {
				// Adaptador sem tabela: a única resposta honesta é "não sei"
				// para TUDO, inclusive para o próprio catálogo. Zero afirmaria
				// que a chamada foi de graça (ADR-0011 §2).
				for _, c := range []agent.ModelClass{agent.ClassCheap, agent.ClassMedium, agent.ClassStrong} {
					if _, ok := info.PriceFor(info.ResolveModel(c)); ok {
						t.Fatalf("ambiente diz não ter tabela de preço, mas %q tem preço", c)
					}
				}
				return
			}
			preco, ok := info.PriceFor(e.ModeloComPreco)
			if !ok {
				t.Fatalf("modelo %q deveria ter preço", e.ModeloComPreco)
			}
			if preco.Currency == "" {
				t.Fatal("preço sem moeda: micros sem unidade é número que soma dólar com real")
			}
			// Aritmética INTEIRA do começo ao fim, e por 1.000 tokens.
			custo := preco.CostMicros(agent.Usage{InputTokens: 1000})
			if custo != preco.InputPer1k {
				t.Fatalf("1.000 tokens de entrada custaram %d, esperava %d", custo, preco.InputPer1k)
			}
		})

		t.Run("12_saida_estruturada_chega_decodificada", func(t *testing.T) {
			if e.Programar == nil {
				t.Skip("ambiente não programa resposta")
			}
			p := e.Conectar(t)
			if !p.Info().Supports(agent.CapStructuredOutput) {
				t.Skip("provedor não anuncia saída estruturada")
			}
			e.Programar(t, RespostaProgramada{
				Texto:        `{"reply":"resposta ao humano","concluded":true,"finding_title":"t","finding_summary":"s","finding_evidence":["e1"]}`,
				ParadaNativa: paradaConcluida(e),
			})
			r, err := p.Send(ctx, turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Send: %v", anotar(err))
			}
			if r.Data == nil {
				t.Fatal("saída estruturada não foi decodificada: o domínio teria de " +
					"re-parsear texto, que é o que a ADR-0012 §2 evita")
			}
			if r.Data["reply"] != "resposta ao humano" {
				t.Fatalf("campo `reply` veio %v", r.Data["reply"])
			}
			// Texto solto NÃO pode virar erro: o schema pode falhar e a fala
			// crua ainda vale para o humano que lê a thread.
			e.Programar(t, RespostaProgramada{Texto: "texto solto, sem JSON", ParadaNativa: paradaConcluida(e)})
			r2, err := p.Send(ctx, turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("texto solto virou erro: %v", anotar(err))
			}
			if r2.Text != "texto solto, sem JSON" {
				t.Fatalf("texto cru se perdeu: %q", r2.Text)
			}
		})

		t.Run("13_send_manda_o_que_render_mostra", func(t *testing.T) {
			if e.Programar == nil || e.UltimoCorpo == nil {
				t.Skip("ambiente não expõe o corpo enviado")
			}
			p := e.Conectar(t)
			turno := turnoDeTeste(true)
			esperado, _, err := p.Render(turno, "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			e.Programar(t, RespostaProgramada{Texto: "oi", ParadaNativa: paradaConcluida(e)})
			if _, err := p.Send(ctx, turno, "modelo-x", agent.EffortHigh); err != nil {
				t.Fatalf("Send: %v", anotar(err))
			}
			if !bytes.Equal(esperado, e.UltimoCorpo()) {
				t.Fatalf("Send enviou algo DIFERENTE do que Render mostra — a auditoria da "+
					"garantia 3 estaria olhando para uma vitrine:\nrender: %s\nenviado: %s",
					esperado, e.UltimoCorpo())
			}
		})

		t.Run("15_breakpoint_de_cache_marca_o_fim_do_prefixo", func(t *testing.T) {
			p := e.Conectar(t)
			explicito := p.Info().Supports(agent.CapExplicitPrefixCache)
			if explicito == (e.MarcadorDeCache == "") {
				t.Fatalf("incoerência entre a capacidade declarada (%v) e o marcador do "+
					"ambiente (%q): capacidade é DADO que a telemetria lê, e declarar o que "+
					"não se faz é pior do que não declarar", explicito, e.MarcadorDeCache)
			}
			if !explicito {
				t.Skip("cache automático neste fornecedor: não há breakpoint para marcar (D1)")
			}
			corpo, _, err := p.Render(turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			iMarca := bytes.Index(corpo, []byte(e.MarcadorDeCache))
			if iMarca < 0 {
				t.Fatal("O BREAKPOINT DE CACHE SUMIU: o adaptador anuncia cache explícito e " +
					"não marca o prefixo. Nada falha — o prefixo inteiro passa a ser cobrado " +
					"como entrada nova a cada turno (~10×), e só a fatura conta (ADR-0012 §1)")
			}
			if iTurno := bytes.Index(corpo, []byte(sentinelaTurno)); iMarca > iTurno {
				t.Fatalf("o breakpoint ficou DEPOIS do texto volátil (%d > %d): marcar no fim "+
					"do prompt escreve uma entrada de cache nova a cada turno e não lê nenhuma",
					iMarca, iTurno)
			}
		})

		t.Run("16_schema_de_saida_vai_para_o_fio", func(t *testing.T) {
			p := e.Conectar(t)
			if !p.Info().Supports(agent.CapStructuredOutput) {
				t.Skip("provedor não anuncia saída estruturada")
			}
			corpo, _, err := p.Render(turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			if !bytes.Contains(corpo, []byte(sentinelaSchema)) {
				t.Fatal("O SCHEMA DE SAÍDA NÃO FOI PARA A REQUISIÇÃO: o adaptador anuncia " +
					"saída estruturada e não a pede. A decodificação do nosso lado continua " +
					"funcionando enquanto o modelo colaborar, e a validação do fornecedor " +
					"(ADR-0012 §2) vira sorte — sem um teste vermelho")
			}
		})

		t.Run("17_teto_de_saida_vai_para_o_fio", func(t *testing.T) {
			corpo, _, err := e.Conectar(t).Render(turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			if !bytes.Contains(corpo, []byte(fmt.Sprint(tetoDeSaidaDoTeste))) {
				t.Fatal("O TETO DE SAÍDA DO TURNO NÃO CHEGOU AO FORNECEDOR: o adaptador está " +
					"usando um limite que ninguém pediu. Resposta cortada passaria a sair como " +
					"StopReason=max_tokens sem que nada no sistema explique por quê — e o teto " +
					"é uma alavanca de custo (ADR-0011)")
			}
		})

		// ── ferramentas: as garantias 15 a 21 (D7–D11) ──────────────────────
		//
		// Prefixo `garantia` no nome porque os subtestes acima foram numerados
		// pela ORDEM em que nasceram, e não pela garantia que provam (o "15_"
		// de cima é a garantia 11). Reaproveitar os números aqui daria dois
		// subtestes "15" provando coisas diferentes, e quem lê a saída de `-v`
		// não teria como saber qual é qual.

		t.Run("garantia15_declaracao_de_ferramenta_vai_antes_do_volatil", func(t *testing.T) {
			p := e.Conectar(t)
			temFerramentas := p.Info().Supports(agent.CapToolUse)
			if temFerramentas == (e.MarcadorDeFerramenta == "") {
				t.Fatalf("incoerência entre a capacidade declarada (%v) e o marcador do "+
					"ambiente (%q): capacidade é DADO que a telemetria lê, e declarar o que "+
					"não se faz é pior do que não declarar", temFerramentas, e.MarcadorDeFerramenta)
			}
			if !temFerramentas {
				t.Skip("este adaptador não implementa ferramentas")
			}

			turno := turnoDeTeste(false)
			turno.Tools = []agent.ToolSpec{ferramentaDeTeste()}
			corpo, _, err := p.Render(turno, "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			if !bytes.Contains(corpo, []byte(e.MarcadorDeFerramenta)) {
				t.Fatalf("a DECLARAÇÃO de ferramenta não foi para a requisição (esperava %q "+
					"no corpo): o adaptador anuncia ferramentas e não as declara, e o modelo "+
					"nunca vai pedir o que não sabe que existe", e.MarcadorDeFerramenta)
			}
			if !bytes.Contains(corpo, []byte(sentinelaSchemaDeFerramenta)) {
				t.Fatal("o SCHEMA da ferramenta não foi para a requisição: sem ele o " +
					"fornecedor não valida nada e todo argumento vira sorte")
			}
			iFerr := bytes.Index(corpo, []byte(sentinelaFerramenta))
			iTurno := bytes.Index(corpo, []byte(sentinelaTurno))
			if iFerr > iTurno {
				t.Fatalf("a declaração ficou DEPOIS do texto volátil (%d > %d): declaração é "+
					"estável por thread e sai do trecho cacheável quando vai para o fim — não "+
					"erra, só custa (ADR-0012 §1)", iFerr, iTurno)
			}
		})

		t.Run("garantia16_turno_sem_ferramenta_nao_manda_o_campo", func(t *testing.T) {
			p := e.Conectar(t)
			if !p.Info().Supports(agent.CapToolUse) || e.MarcadorDeFerramenta == "" {
				t.Skip("este adaptador não implementa ferramentas")
			}
			corpo, _, err := p.Render(turnoDeTeste(false), "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			// Array vazio não é a mesma coisa que campo ausente: ele ocupa
			// lugar no prompt e convida o modelo a chamar o que não existe.
			if bytes.Contains(corpo, []byte(`"tools"`)) {
				t.Fatalf("turno SEM ferramentas mandou o campo `tools` assim mesmo:\n%s", corpo)
			}
		})

		t.Run("garantia17e19_chamada_normalizada_e_parada_tool_use", func(t *testing.T) {
			p := e.Conectar(t)
			if e.Programar == nil || !p.Info().Supports(agent.CapToolUse) {
				t.Skip("ambiente não programa resposta ou adaptador sem ferramentas")
			}
			pedidas := []agent.ToolCall{
				{ID: sentinelaIDDeChamada, Name: sentinelaFerramenta,
					Input: map[string]any{"alvo": "primeira"}},
				{ID: sentinelaIDDeChamada + "-b", Name: sentinelaFerramenta,
					Input: map[string]any{"alvo": "segunda"}},
			}
			e.Programar(t, RespostaProgramada{
				Texto: "vou olhar", Ferramentas: pedidas, ParadaNativa: paradaDeFerramenta(e),
			})

			turno := turnoDeTeste(false)
			turno.Tools = []agent.ToolSpec{ferramentaDeTeste()}
			r, err := p.Send(ctx, turno, "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Send: %v", anotar(err))
			}
			if r.StopReason != agent.StopToolUse {
				t.Fatalf("o fornecedor pediu ferramenta e a parada veio %q, esperava %q (D5)",
					r.StopReason, agent.StopToolUse)
			}
			if len(r.ToolCalls) != 2 {
				t.Fatalf("esperava 2 chamadas, vieram %d: %+v", len(r.ToolCalls), r.ToolCalls)
			}
			// A ORDEM é a que o fornecedor emitiu (D10). Reordenar transforma
			// "rodei o teste e depois li o log" em "li o log e depois rodei o
			// teste" na leitura do modelo.
			for i, quero := range pedidas {
				veio := r.ToolCalls[i]
				if veio.ID != quero.ID || veio.Name != quero.Name {
					t.Fatalf("chamada %d veio %+v, esperava id=%q nome=%q",
						i, veio, quero.ID, quero.Name)
				}
				if veio.Input == nil {
					t.Fatalf("chamada %d chegou com Input NULO: um dos fornecedores manda os "+
						"argumentos como STRING (D8), e não decodificá-los deixa o laço sem "+
						"o que executar", i)
				}
				if veio.Input["alvo"] != quero.Input["alvo"] {
					t.Fatalf("chamada %d perdeu o argumento: %+v", i, veio.Input)
				}
			}
		})

		t.Run("garantia18_argumento_ilegivel_nao_derruba_o_turno", func(t *testing.T) {
			p := e.Conectar(t)
			if e.Programar == nil || !p.Info().Supports(agent.CapToolUse) {
				t.Skip("ambiente não programa resposta ou adaptador sem ferramentas")
			}
			e.Programar(t, RespostaProgramada{
				Texto: "vou olhar",
				Ferramentas: []agent.ToolCall{
					{ID: sentinelaIDDeChamada, Name: sentinelaFerramenta},
				},
				ArgumentoIlegivel: true,
				ParadaNativa:      paradaDeFerramenta(e),
			})
			turno := turnoDeTeste(false)
			turno.Tools = []agent.ToolSpec{ferramentaDeTeste()}

			r, err := p.Send(ctx, turno, "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("ARGUMENTO ILEGÍVEL DERRUBOU O TURNO: quem conserta o argumento é o "+
					"MODELO, e ele só conserta se receber o erro de volta (D8): %v", anotar(err))
			}
			if len(r.ToolCalls) != 1 {
				t.Fatalf("a chamada com argumento ilegível SUMIU: %+v", r.ToolCalls)
			}
			c := r.ToolCalls[0]
			if c.Input != nil {
				t.Fatalf("argumento ilegível virou objeto: %+v — nulo é o que diz 'não deu "+
					"para ler', e mapa vazio afirmaria 'sem argumentos'", c.Input)
			}
			if strings.TrimSpace(c.RawInput) == "" {
				t.Fatal("o argumento cru se perdeu: 'seu argumento é inválido' sem dizer QUAL " +
					"argumento é uma mensagem que não conserta nada")
			}
			if len(r.Warnings) == 0 {
				t.Fatal("argumento ilegível sem aviso: quem lê a telemetria não tem como " +
					"distinguir isto de um modelo que simplesmente não chamou ferramenta")
			}
			// E o id precisa sobreviver: sem ele, o resultado de erro do laço
			// fica órfão e os DOIS fornecedores recusam o turno seguinte (D11).
			if c.ID != sentinelaIDDeChamada {
				t.Fatalf("o id da chamada se perdeu (%q): resultado sem par é 400 nos dois", c.ID)
			}
		})

		t.Run("garantia20e21_resultado_ligado_a_chamada_e_marca_de_erro", func(t *testing.T) {
			p := e.Conectar(t)
			if !p.Info().Supports(agent.CapToolUse) {
				t.Skip("este adaptador não implementa ferramentas")
			}
			turno := turnoDeTeste(false)
			turno.Tools = []agent.ToolSpec{ferramentaDeTeste()}
			// A história de uma segunda volta: a fala do modelo COM a chamada,
			// e o resultado logo depois. As duas são obrigatórias e nesta
			// ordem — os dois fornecedores recusam resultado sem chamada (D11).
			turno.Messages = append(turno.Messages,
				agent.Message{Role: agent.RoleAssistant, Text: "vou olhar",
					ToolCalls: []agent.ToolCall{{
						ID: sentinelaIDDeChamada, Name: sentinelaFerramenta,
						Input: map[string]any{"alvo": "x"},
					}}},
				agent.Message{Role: agent.RoleToolResult,
					ToolResults: []agent.ToolResult{{
						CallID: sentinelaIDDeChamada, Name: sentinelaFerramenta,
						Content: sentinelaResultado, IsError: true,
					}}},
			)

			corpo, _, err := p.Render(turno, "modelo-x", agent.EffortHigh)
			if err != nil {
				t.Fatalf("Render: %v", anotar(err))
			}
			if !bytes.Contains(corpo, []byte(sentinelaResultado)) {
				t.Fatal("o resultado da ferramenta não foi para a requisição")
			}
			// O id precisa aparecer DUAS vezes: uma na chamada reenviada (do
			// lado do assistente) e outra ligando o resultado a ela. Contar as
			// ocorrências, e não só procurar o id, é o que separa "o par
			// existe" de "o id está em algum lugar do corpo" — e a diferença
			// não é acadêmica: a primeira versão deste subteste procurava só a
			// presença, e apagar o reenvio das chamadas PASSAVA nela. Um
			// resultado órfão é 400 nos dois fornecedores (D11), e é a suíte
			// que tem de pegar isso, não a API em produção.
			if n := bytes.Count(corpo, []byte(sentinelaIDDeChamada)); n < 2 {
				t.Fatalf("o id da chamada aparece %d vez(es) no corpo, esperava ao menos 2 "+
					"(a chamada reenviada e o resultado que a referencia). Resultado cujo id "+
					"não tem par é 400 nos dois fornecedores (D11):\n%s", n, corpo)
			}
			// E a FALA do assistente também é reenviada: sem ela o modelo lê o
			// resultado sem lembrar por que o pediu.
			if !bytes.Contains(corpo, []byte("vou olhar")) {
				t.Fatal("a fala do assistente com a chamada não foi reenviada: o resultado " +
					"seguinte fica órfão (D11)")
			}
			// A marca de erro precisa CHEGAR ao modelo de algum jeito: um
			// fornecedor tem o booleano nativo, o outro não tem campo nenhum e
			// escreve no texto (D9). A suíte não sabe qual é qual — ela exige
			// que a informação exista em ALGUMA forma.
			if !bytes.Contains(corpo, []byte("is_error")) &&
				!bytes.Contains(bytes.ToUpper(corpo), []byte("ERRO")) {
				t.Fatalf("a MARCA DE ERRO do resultado se perdeu: o modelo vai ler uma falha "+
					"como saída normal e seguir afirmando o contrário do que aconteceu "+
					"(garantia 21):\n%s", corpo)
			}
			// E o conteúdo do resultado NÃO pode ser atribuído ao usuário: é
			// conteúdo não confiável (spec do substrato §6), e um `README`
			// malicioso lido por `cat` não pode chegar com autoridade de quem
			// pediu o trabalho.
			if papelDoTexto(t, corpo, sentinelaResultado) == "user" &&
				!bytes.Contains(corpo, []byte("tool_result")) {
				t.Fatal("o resultado da ferramenta foi serializado como FALA DO USUÁRIO solta: " +
					"saída de comando é conteúdo não confiável e não pode virar instrução")
			}
		})

		t.Run("14_recuo_do_canal_de_operador", func(t *testing.T) {
			if e.ModeloSemCanalDeOperador == "" || e.Programar == nil {
				t.Skip("este fornecedor não tem modelo sem canal de operador")
			}
			p := e.Conectar(t)
			e.Programar(t, RespostaProgramada{Texto: "oi", ParadaNativa: paradaConcluida(e)})
			r, err := p.Send(ctx, turnoDeTeste(true), e.ModeloSemCanalDeOperador, agent.EffortHigh)
			if err != nil {
				t.Fatalf("o recuo não aconteceu: %v", anotar(err))
			}
			if len(r.Warnings) == 0 {
				t.Fatal("RECUO SEM AVISO: a instrução do operador foi entregue dentro do turno " +
					"do usuário e ninguém ficou sabendo (D3)")
			}
			corpo := e.UltimoCorpo()
			if !bytes.Contains(corpo, []byte(sentinelaOperador)) {
				t.Fatal("a instrução do operador sumiu no recuo")
			}
			// Mesmo no recuo, ela precisa estar MARCADA — entregue como texto
			// solto do usuário, ela viraria dado indistinguível de injeção.
			if !bytes.Contains(corpo, []byte("intervencao-do-operador")) {
				t.Fatal("no recuo, a instrução entrou no turno do usuário SEM MARCAÇÃO")
			}
		})
	})
}

// paradaConcluida devolve um motivo de parada nativo que signifique "terminou",
// para os subtestes que não estão medindo parada.
func paradaConcluida(e AgentProviderEnv) string {
	for nativo, dominio := range e.Paradas {
		if dominio == agent.StopCompleted {
			return nativo
		}
	}
	return ""
}

// paradaDeFerramenta devolve o motivo NATIVO que significa "pedi ferramenta".
func paradaDeFerramenta(e AgentProviderEnv) string {
	for nativo, dominio := range e.Paradas {
		if dominio == agent.StopToolUse {
			return nativo
		}
	}
	return ""
}

// papelDoTexto devolve o papel do objeto que contém `alvo`, ou "" se não achar.
//
// Genérica pelo mesmo motivo de `atribuidaAoUsuario`: a suíte não pode conhecer
// o formato de fornecedor nenhum. O que ela sabe é que os dois marcam quem fala
// num campo `role`.
func papelDoTexto(t *testing.T, corpo []byte, alvo string) string {
	t.Helper()
	var raiz any
	if err := json.Unmarshal(corpo, &raiz); err != nil {
		t.Fatalf("corpo ilegível: %v", err)
	}
	for _, papel := range []string{"user", "assistant", "tool", "developer", "system"} {
		if procurarSobPapel(raiz, papel, alvo) {
			return papel
		}
	}
	return ""
}

// atribuidaAoUsuario procura `alvo` DENTRO de algum objeto com `role: "user"`.
//
// Genérico de propósito: a suíte não pode conhecer o formato de fornecedor
// nenhum, senão ela vira dois testes com um nome só. O que ela sabe é que os dois
// formatos marcam o papel de quem fala num campo `role`, e isso basta para
// perguntar "esta frase foi atribuída ao usuário?".
func atribuidaAoUsuario(t *testing.T, corpo []byte, alvo string) bool {
	t.Helper()
	var raiz any
	if err := json.Unmarshal(corpo, &raiz); err != nil {
		t.Fatalf("corpo ilegível: %v", err)
	}
	return procurarSobPapel(raiz, "user", alvo)
}

func procurarSobPapel(v any, papel, alvo string) bool {
	switch n := v.(type) {
	case map[string]any:
		if p, ok := n["role"].(string); ok && p == papel && contemTexto(n, alvo) {
			return true
		}
		for _, e := range n {
			if procurarSobPapel(e, papel, alvo) {
				return true
			}
		}
	case []any:
		for _, e := range n {
			if procurarSobPapel(e, papel, alvo) {
				return true
			}
		}
	}
	return false
}

func contemTexto(v any, alvo string) bool {
	switch n := v.(type) {
	case string:
		return strings.Contains(n, alvo)
	case map[string]any:
		for _, e := range n {
			if contemTexto(e, alvo) {
				return true
			}
		}
	case []any:
		for _, e := range n {
			if contemTexto(e, alvo) {
				return true
			}
		}
	}
	return false
}
