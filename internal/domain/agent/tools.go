package agent

import (
	"fmt"
	"sort"
	"strings"
)

// ════════════════════════════════════════════════════════════════════════════
// O CATÁLOGO DE FERRAMENTAS — o que o agente pode FAZER, e não só dizer.
//
// O catálogo mora no DOMÍNIO, e não no substrato, por duas razões que puxam para
// o mesmo lado:
//
//  1. a declaração de ferramenta É PROMPT. Ela entra no corpo antes da conversa
//     (garantia 15) e faz parte do que o fornecedor cacheia. Se o texto viesse
//     do domínio de execução, uma mudança de descrição lá invalidaria o prefixo
//     de todas as threads de todas as contas aqui — o invalidador silencioso da
//     ADR-0012 §1, agora com o gatilho num pacote que nem sabe que ele existe;
//
//  2. o contrato com o modelo é do runtime. Nome, descrição e schema são o que
//     o agente lê para decidir o que fazer; o substrato só sabe rodar comando.
//
// ── POR QUE UMA FERRAMENTA SÓ ───────────────────────────────────────────────
//
// A tentação é publicar um cardápio: `read_file`, `write_file`, `list_dir`,
// `run_tests`, `git_status`. Todas elas são um comando com outro nome, e cada
// uma custa três coisas: tokens permanentes no prefixo de TODO turno, uma
// superfície a mais para o modelo errar de escolha, e uma tradução a mais para
// manter. `run_command` compõe todas: `cat`, `ls`, `git status`, o runner de
// teste do projeto.
//
// E há um ganho que não é de economia. Com uma ferramenta só, a auditoria da
// demanda mostra o COMANDO que rodou — `sh -c 'npm test'` — em vez de
// `run_tests{}`, que esconde o que foi executado atrás de um nome nosso. Numa
// plataforma cuja premissa é distinguir o que o agente fez do que o humano fez
// (ADR-0006), esconder o comando seria trabalhar contra a premissa.
//
// ── SEM SHELL IMPLÍCITO ─────────────────────────────────────────────────────
//
// `command` é argv. Quem quiser pipeline passa `["sh","-c","… | …"]`, e o shell
// aparece na auditoria. Embrulhar tudo em `sh -c` por dentro faria o contrário:
// todo comando pareceria seguro na leitura e nenhum seria.
// ════════════════════════════════════════════════════════════════════════════

// ToolRunCommand é o nome da ferramenta, tal como ele aparece na ficha da thread
// (`AgentCard.Tools`) e na declaração enviada ao fornecedor.
const ToolRunCommand = "run_command"

// Limites de UM comando, no vocabulário do runtime.
//
// Eles são do DOMÍNIO e não do adaptador porque são política de custo, não
// detalhe de substrato: a saída de uma ferramenta vira contexto do próximo
// turno, contexto vira token e token vira fatura (ADR-0011). O teto do modelo
// pode até ser configurável um dia; o que não pode é não existir.
const (
	// DefaultToolTimeoutSeconds é o prazo de um comando de ferramenta. Menor
	// que o default da porta (2 min) de propósito: aqui o chamador é um laço
	// que já reenvia a conversa a cada volta, e um comando pendurado por dois
	// minutos por volta multiplica a espera pelo teto de voltas.
	DefaultToolTimeoutSeconds = 90
	// MaxToolTimeoutSeconds é o teto que o MODELO pode pedir. Ele escolhe
	// dentro da faixa; passar disso é rebaixado em silêncio para o teto —
	// silêncio aceitável porque não muda a semântica, só o tempo de espera.
	MaxToolTimeoutSeconds = 300
	// DefaultToolOutputBytes é quanto da saída volta para o modelo. 32 KiB é
	// da ordem de 8 mil tokens: um log longo cabe, um `cat` de binário não
	// derruba o turno.
	DefaultToolOutputBytes = 32 << 10
)

// runCommandSpec é a declaração da ferramenta.
//
// É FUNÇÃO e não variável de pacote pela mesma razão de `OutputSchema()`:
// devolve mapas, e uma variável seria compartilhada — o primeiro adaptador que
// mexesse nela por engano mudaria o contrato de todos os turnos de todas as
// contas.
func runCommandSpec() ToolSpec {
	return ToolSpec{
		Name: ToolRunCommand,
		Description: "Executa um comando dentro do sandbox desta demanda e devolve a saída. " +
			"O diretório de trabalho é " + SandboxWorkspaceHint + ". " +
			"NÃO há shell implícito: para pipeline ou redirecionamento, use " +
			`["sh","-c","..."]` + ". " +
			"Código de saída diferente de zero é resultado normal — leia a saída e corrija.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "O comando como lista de argumentos (argv). Ex.: [\"git\",\"status\"].",
				},
				"timeout_seconds": map[string]any{
					"type": "integer",
					"description": fmt.Sprintf(
						"Prazo do comando em segundos. Padrão %d, máximo %d.",
						DefaultToolTimeoutSeconds, MaxToolTimeoutSeconds),
				},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	}
}

// SandboxWorkspaceHint é o caminho do workspace COMO O AGENTE O LÊ.
//
// Ele repete o valor de `ports.SandboxWorkspacePath`, e a repetição é
// deliberada: este domínio não importa `ports` (é o pacote de infraestrutura), e
// importá-lo só para uma string colocaria a porta de infra dentro do runtime.
// A cola em internal/app é quem garante que os dois valores concordam — e é o
// mesmo preço já pago por `Micros` não importar `cost`.
const SandboxWorkspaceHint = "/workspace"

// catalogo é o cardápio completo, por nome. Mapa aqui é seguro porque nada dele
// sai iterado: `ToolCatalog` monta a lista ORDENADA.
func catalogo() map[string]ToolSpec {
	return map[string]ToolSpec{ToolRunCommand: runCommandSpec()}
}

// ToolCatalog devolve as ferramentas CONCEDIDAS a esta thread, ordenadas por
// nome, mais os nomes concedidos que não existem no catálogo.
//
// Duas decisões, e as duas viram comportamento visível:
//
//   - a ORDEM é alfabética e não a da ficha. A ficha é dado de conta, escrito
//     por gente, e a ordem em que alguém digitou duas ferramentas não é escolha
//     de ninguém — mas mudaria os bytes do prefixo e a entrada de cache junto
//     (ADR-0012 §1, camada 3 de prompt.go);
//
//   - nome CONCEDIDO E INEXISTENTE não é silêncio nem erro: volta na segunda
//     lista, e o ciclo do turno o transforma em aviso legível. Silenciar faria
//     o agente parecer capaz de algo que ninguém instalou; falhar o turno faria
//     uma linha errada na ficha de uma thread parar o trabalho dela inteiro.
func ToolCatalog(granted []string) (specs []ToolSpec, desconhecidas []string) {
	if len(granted) == 0 {
		return nil, nil
	}
	disponivel := catalogo()
	vistos := make(map[string]bool, len(granted))
	for _, nome := range granted {
		nome = strings.TrimSpace(nome)
		if nome == "" || vistos[nome] {
			continue
		}
		vistos[nome] = true
		if spec, ok := disponivel[nome]; ok {
			specs = append(specs, spec)
			continue
		}
		desconhecidas = append(desconhecidas, nome)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	sort.Strings(desconhecidas)
	return specs, desconhecidas
}

// ── da chamada do modelo ao comando do substrato ────────────────────────────

// comandoDe traduz uma chamada de ferramenta em comando do substrato.
//
// Todo caminho de recusa aqui devolve MOTIVO, não erro: quem recebe o motivo é o
// laço, que o entrega ao modelo como resultado de erro. Argumento errado é coisa
// que o modelo conserta sozinho na volta seguinte — desde que ele saiba o que
// estava errado.
func comandoDe(call ToolCall, permitidas []ToolSpec) (SandboxCommand, string) {
	if !ferramentaPermitida(call.Name, permitidas) {
		// D11: o modelo chamou o que não foi declarado. Nenhum dos dois
		// fornecedores impede isso, então quem impede é o laço — e a lista de
		// nomes válidos vai junto, porque uma recusa sem alternativa faz o
		// modelo tentar de novo o mesmo nome.
		return SandboxCommand{}, fmt.Sprintf(
			"ferramenta %q não foi concedida a esta thread. Disponíveis: %s.",
			call.Name, nomesDe(permitidas))
	}
	if call.Input == nil {
		// D8: o fornecedor mandou argumento que não decodifica. O texto cru vai
		// junto — "seu argumento é inválido" sem dizer qual argumento é uma
		// mensagem que não conserta nada.
		return SandboxCommand{}, fmt.Sprintf(
			"os argumentos não são um objeto JSON válido. Recebido: %s",
			resumir(call.RawInput, 400))
	}

	argv, ok := listaDeStrings(call.Input["command"])
	if !ok || len(argv) == 0 {
		return SandboxCommand{}, `o campo "command" é obrigatório e precisa ser uma lista ` +
			`de strings não vazia. Ex.: {"command":["git","status"]}`
	}

	prazo := DefaultToolTimeoutSeconds
	if n, ok := inteiroDe(call.Input["timeout_seconds"]); ok && n > 0 {
		prazo = n
		if prazo > MaxToolTimeoutSeconds {
			prazo = MaxToolTimeoutSeconds
		}
	}
	return SandboxCommand{
		Command:        argv,
		TimeoutSeconds: prazo,
		MaxOutputBytes: DefaultToolOutputBytes,
	}, ""
}

func ferramentaPermitida(nome string, permitidas []ToolSpec) bool {
	for _, s := range permitidas {
		if s.Name == nome {
			return true
		}
	}
	return false
}

func nomesDe(specs []ToolSpec) string {
	if len(specs) == 0 {
		return "nenhuma"
	}
	nomes := make([]string, 0, len(specs))
	for _, s := range specs {
		nomes = append(nomes, s.Name)
	}
	return strings.Join(nomes, ", ")
}

// listaDeStrings aceita a lista como a decodificação JSON a entrega (`[]any`).
// Item que não é string DERRUBA a conversão inteira, e aqui — diferente das
// evidências do achado — descartar seria pior: um argv com um argumento a menos
// não é um comando pior, é OUTRO comando.
func listaDeStrings(v any) ([]string, bool) {
	bruto, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(bruto))
	for _, item := range bruto {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// inteiroDe lê um número da decodificação JSON, que sempre chega como float64.
func inteiroDe(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// ── do resultado do substrato ao resultado do modelo ────────────────────────

// resultadoDe monta o que o MODELO vai ler.
//
// O formato é fixo e legível de propósito: o modelo precisa distinguir, sem
// adivinhar, três coisas que se parecem numa saída solta — o comando terminou
// bem, o comando falhou, e o comando nem terminou. E `Truncated` aparece SEMPRE
// que houve corte: um agente que conclui a partir de saída cortada sem saber que
// ela foi cortada conclui errado, e ninguém consegue explicar depois por quê.
func resultadoDe(call ToolCall, out SandboxOutput) ToolResult {
	var b strings.Builder
	switch {
	case out.TimedOut:
		fmt.Fprintf(&b, "O comando NÃO terminou dentro do prazo e foi abandonado.\n")
	default:
		fmt.Fprintf(&b, "exit_code: %d\n", out.ExitCode)
	}
	if out.Truncated {
		b.WriteString("AVISO: a saída foi CORTADA por tamanho — o que está abaixo é parcial.\n")
	}
	b.WriteString("stdout:\n" + textoOuVazio(out.Stdout))
	b.WriteString("stderr:\n" + textoOuVazio(out.Stderr))

	return ToolResult{
		CallID:  call.ID,
		Name:    call.Name,
		Content: strings.TrimRight(b.String(), "\n"),
		IsError: out.Failed(),
	}
}

// resultadoDeErro é a recusa que o modelo consegue corrigir.
func resultadoDeErro(call ToolCall, motivo string) ToolResult {
	return ToolResult{CallID: call.ID, Name: call.Name, Content: motivo, IsError: true}
}

func textoOuVazio(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(vazio)\n"
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// resumir corta um texto para caber numa mensagem de erro, dizendo que cortou.
func resumir(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(vazio)"
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "… (cortado)"
}
