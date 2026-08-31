package agent_test

import (
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

func pacoteCheio() agent.ContextPackage {
	return agent.ContextPackage{
		// Ordem da CURADORIA: a regra do projeto vem antes da regra da conta
		// porque o mais específico ganha (knowledge.ResolveRules). Alfabética
		// inverteria as duas.
		Rules: []string{"zelar pelo padrão do projeto", "abrir PR contra develop"},
		Index: []agent.ContextArtifact{
			{Name: "dop-core", Body: "mapa do núcleo"},
			{Name: "dop-api", ObjectRef: "gs://artefatos/dop-api"},
		},
		Memories: []agent.ContextArtifact{{Name: "lição de outubro", Body: "não mexer no relógio"}},
		Findings: []agent.ContextFinding{{Title: "o bug era o TTL", Summary: "cinco minutos"}},
	}
}

// O PREFIXO É O ATIVO: dois turnos da mesma thread precisam produzir bytes
// idênticos no topo, senão o desconto de cache some — sem erro, só na fatura.
func TestPrefixoEstavelEntreTurnos(t *testing.T) {
	pkg := pacoteCheio()
	card := agent.AgentCard{Purpose: "investigar", Tools: []string{"grep", "bash"}, BudgetMicros: 500}

	primeiro := buildTurn(pkg, "principal", card, "primeira pergunta", "", 0)
	segundo := buildTurn(pkg, "principal", card, "segunda pergunta, bem diferente",
		"o operador manda parar", 0)

	if primeiro.StablePrefix != segundo.StablePrefix {
		t.Fatalf("O PREFIXO MUDOU ENTRE TURNOS DA MESMA THREAD — é a economia da ADR-0012 §1 "+
			"indo embora em silêncio.\nprimeiro:\n%s\nsegundo:\n%s",
			primeiro.StablePrefix, segundo.StablePrefix)
	}
	if primeiro.Fingerprint() != segundo.Fingerprint() {
		t.Fatal("Fingerprint divergiu: ela é do prefixo, e só dele")
	}
	// E o texto do turno NÃO pode ter escorregado para dentro do prefixo — o
	// erro que o campo separado existe para impedir.
	if strings.Contains(primeiro.StablePrefix, "primeira pergunta") {
		t.Fatal("o texto do turno entrou no prefixo estável")
	}
	if strings.Contains(segundo.StablePrefix, "o operador manda parar") {
		t.Fatal("a intervenção do operador entrou no prefixo estável: ela é VOLÁTIL, e " +
			"reescrever o topo do prompt a cada intervenção é o que a ADR-0012 §1 evita")
	}
}

// Montar o mesmo turno duas vezes tem de dar exatamente os mesmos bytes: sem
// relógio, sem id volátil, sem coleção iterada fora de ordem.
func TestMontagemDeterministica(t *testing.T) {
	pkg := pacoteCheio()
	card := agent.AgentCard{Purpose: "investigar", Tools: []string{"zsh", "grep", "bash", "curl"}}
	base := buildTurn(pkg, "principal", card, "oi", "", 0)
	for i := 0; i < 50; i++ {
		if outro := buildTurn(pkg, "principal", card, "oi", "", 0); outro.StablePrefix != base.StablePrefix {
			t.Fatalf("prefixo não determinístico na tentativa %d", i)
		}
	}
	// A ficha do chamador não pode ser mexida por efeito colateral: ela é dado
	// da thread, e reordená-la aqui mudaria os bytes do prefixo no turno
	// seguinte sem que ninguém tivesse pedido.
	if card.Tools[0] != "zsh" {
		t.Fatalf("BuildTurn reordenou o slice do chamador: %v", card.Tools)
	}
	// E o que a ficha ANUNCIA são as ferramentas DECLARADAS, não as concedidas:
	// nenhuma destas quatro existe no catálogo do runtime, então o prefixo não
	// pode prometer nenhuma. Anunciar uma ferramenta inexistente faz o agente
	// planejar em cima dela e prometer ao humano que vai usá-la.
	if strings.Contains(base.StablePrefix, "Ferramentas concedidas") {
		t.Fatalf("a ficha anunciou ferramenta que o runtime não declara:\n%s", base.StablePrefix)
	}
}

// A ficha anuncia o que foi DECLARADO, e a declaração vai no campo próprio do
// turno — não interpolada no texto do prefixo, porque schema descrito em prosa é
// schema que nenhum fornecedor valida.
func TestFerramentasEntramNaFichaDaThread(t *testing.T) {
	card := agent.AgentCard{Purpose: "implementar", Tools: []string{agent.ToolRunCommand}}
	specs, desconhecidas := agent.ToolCatalog(card.Tools)
	if len(specs) != 1 || len(desconhecidas) != 0 {
		t.Fatalf("catálogo devolveu %d spec(s) e %d desconhecida(s)", len(specs), len(desconhecidas))
	}

	turno := agent.BuildTurn(agent.ContextPackage{}, "principal", card, "oi", "", 0, specs)
	if len(turno.Tools) != 1 || turno.Tools[0].Name != agent.ToolRunCommand {
		t.Fatalf("as ferramentas não chegaram ao turno: %+v", turno.Tools)
	}
	if !strings.Contains(turno.StablePrefix, "Ferramentas concedidas: "+agent.ToolRunCommand) {
		t.Fatalf("a ficha não anunciou a ferramenta declarada:\n%s", turno.StablePrefix)
	}
	// O prefixo continua sendo do PREFIXO: a declaração viaja no campo, e o
	// schema não pode ter vazado para o texto.
	if strings.Contains(turno.StablePrefix, "additionalProperties") {
		t.Fatal("o schema da ferramenta foi interpolado no prefixo: ele é do campo Tools, " +
			"e é o fornecedor quem precisa validá-lo")
	}
	// Determinismo, que é a camada 3 de prompt.go valendo também aqui.
	for i := 0; i < 20; i++ {
		outro := agent.BuildTurn(agent.ContextPackage{}, "principal", card, "oi", "", 0, specs)
		if outro.StablePrefix != turno.StablePrefix {
			t.Fatalf("prefixo com ferramentas não é determinístico (tentativa %d)", i)
		}
	}
}

// A ordem do núcleo é PRIORIDADE, não sugestão: ordenar aqui daria estabilidade
// pelo preço de desfazer a curadoria (ADR-0009 §3).
func TestOrdemDaCuradoriaEhPreservada(t *testing.T) {
	p := buildTurn(pacoteCheio(), "principal", agent.AgentCard{}, "oi", "", 0)
	iEspecifica := strings.Index(p.StablePrefix, "zelar pelo padrão do projeto")
	iGenerica := strings.Index(p.StablePrefix, "abrir PR contra develop")
	if iEspecifica < 0 || iGenerica < 0 {
		t.Fatal("as regras não entraram no prefixo")
	}
	if iEspecifica > iGenerica {
		t.Fatal("as regras foram REORDENADAS: a ordem do núcleo é a prioridade dele")
	}
}

// Bloco vazio não vira título órfão — e o layout continua sendo
// contrato → ficha → contexto.
func TestPacoteVazioNaoGeraTituloOrfao(t *testing.T) {
	p := buildTurn(agent.ContextPackage{}, "principal", agent.AgentCard{}, "oi", "", 0)
	for _, titulo := range []string{"Regras do projeto", "Índice dos repositórios",
		"Memória do projeto", "Achados já publicados"} {
		if strings.Contains(p.StablePrefix, titulo) {
			t.Fatalf("título órfão %q num pacote vazio", titulo)
		}
	}
	if !strings.HasPrefix(p.StablePrefix, agent.ContratoDoRuntime) {
		t.Fatal("o contrato do runtime não é a primeira coisa do prefixo")
	}
	if !strings.Contains(p.StablePrefix, "Sem ficha") {
		t.Fatal("thread sem ficha precisa DIZER que não tem, para o agente saber o escopo")
	}
}

// Artefato externalizado entra pela REFERÊNCIA: omitir daria a impressão de que
// não existe material nenhum.
func TestArtefatoExternalizadoEntraPelaReferencia(t *testing.T) {
	p := buildTurn(pacoteCheio(), "principal", agent.AgentCard{}, "oi", "", 0)
	if !strings.Contains(p.StablePrefix, "gs://artefatos/dop-api") {
		t.Fatalf("a referência do artefato externalizado sumiu:\n%s", p.StablePrefix)
	}
}

// A intervenção do operador é VOLÁTIL, vem DEPOIS do turno do usuário e tem
// papel próprio — nunca `user` (D3).
func TestIntervencaoDoOperadorTemCanalProprio(t *testing.T) {
	p := buildTurn(agent.ContextPackage{}, "principal", agent.AgentCard{},
		"pergunta", "  pare de mexer no schema  ", 0)
	if len(p.Messages) != 2 {
		t.Fatalf("esperava 2 mensagens, veio %d", len(p.Messages))
	}
	if p.Messages[0].Role != agent.RoleUser || p.Messages[1].Role != agent.RoleOperator {
		t.Fatalf("papéis errados: %+v", p.Messages)
	}
	if p.Messages[1].Text != "pare de mexer no schema" {
		t.Fatalf("a instrução não foi normalizada: %q", p.Messages[1].Text)
	}
	// Nota em branco não vira mensagem vazia — que sairia como uma instrução
	// de operador sem conteúdo, e o modelo teria de adivinhar o que fazer.
	vazio := buildTurn(agent.ContextPackage{}, "principal", agent.AgentCard{}, "oi", "   ", 0)
	if len(vazio.Messages) != 1 {
		t.Fatalf("nota em branco virou mensagem: %+v", vazio.Messages)
	}
}

// Os dois avisos de truncamento falam com PÚBLICOS diferentes, e os dois somem
// quando não houve corte.
func TestAvisosDeTruncamento(t *testing.T) {
	sem := agent.ContextPackage{}
	if s := agent.TruncationNotice(sem); s != "" {
		t.Fatalf("aviso na thread sem truncamento: %q", s)
	}
	if p := buildTurn(sem, "principal", agent.AgentCard{}, "oi", "", 0); strings.Contains(
		p.StablePrefix, "TRUNCADO") {
		t.Fatal("aviso no prefixo sem truncamento")
	}

	com := agent.ContextPackage{Dropped: agent.ContextDropped{Rules: 1, Index: 2, Memories: 3, Findings: 4}}
	naThread := agent.TruncationNotice(com)
	noPrefixo := buildTurn(com, "principal", agent.AgentCard{}, "oi", "", 0).StablePrefix
	if !strings.Contains(naThread, "1 regra(s)") || !strings.Contains(naThread, "4 achado(s)") {
		t.Fatalf("o aviso da thread não conta o que ficou de fora: %q", naThread)
	}
	if !strings.Contains(noPrefixo, "Não conclua sobre o que não está aqui") {
		t.Fatal("o prefixo não instrui o agente sobre o contexto parcial")
	}
	if strings.Contains(noPrefixo, "A resposta abaixo") {
		t.Fatal("o aviso do HUMANO entrou no prefixo do agente: são públicos diferentes")
	}
}

// O schema de saída é FUNÇÃO, e não variável compartilhada: uma variável de
// pacote mutável mudaria o contrato de todos os turnos de todas as contas no dia
// em que um adaptador mexesse nela.
func TestSchemaDeSaidaNaoEhCompartilhado(t *testing.T) {
	a, b := agent.OutputSchema(), agent.OutputSchema()
	a["type"] = "vandalizado"
	if b["type"] != "object" {
		t.Fatal("OutputSchema devolve a MESMA instância: mexer numa mudaria o contrato de todas")
	}
}

func TestTetoDeSaidaTemPadrao(t *testing.T) {
	if p := buildTurn(agent.ContextPackage{}, "k", agent.AgentCard{}, "oi", "", 0); p.MaxOutputTokens != agent.DefaultMaxOutputTokens {
		t.Fatalf("teto de saída zerado não caiu no padrão: %d", p.MaxOutputTokens)
	}
}

// buildTurn é o BuildTurn SEM ferramentas — a forma que este arquivo exercita.
//
// A montagem do prompt e o catálogo de ferramentas são coisas separadas de
// propósito, e os testes seguem a separação: aqui se prova o layout do prefixo,
// em tools_test.go se prova o catálogo, e o encontro dos dois tem teste próprio
// (ver TestFerramentasEntramNaFichaDaThread).
func buildTurn(pkg agent.ContextPackage, threadKey string, card agent.AgentCard,
	text, operatorNote string, maxOutputTokens int) agent.Turn {
	return agent.BuildTurn(pkg, threadKey, card, text, operatorNote, maxOutputTokens, nil)
}
