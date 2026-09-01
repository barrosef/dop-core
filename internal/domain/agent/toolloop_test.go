package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ════════════════════════════════════════════════════════════════════════════
// O LAÇO DE FERRAMENTA, testado sem fornecedor e sem substrato de verdade.
//
// O que estes testes protegem é caro e silencioso: um laço sem teto é conta sem
// teto; uma medição que só conta a última volta faz o orçamento enxergar uma
// fração do gasto; uma falha de ferramenta entregue como erro do turno tira do
// modelo a única informação que resolve o problema dele.
// ════════════════════════════════════════════════════════════════════════════

// sandboxFalso é o duplo da porta estreita do substrato.
//
// Ele guarda TUDO o que recebeu — inclusive para a varredura de credencial, que
// é a única forma de provar, de fora, que nada da plataforma escorre para dentro
// do sandbox.
type sandboxFalso struct {
	comandos   []agent.SandboxCommand
	demandas   []string
	saida      agent.SandboxOutput
	erro       error
	chamadas   int
	porChamada []agent.SandboxOutput
}

func (s *sandboxFalso) RunCommand(_ context.Context, demandID string,
	cmd agent.SandboxCommand) (agent.SandboxOutput, error) {

	s.chamadas++
	s.demandas = append(s.demandas, demandID)
	s.comandos = append(s.comandos, cmd)
	if s.erro != nil {
		return agent.SandboxOutput{}, s.erro
	}
	if n := len(s.porChamada); n > 0 {
		i := s.chamadas - 1
		if i >= n {
			i = n - 1
		}
		return s.porChamada[i], nil
	}
	return s.saida, nil
}

// ── auxiliares ──────────────────────────────────────────────────────────────

func fichaComFerramenta() agent.AgentCard {
	return agent.AgentCard{Purpose: "implementar", Tools: []string{agent.ToolRunCommand}}
}

func respostaPedindoFerramenta(id, argumento string) *agent.Reply {
	return &agent.Reply{
		Text:  "vou olhar o repositório",
		Model: "x-grande",
		Usage: agent.Usage{InputTokens: 100, OutputTokens: 20},
		// A parada é `tool_use`, mas quem decide continuar é a PRESENÇA das
		// chamadas — ver o laço: parada de ferramenta sem chamada nenhuma é
		// incoerência do fornecedor e não pode travar o turno.
		StopReason:    agent.StopToolUse,
		EffortApplied: agent.EffortHigh,
		ToolCalls: []agent.ToolCall{{
			ID: id, Name: agent.ToolRunCommand,
			Input: map[string]any{"command": []any{"sh", "-c", argumento}},
		}},
	}
}

// montarComFerramentas monta o cenário com substrato ligado.
func montarComFerramentas(t *testing.T, sb agent.Sandbox, conta agent.Accounting,
	respostas []*agent.Reply, opts ...agent.Option) cenario {

	t.Helper()
	prov := &provedorFalso{info: fichaDoProvedor(), respostas: respostas, resposta: respostas[0]}
	cust := &custoFalso{
		decisao: agent.Decision{TaskKind: "implementation", Class: agent.ClassStrong,
			Model: "claude-opus", Effort: agent.EffortHigh, Reason: "ADR-0011 §3"},
		conta: conta,
	}
	conv := &conversaFalsa{thread: agent.Thread{
		ID: "thr-1", Key: "principal", Card: fichaComFerramenta(),
	}}
	todas := append([]agent.Option{agent.WithSandbox(sb)}, opts...)
	return cenario{
		svc:  agent.NewService(prov, conhecimentoFalso{}, cust, conv, todas...),
		prov: prov, cust: cust, conv: conv,
	}
}

// ── o caminho feliz ─────────────────────────────────────────────────────────

// O agente pede, a ferramenta roda no sandbox, o resultado volta, o agente
// conclui. É a ponte inteira em um teste.
func TestLacoExecutaFerramentaEContinua(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{
		ExitCode: 0, Stdout: "MARCA-DA-SAIDA-DO-COMANDO\n",
	}}
	c := montarComFerramentas(t, sb, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "git status"),
		respostaConcluindo(),
	})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.ToolRounds != 2 {
		t.Fatalf("esperava 2 voltas, veio %d", out.ToolRounds)
	}
	if out.LoopStop != agent.LoopFinished {
		t.Fatalf("parada %q, esperava %q", out.LoopStop, agent.LoopFinished)
	}
	if sb.chamadas != 1 {
		t.Fatalf("o sandbox foi chamado %d vez(es), esperava 1", sb.chamadas)
	}
	// A ferramenta rodou o comando que o MODELO pediu, e na demanda certa.
	if got := sb.comandos[0].Command; strings.Join(got, " ") != "sh -c git status" {
		t.Fatalf("o comando chegou torto ao substrato: %v", got)
	}
	if sb.demandas[0] != "dem-1" {
		t.Fatalf("o comando foi para a demanda %q", sb.demandas[0])
	}

	// E o RESULTADO voltou para o modelo na volta seguinte — sem isso o laço é
	// só uma chamada extra que o agente nunca lê.
	if len(c.prov.turnos) != 2 {
		t.Fatalf("o provedor recebeu %d turno(s)", len(c.prov.turnos))
	}
	segundo := c.prov.turnos[1]
	if !mensagemComResultado(segundo, "MARCA-DA-SAIDA-DO-COMANDO") {
		t.Fatalf("a saída do comando não voltou ao modelo:\n%+v", segundo.Messages)
	}
	// A fala do assistente com a chamada é reenviada junto: sem ela, o
	// resultado fica órfão e os dois fornecedores recusam com 400 (D11).
	if !mensagemComChamada(segundo, "call-1") {
		t.Fatalf("a chamada não foi reenviada na história:\n%+v", segundo.Messages)
	}
	// Concluiu de verdade: achado publicado.
	if !out.Concluded || out.Finding == nil {
		t.Fatalf("o turno não concluiu depois da ferramenta: %+v", out)
	}
}

// Cada volta consome, e o total do turno é a SOMA. Registrar só a última
// subestimaria o gasto por um fator igual ao número de voltas (ADR-0011 §2).
func TestConsumoSomaTodasAsVoltas(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	c := montarComFerramentas(t, sb, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "ls"),
		respostaConcluindo(), // 1000 de entrada, 200 de saída
	})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-9")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(c.cust.consumos) != 2 {
		t.Fatalf("esperava 2 registros de consumo (um por volta), veio %d", len(c.cust.consumos))
	}
	// Chaves DERIVADAS e distintas por volta: uma chave só faria o domínio de
	// custo descartar a segunda como duplicata, e o orçamento enxergaria metade.
	if c.cust.chavesUso[0] != "turno-9:usage:1" || c.cust.chavesUso[1] != "turno-9:usage:2" {
		t.Fatalf("chaves de consumo: %v", c.cust.chavesUso)
	}
	quero := int64(100 + 1000)
	if out.Usage.InputTokens != quero {
		t.Fatalf("ORÇAMENTO FICTÍCIO: entrada somada deu %d, esperava %d — registrar só a "+
			"última volta esconde o gasto das outras", out.Usage.InputTokens, quero)
	}
	if out.Usage.OutputTokens != 20+200 {
		t.Fatalf("saída somada deu %d", out.Usage.OutputTokens)
	}
	// E o custo também é somado, com o preço do modelo de cada volta.
	if !out.Usage.CostKnown || out.Usage.CostMicros <= 0 {
		t.Fatalf("custo somado veio %+v", out.Usage)
	}
}

// ── o teto ──────────────────────────────────────────────────────────────────

// Laço sem teto é conta sem teto. "Bati o teto" precisa ser distinguível de
// "terminei" — e um laço interrompido NÃO conclui thread.
func TestTetoDeVoltasParaOLacoEhLegivel(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	// Um agente teimoso: pede ferramenta para sempre, e ainda marca conclusão.
	insistente := respostaPedindoFerramenta("call-x", "npm test")
	insistente.Data = map[string]any{
		"reply": "quase lá", "concluded": true,
		"finding_title": "titulo", "finding_summary": "resumo",
	}
	c := montarComFerramentas(t, sb, agent.Accounting{},
		[]*agent.Reply{insistente}, agent.WithMaxToolRounds(3))

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-teto")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.ToolRounds != 3 {
		t.Fatalf("LAÇO SEM TETO: deu %d voltas contra um teto de 3", out.ToolRounds)
	}
	if out.LoopStop != agent.LoopMaxRounds {
		t.Fatalf("parada %q, esperava %q — 'bati o teto' é diferente de 'terminei'",
			out.LoopStop, agent.LoopMaxRounds)
	}
	if out.MaxToolRounds != 3 {
		t.Fatalf("o teto que valeu não viajou no resultado: %d", out.MaxToolRounds)
	}
	// A última volta pediu ferramenta e ela NÃO rodou: o teto para antes.
	if sb.chamadas != 2 {
		t.Fatalf("o sandbox rodou %d vez(es); com teto 3, a última volta não executa", sb.chamadas)
	}
	if !algumAvisoContem(out.Warnings, "teto") {
		t.Fatalf("bater o teto não gerou aviso legível: %v", out.Warnings)
	}
	// A thread precisa DIZER que o trabalho parou no meio.
	if !algumaMensagemContem(c.conv.mensagens, "teto de 3 volta") {
		t.Fatal("a thread não recebeu o aviso da parada: quem lê a conversa veria um " +
			"trabalho interrompido com cara de conclusão")
	}
	// E a conclusão que o modelo marcou é RECUSADA: achado escrito no meio do
	// trabalho é durável e passa a valer como verdade.
	if out.Concluded || out.Finding != nil || len(c.conv.achados) != 0 {
		t.Fatal("o laço parou no teto e o turno CONCLUIU assim mesmo: o achado entraria na " +
			"memória do projeto como se o trabalho tivesse terminado")
	}
}

// O chamador pode ABAIXAR o teto, nunca levantar: teto que o cliente levanta não
// é teto, é sugestão.
func TestTetoDoChamadorSoAbaixa(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	c := montarComFerramentas(t, sb, agent.Accounting{},
		[]*agent.Reply{respostaPedindoFerramenta("c", "ls")}, agent.WithMaxToolRounds(4))

	req := pedido()
	req.MaxToolRounds = 99 // tentativa de levantar
	alto, err := c.svc.RunTurn(contexto(), req, "turno-a")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if alto.MaxToolRounds != 4 {
		t.Fatalf("o chamador LEVANTOU o teto para %d: um teto que o cliente levanta não é "+
			"teto (ADR-0011 §2)", alto.MaxToolRounds)
	}

	c2 := montarComFerramentas(t, &sandboxFalso{}, agent.Accounting{},
		[]*agent.Reply{respostaPedindoFerramenta("c", "ls")}, agent.WithMaxToolRounds(4))
	req.MaxToolRounds = 2
	baixo, err := c2.svc.RunTurn(contexto(), req, "turno-b")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if baixo.MaxToolRounds != 2 || baixo.ToolRounds != 2 {
		t.Fatalf("o chamador não conseguiu ABAIXAR o teto: %+v", baixo)
	}
}

// ── orçamento ───────────────────────────────────────────────────────────────

// Orçamento estourado no meio do laço PARA o laço; não mata o turno. O que já
// rodou é entregue (ADR-0011 §2).
func TestOrcamentoEstouradoParaOLacoSemMatarOTurno(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	estourado := agent.Accounting{
		BudgetExceeded: true,
		Exceeded: []agent.BudgetView{{
			Scope: "demand", ScopeID: "dem-1", LimitMicros: 10, SpentMicros: 99, Currency: "USD",
		}},
	}
	c := montarComFerramentas(t, sb, estourado, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "npm test"),
		respostaConcluindo(),
	})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-orc")
	if err != nil {
		t.Fatalf("orçamento estourado MATOU o turno: %v — a ADR-0011 §2 recusou o corte duro", err)
	}
	if out.LoopStop != agent.LoopBudget {
		t.Fatalf("parada %q, esperava %q", out.LoopStop, agent.LoopBudget)
	}
	if out.ToolRounds != 1 {
		t.Fatalf("o laço deu %d voltas depois do estouro", out.ToolRounds)
	}
	if sb.chamadas != 0 {
		t.Fatal("a ferramenta rodou DEPOIS do orçamento estourar: o laço para antes da " +
			"próxima volta, e a próxima volta inclui executar o que foi pedido")
	}
	if !out.Paused || out.Notice == "" {
		t.Fatalf("o turno não saiu pausado com aviso: %+v", out)
	}
	// O que JÁ rodou é entregue: a resposta do modelo está na thread.
	if out.Reply == "" || len(out.MessageIDs) < 2 {
		t.Fatalf("o turno interrompido não entregou o que já tinha: %+v", out)
	}
	// E o consumo daquela volta ficou registrado: tokens pagos não somem.
	if len(c.cust.consumos) != 1 {
		t.Fatalf("consumo registrado %d vez(es)", len(c.cust.consumos))
	}
}

// ── falha de ferramenta × falha de infraestrutura ───────────────────────────

// Comando que sai com código != 0 é RESULTADO: o modelo precisa ver a falha para
// corrigir. O turno não morre.
func TestFalhaDaFerramentaEhResultadoENaoErroDoTurno(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{
		ExitCode: 2, Stdout: "", Stderr: "FALHA-DO-COMANDO: teste reprovou",
	}}
	c := montarComFerramentas(t, sb, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "npm test"),
		respostaConcluindo(),
	})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-falha")
	if err != nil {
		t.Fatalf("FALHA DE FERRAMENTA MATOU O TURNO: %v — o modelo precisa VER que o "+
			"comando falhou para corrigir", err)
	}
	if out.LoopStop != agent.LoopFinished {
		t.Fatalf("parada %q", out.LoopStop)
	}
	res := resultadoNaHistoria(c.prov.turnos[1], "call-1")
	if res == nil {
		t.Fatal("o resultado não chegou ao modelo")
	}
	if !res.IsError {
		t.Fatal("o resultado de um comando que saiu com código 2 não foi marcado como ERRO: " +
			"o modelo leria a falha como saída normal")
	}
	if !strings.Contains(res.Content, "FALHA-DO-COMANDO") || !strings.Contains(res.Content, "exit_code: 2") {
		t.Fatalf("o resultado não conta ao modelo o que houve:\n%s", res.Content)
	}
}

// Sandbox caiu é outra coisa: sobe e mata o turno. Nenhuma quantidade de tokens
// gastos pelo modelo conserta um cluster fora do ar.
func TestFalhaDeInfraestruturaSobeEMataOTurno(t *testing.T) {
	sb := &sandboxFalso{erro: errs.New(errs.KindUnavailable, "o cluster não respondeu")}
	c := montarComFerramentas(t, sb, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "ls"),
		respostaConcluindo(),
	})

	_, err := c.svc.RunTurn(contexto(), pedido(), "turno-infra")
	if err == nil {
		t.Fatal("substrato fora do ar virou resultado de ferramenta: insistir com o modelo " +
			"contra uma parede é queimar dinheiro")
	}
	if errs.KindOf(err) != errs.KindUnavailable {
		t.Fatalf("erro do tipo %q, esperava indisponível", errs.KindOf(err))
	}
}

// Recusa SOBRE A CHAMADA (nome errado, permissão) é coisa que o modelo conserta:
// vira resultado de erro, não erro do turno.
func TestRecusaSobreAChamadaViraResultado(t *testing.T) {
	sb := &sandboxFalso{erro: errs.Invalid("comando não informado")}
	c := montarComFerramentas(t, sb, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "ls"),
		respostaConcluindo(),
	})

	out, err := c.svc.RunTurn(contexto(), pedido(), "turno-recusa")
	if err != nil {
		t.Fatalf("recusa sobre a chamada matou o turno: %v", err)
	}
	res := resultadoNaHistoria(c.prov.turnos[1], "call-1")
	if res == nil || !res.IsError {
		t.Fatalf("a recusa não voltou como resultado de erro: %+v", res)
	}
	if out.LoopStop != agent.LoopFinished {
		t.Fatalf("parada %q", out.LoopStop)
	}
}

// ── ferramenta não declarada e argumento ilegível (D8, D11) ─────────────────

func TestFerramentaNaoConcedidaViraResultadoDeErro(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0}}
	pedindo := respostaPedindoFerramenta("call-1", "ls")
	pedindo.ToolCalls[0].Name = "apagar_o_banco"
	c := montarComFerramentas(t, sb, agent.Accounting{},
		[]*agent.Reply{pedindo, respostaConcluindo()})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-nd"); err != nil {
		t.Fatalf("ferramenta desconhecida matou o turno: %v", err)
	}
	if sb.chamadas != 0 {
		t.Fatal("O SUBSTRATO EXECUTOU UMA FERRAMENTA QUE NÃO FOI CONCEDIDA: nenhum dos dois " +
			"fornecedores impede o modelo de chamar o que não existe — quem impede é o laço")
	}
	res := resultadoNaHistoria(c.prov.turnos[1], "call-1")
	if res == nil || !res.IsError {
		t.Fatalf("a recusa não voltou ao modelo: %+v", res)
	}
	// A lista de nomes válidos vai junto: recusa sem alternativa faz o modelo
	// tentar o mesmo nome de novo, e cada tentativa é uma volta paga.
	if !strings.Contains(res.Content, agent.ToolRunCommand) {
		t.Fatalf("a recusa não diz o que existe:\n%s", res.Content)
	}
}

func TestArgumentoIlegivelViraResultadoDeErroComOTextoCru(t *testing.T) {
	sb := &sandboxFalso{}
	pedindo := respostaPedindoFerramenta("call-1", "ls")
	// É o que o adaptador entrega quando o fornecedor manda algo que não
	// decodifica (D8): Input nulo, RawInput com o que veio.
	pedindo.ToolCalls[0].Input = nil
	pedindo.ToolCalls[0].RawInput = `{"command": ["sh", "-c", "npm te`
	c := montarComFerramentas(t, sb, agent.Accounting{},
		[]*agent.Reply{pedindo, respostaConcluindo()})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-arg"); err != nil {
		t.Fatalf("argumento ilegível matou o turno: %v", err)
	}
	if sb.chamadas != 0 {
		t.Fatal("o laço executou uma chamada cujos argumentos não decodificaram")
	}
	res := resultadoNaHistoria(c.prov.turnos[1], "call-1")
	if res == nil || !res.IsError {
		t.Fatalf("a recusa não voltou ao modelo: %+v", res)
	}
	if !strings.Contains(res.Content, "npm te") {
		t.Fatalf("o texto CRU não voltou ao modelo: 'seu argumento é inválido' sem dizer "+
			"qual argumento não conserta nada:\n%s", res.Content)
	}
}

// Argumento válido no JSON mas errado no schema também é assunto do modelo.
func TestComandoAusenteViraResultadoDeErro(t *testing.T) {
	sb := &sandboxFalso{}
	pedindo := respostaPedindoFerramenta("call-1", "ls")
	pedindo.ToolCalls[0].Input = map[string]any{"comando": "git status"} // campo errado
	c := montarComFerramentas(t, sb, agent.Accounting{},
		[]*agent.Reply{pedindo, respostaConcluindo()})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-cmd"); err != nil {
		t.Fatalf("argumento fora do schema matou o turno: %v", err)
	}
	if sb.chamadas != 0 {
		t.Fatal("o laço executou uma chamada sem comando")
	}
	res := resultadoNaHistoria(c.prov.turnos[1], "call-1")
	if res == nil || !strings.Contains(res.Content, "command") {
		t.Fatalf("a recusa não diz qual campo falta: %+v", res)
	}
}

// ── a garantia mais cara: nada da plataforma entra no sandbox ───────────────

// O sandbox roda código de agente, que lê conteúdo não confiável (spec do
// substrato §6). A credencial do provedor de modelo mora no cofre e é usada no
// mesmo processo (ADR-0023) — e não pode escorrer para dentro do sandbox por
// nenhuma fresta do laço.
//
// A prova estrutural é a porta: `SandboxCommand` não tem campo de ambiente nem
// de credencial (nem `ports.ExecRequest` tem). Este teste é a prova de
// COMPORTAMENTO: nada do que atravessa o laço — nem o prefixo do prompt, que
// contém a ficha da thread, nem o texto do usuário — chega ao substrato.
func TestNadaDoQuePassaPeloLacoCarregaCredencial(t *testing.T) {
	const chave = "sk-SENTINELA-DE-CREDENCIAL-NAO-PODE-CHEGAR-AO-SANDBOX"

	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	// Um modelo hostil: tenta arrastar a chave para dentro por três caminhos —
	// no comando, num campo extra do argumento, e no id da chamada.
	pedindo := respostaPedindoFerramenta("call-"+chave, "echo oi")
	pedindo.ToolCalls[0].Input["credencial"] = chave
	pedindo.Text = "vou usar " + chave

	c := montarComFerramentas(t, sb, agent.Accounting{},
		[]*agent.Reply{pedindo, respostaConcluindo()})
	// E o provedor carrega a chave na ficha dele, que é o pior caso: uma ficha
	// que vazasse para o comando entregaria a chave de todas as contas.
	c.prov.info.Prices = map[string]agent.Price{chave: {Currency: "USD"}}

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-cred"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if sb.chamadas != 1 {
		t.Fatalf("o sandbox foi chamado %d vez(es)", sb.chamadas)
	}
	// A varredura é sobre o comando INTEIRO, serializado: é a única forma de
	// não depender de lembrar quais campos existem hoje.
	bruto, err := json.Marshal(sb.comandos[0])
	if err != nil {
		t.Fatalf("serialização do comando: %v", err)
	}
	if strings.Contains(string(bruto), chave) {
		t.Fatalf("A CREDENCIAL ATRAVESSOU O LAÇO E CHEGOU AO SUBSTRATO: %s", bruto)
	}
	// E a demanda também não pode carregar nada além do id.
	if strings.Contains(sb.demandas[0], chave) {
		t.Fatal("a credencial foi parar no identificador da demanda")
	}
}

// ── instalação sem substrato ────────────────────────────────────────────────

// Ficha que concede ferramenta numa instalação sem substrato: o turno roda, as
// ferramentas NÃO são declaradas, e o aviso diz por quê. As três alternativas
// piores são declarar (o agente planeja em cima do que não existe), silenciar (a
// ficha parece honrada) e recusar (uma linha de ficha para o trabalho inteiro).
func TestFichaComFerramentaSemSubstratoAvisaENaoDeclara(t *testing.T) {
	prov := &provedorFalso{info: fichaDoProvedor(), resposta: respostaConcluindo()}
	cust := &custoFalso{decisao: agent.Decision{TaskKind: "implementation", Class: agent.ClassStrong}}
	conv := &conversaFalsa{thread: agent.Thread{ID: "thr-1", Key: "principal", Card: fichaComFerramenta()}}
	svc := agent.NewService(prov, conhecimentoFalso{}, cust, conv) // SEM WithSandbox

	out, err := svc.RunTurn(contexto(), pedido(), "turno-sem-sb")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(prov.turnoPedido.Tools) != 0 {
		t.Fatalf("as ferramentas foram DECLARADAS sem substrato para executá-las: %+v",
			prov.turnoPedido.Tools)
	}
	if !algumAvisoContem(out.Warnings, "substrato de execução") {
		t.Fatalf("a ficha prometeu ação, nada foi ligado, e ninguém avisou: %v", out.Warnings)
	}
}

// Modelo que pede ferramenta numa instalação sem substrato: o laço para e DIZ.
func TestPedidoDeFerramentaSemSubstratoParaOLacoComAviso(t *testing.T) {
	prov := &provedorFalso{
		info: fichaDoProvedor(), resposta: respostaPedindoFerramenta("call-1", "ls"),
	}
	cust := &custoFalso{decisao: agent.Decision{TaskKind: "implementation", Class: agent.ClassStrong}}
	conv := &conversaFalsa{thread: agent.Thread{ID: "thr-1", Key: "principal"}}
	svc := agent.NewService(prov, conhecimentoFalso{}, cust, conv)

	out, err := svc.RunTurn(contexto(), pedido(), "turno-sem-sb2")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.LoopStop != agent.LoopNoSandbox {
		t.Fatalf("parada %q, esperava %q", out.LoopStop, agent.LoopNoSandbox)
	}
	if out.ToolRounds != 1 {
		t.Fatalf("o laço deu %d voltas sem ter o que executar", out.ToolRounds)
	}
}

// ── o catálogo ──────────────────────────────────────────────────────────────

func TestCatalogoDeFerramentas(t *testing.T) {
	t.Run("sem_concessao_nao_ha_ferramenta", func(t *testing.T) {
		specs, desconhecidas := agent.ToolCatalog(nil)
		if len(specs) != 0 || len(desconhecidas) != 0 {
			t.Fatalf("ficha sem ferramenta declarou %d: agente sem concessão não age", len(specs))
		}
	})

	t.Run("nome_inexistente_volta_separado", func(t *testing.T) {
		specs, desconhecidas := agent.ToolCatalog(
			[]string{"apagar_o_banco", agent.ToolRunCommand, "  "})
		if len(specs) != 1 || specs[0].Name != agent.ToolRunCommand {
			t.Fatalf("as concedidas válidas não saíram: %+v", specs)
		}
		if len(desconhecidas) != 1 || desconhecidas[0] != "apagar_o_banco" {
			t.Fatalf("o nome inexistente não voltou separado: %v — silenciá-lo faria o "+
				"agente parecer capaz de algo que ninguém instalou", desconhecidas)
		}
	})

	t.Run("ordem_alfabetica_e_sem_repeticao", func(t *testing.T) {
		// A ordem é a do catálogo, não a da ficha: a ordem em que alguém
		// digitou duas ferramentas não é escolha de ninguém, mas mudaria os
		// bytes do prefixo e a entrada de cache junto (ADR-0012 §1).
		a, _ := agent.ToolCatalog([]string{agent.ToolRunCommand, agent.ToolRunCommand})
		if len(a) != 1 {
			t.Fatalf("nome repetido virou duas declarações: %+v", a)
		}
	})

	t.Run("o_schema_nao_e_compartilhado", func(t *testing.T) {
		// Duas chamadas precisam devolver mapas DIFERENTES: um mapa
		// compartilhado deixaria o primeiro adaptador que o alterasse mudar o
		// contrato de todos os turnos de todas as contas.
		a, _ := agent.ToolCatalog([]string{agent.ToolRunCommand})
		b, _ := agent.ToolCatalog([]string{agent.ToolRunCommand})
		a[0].InputSchema["envenenado"] = true
		if _, ok := b[0].InputSchema["envenenado"]; ok {
			t.Fatal("o schema da ferramenta é COMPARTILHADO entre chamadas")
		}
	})
}

// ── auxiliares de leitura ───────────────────────────────────────────────────

func mensagemComResultado(t agent.Turn, trecho string) bool {
	for _, m := range t.Messages {
		for _, r := range m.ToolResults {
			if strings.Contains(r.Content, trecho) {
				return true
			}
		}
	}
	return false
}

func mensagemComChamada(t agent.Turn, id string) bool {
	for _, m := range t.Messages {
		if m.Role != agent.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == id {
				return true
			}
		}
	}
	return false
}

func resultadoNaHistoria(t agent.Turn, callID string) *agent.ToolResult {
	for _, m := range t.Messages {
		for i, r := range m.ToolResults {
			if r.CallID == callID {
				return &m.ToolResults[i]
			}
		}
	}
	return nil
}

func algumAvisoContem(avisos []string, trecho string) bool {
	for _, a := range avisos {
		if strings.Contains(a, trecho) {
			return true
		}
	}
	return false
}

func algumaMensagemContem(msgs []mensagemGravada, trecho string) bool {
	for _, m := range msgs {
		if strings.Contains(m.texto, trecho) {
			return true
		}
	}
	return false
}

// Só "terminei" admite conclusão de thread.
//
// A regra vive aqui, na forma pura, porque no ciclo do turno ela é a SEGUNDA
// tranca de uma porta que `executeTurn` já trancou (ver service.go): toda parada
// que não é `finished` acontece com chamada de ferramenta pendente, e chamada
// pendente já zera `concluded`. A redundância é deliberada — as duas regras são
// sobre coisas diferentes —, e testá-la aqui é o que impede que ela apodreça sem
// ninguém notar.
func TestSoOFimDoLacoAdmiteConclusao(t *testing.T) {
	if !agent.LoopFinished.Concluded() {
		t.Fatal("o laço terminou sozinho e a conclusão foi recusada")
	}
	for _, parada := range []agent.LoopStop{
		agent.LoopMaxRounds, agent.LoopBudget, agent.LoopNoSandbox,
	} {
		if parada.Concluded() {
			t.Fatalf("a parada %q admitiu conclusão: um achado escrito no meio do trabalho "+
				"é durável e passa a valer como verdade na memória do projeto", parada)
		}
	}
}

// ════════════════════════════════════════════════════════════════════════════
// AS CINCO SONDAS — o que a suíte NÃO pegava depois de todas as quebras
// esperadas terem reprovado.
//
// Cada teste abaixo nasceu de uma quebra deliberada que PASSOU LIMPO. É o mesmo
// método que descobriu, na entrega anterior, que apagar o breakpoint de cache da
// Anthropic não reprovava nada e multiplicava a conta por dez: quebrar o óbvio
// prova que a suíte funciona; quebrar o não-óbvio é o que descobre o que ela não
// vê. Nenhuma das cinco falha com erro — todas falham com fatura, com uma
// conclusão errada, ou com um comando que ninguém pediu.
// ════════════════════════════════════════════════════════════════════════════

// SONDA 1 — a mais cara. O prefixo estável precisa ser BYTE A BYTE o mesmo em
// todas as voltas do laço.
//
// A quebra que passou limpo: acrescentar um byte ao prefixo a cada volta. Nada
// erra. O laço continua funcionando, o agente continua respondendo, os testes
// continuam verdes — e cada volta passa a pagar o prefixo INTEIRO como entrada
// nova, a 10× o preço da leitura de cache (ADR-0012 §1). Num turno de oito
// voltas com um pacote de contexto grande, é a diferença entre centavos e
// dólares por turno, multiplicada por toda thread de toda conta.
func TestPrefixoEstavelNaoMudaEntreAsVoltas(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	c := montarComFerramentas(t, sb, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "ls"),
		respostaPedindoFerramenta("call-2", "cat go.mod"),
		respostaConcluindo(),
	})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-cache"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(c.prov.turnos) != 3 {
		t.Fatalf("esperava 3 voltas, veio %d", len(c.prov.turnos))
	}
	base := c.prov.turnos[0]
	for i, turno := range c.prov.turnos[1:] {
		if turno.StablePrefix != base.StablePrefix {
			t.Fatalf("O PREFIXO ESTÁVEL MUDOU NA VOLTA %d: nada falha por isso, e cada volta "+
				"passa a pagar o prefixo inteiro como entrada nova (~10× a leitura de cache, "+
				"ADR-0012 §1). É o defeito que só a fatura conta.\nvolta 1: %q\nvolta %d: %q",
				i+2, base.StablePrefix, i+2, turno.StablePrefix)
		}
		if turno.Fingerprint() != base.Fingerprint() {
			t.Fatalf("a impressão digital do prefixo mudou na volta %d", i+2)
		}
		// A DECLARAÇÃO de ferramenta também é prefixo nos dois fornecedores
		// (garantia 15). Uma lista que muda de conteúdo ou de ordem entre
		// voltas invalida o cache pela mesma porta.
		if len(turno.Tools) != len(base.Tools) {
			t.Fatalf("a declaração de ferramentas mudou na volta %d: %+v", i+2, turno.Tools)
		}
		for j := range turno.Tools {
			if turno.Tools[j].Name != base.Tools[j].Name {
				t.Fatalf("a ORDEM das ferramentas mudou na volta %d: %+v", i+2, turno.Tools)
			}
		}
	}
}

// SONDA 2 — saída cortada que não se anuncia.
//
// A quebra que passou limpo: apagar o aviso de corte do resultado que volta ao
// modelo. O comando continua rodando, o resultado continua chegando, o teto
// continua sendo respeitado — e o agente conclui a partir de metade de um log
// achando que leu o log inteiro. É a mesma classe do truncamento de contexto
// (ADR-0012), que o runtime já anuncia em dois lugares: a conclusão sai errada e
// ninguém consegue explicar por quê depois.
func TestSaidaCortadaEhAnunciadaAoModelo(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{
		ExitCode: 0, Stdout: "primeiras linhas do log", Truncated: true,
	}}
	c := montarComFerramentas(t, sb, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "cat /var/log/enorme"),
		respostaConcluindo(),
	})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-corte"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	res := resultadoNaHistoria(c.prov.turnos[1], "call-1")
	if res == nil {
		t.Fatal("o resultado não chegou ao modelo")
	}
	if !strings.Contains(strings.ToUpper(res.Content), "CUT") {
		t.Fatalf("A SAÍDA FOI CORTADA E O MODELO NÃO FOI AVISADO: ele vai concluir a partir "+
			"de metade do log achando que leu tudo, e a conclusão errada não terá "+
			"explicação depois.\nresultado:\n%s", res.Content)
	}
}

// SONDA 3 — o prazo que o MODELO pede não pode ser ilimitado.
//
// A quebra que passou limpo: remover o teto de `timeout_seconds`. O schema aceita
// o campo, o modelo pode escrever 86400, e um único comando pendurado passa a
// segurar o turno por um dia. Não é erro: é uma thread que nunca responde e um
// sandbox que nunca suspende por ociosidade porque o comando ainda está "vivo".
func TestPrazoPedidoPeloModeloEhLimitado(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0}}
	pedindo := respostaPedindoFerramenta("call-1", "sleep infinity")
	pedindo.ToolCalls[0].Input["timeout_seconds"] = float64(86400) // um dia
	c := montarComFerramentas(t, sb, agent.Accounting{},
		[]*agent.Reply{pedindo, respostaConcluindo()})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-prazo"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got := sb.comandos[0].TimeoutSeconds; got > agent.MaxToolTimeoutSeconds {
		t.Fatalf("O MODELO ESCOLHEU UM PRAZO DE %ds, ACIMA DO TETO DE %ds: um comando "+
			"pendurado passa a segurar o turno indefinidamente, e a thread nunca responde",
			got, agent.MaxToolTimeoutSeconds)
	}
	// E o padrão continua valendo quando ele não pede nada.
	sb2 := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0}}
	c2 := montarComFerramentas(t, sb2, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "ls"), respostaConcluindo(),
	})
	if _, err := c2.svc.RunTurn(contexto(), pedido(), "turno-prazo2"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if sb2.comandos[0].TimeoutSeconds != agent.DefaultToolTimeoutSeconds {
		t.Fatalf("sem pedido do modelo, o prazo veio %ds", sb2.comandos[0].TimeoutSeconds)
	}
}

// SONDA 4 — a ordem dos resultados é a das chamadas (D10).
//
// A quebra que passou limpo: inverter os resultados antes de devolvê-los. Os dois
// fornecedores aceitam — o vínculo é por id, não por posição —, e nada falha. O
// que muda é a LEITURA do modelo: "rodei o teste e depois li o log" vira "li o
// log e depois rodei o teste", e ele passa a raciocinar sobre uma sequência de
// eventos que não aconteceu.
func TestResultadosVoltamNaOrdemDasChamadas(t *testing.T) {
	sb := &sandboxFalso{porChamada: []agent.SandboxOutput{
		{ExitCode: 0, Stdout: "SAIDA-DA-PRIMEIRA"},
		{ExitCode: 0, Stdout: "SAIDA-DA-SEGUNDA"},
	}}
	// Duas chamadas na MESMA volta — é o paralelismo que os dois fornecedores
	// fazem por padrão (D10).
	pedindo := respostaPedindoFerramenta("call-1", "primeiro")
	pedindo.ToolCalls = append(pedindo.ToolCalls, agent.ToolCall{
		ID: "call-2", Name: agent.ToolRunCommand,
		Input: map[string]any{"command": []any{"sh", "-c", "segundo"}},
	})
	c := montarComFerramentas(t, sb, agent.Accounting{},
		[]*agent.Reply{pedindo, respostaConcluindo()})

	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-ordem"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	var resultados []agent.ToolResult
	for _, m := range c.prov.turnos[1].Messages {
		if m.Role == agent.RoleToolResult {
			resultados = append(resultados, m.ToolResults...)
		}
	}
	if len(resultados) != 2 {
		t.Fatalf("esperava 2 resultados, vieram %d", len(resultados))
	}
	if resultados[0].CallID != "call-1" || resultados[1].CallID != "call-2" {
		t.Fatalf("OS RESULTADOS VOLTARAM FORA DA ORDEM DAS CHAMADAS (%q, %q): os fornecedores "+
			"aceitam, porque o vínculo é por id — quem lê errado é o MODELO, que passa a "+
			"raciocinar sobre uma sequência de eventos que não aconteceu (D10)",
			resultados[0].CallID, resultados[1].CallID)
	}
	if !strings.Contains(resultados[0].Content, "SAIDA-DA-PRIMEIRA") {
		t.Fatalf("o resultado da primeira chamada não é o da primeira execução:\n%s",
			resultados[0].Content)
	}
}

// SONDA 5 — o teto de saída do DOMÍNIO precisa chegar ao substrato.
//
// A quebra que passou limpo: mandar zero em `MaxOutputBytes`. A porta trata zero
// como "use o meu padrão", então nada falha e nada estoura — o que muda é que o
// teto passa a ser o da PORTA (64 KiB) em vez do do domínio (32 KiB), e cada
// resultado de ferramenta entra no contexto do próximo turno com o dobro do
// tamanho. É política de custo (ADR-0011) decidida por omissão, na camada errada.
func TestTetoDeSaidaDoDominioChegaAoSubstrato(t *testing.T) {
	sb := &sandboxFalso{saida: agent.SandboxOutput{ExitCode: 0}}
	c := montarComFerramentas(t, sb, agent.Accounting{}, []*agent.Reply{
		respostaPedindoFerramenta("call-1", "ls"), respostaConcluindo(),
	})
	if _, err := c.svc.RunTurn(contexto(), pedido(), "turno-teto-saida"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got := sb.comandos[0].MaxOutputBytes; got != agent.DefaultToolOutputBytes {
		t.Fatalf("o teto de saída chegou ao substrato como %d, esperava %d: zero faz a porta "+
			"usar o padrão DELA, e a política de custo passa a ser decidida por omissão na "+
			"camada errada (ADR-0011)", got, agent.DefaultToolOutputBytes)
	}
}
