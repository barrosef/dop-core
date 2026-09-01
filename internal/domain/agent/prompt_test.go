package agent_test

import (
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

func fullPackage() agent.ContextPackage {
	return agent.ContextPackage{
		// Ordem da CURADORIA: a regra do projeto vem antes da regra da conta
		// because the more specific wins (knowledge.ResolveRules). Alphabetical
		// inverteria as duas.
		Rules: []string{"uphold the project standard", "open the PR against develop"},
		Index: []agent.ContextArtifact{
			{Name: "dop-core", Body: "the core's map"},
			{Name: "dop-api", ObjectRef: "gs://artefatos/dop-api"},
		},
		Memories: []agent.ContextArtifact{{Name: "october lesson", Body: "do not touch the clock"}},
		Findings: []agent.ContextFinding{{Title: "o bug era o TTL", Summary: "cinco minutos"}},
	}
}

// THE PREFIX IS THE ASSET: two turns of the same thread have to produce
// identical bytes at the top, otherwise the cache discount vanishes — with no
func TestPrefixoEstavelEntreTurnos(t *testing.T) {
	pkg := fullPackage()
	card := agent.AgentCard{Purpose: "investigar", Tools: []string{"grep", "bash"}, BudgetMicros: 500}

	primeiro := buildTurn(pkg, "principal", card, "primeira pergunta", "", 0)
	segundo := buildTurn(pkg, "principal", card, "segunda pergunta, bem diferente",
		"o operador manda parar", 0)

	if primeiro.StablePrefix != segundo.StablePrefix {
		t.Fatalf("THE PREFIX CHANGED BETWEEN TURNS OF THE SAME THREAD — it is ADR-0012 §1's saving "+
			"going away in silence.\nfirst:\n%s\nsecond:\n%s",
			primeiro.StablePrefix, segundo.StablePrefix)
	}
	if primeiro.Fingerprint() != segundo.Fingerprint() {
		t.Fatal("Fingerprint diverged: it is of the prefix, and only of it")
	}
	// E o texto do turn NÃO pode ter escorregado para dentro do prefixo — o
	// erro que o campo separado existe para impedir.
	if strings.Contains(primeiro.StablePrefix, "primeira pergunta") {
		t.Fatal("the turn's text entered the stable prefix")
	}
	if strings.Contains(segundo.StablePrefix, "o operador manda parar") {
		t.Fatal("the operator's intervention entered the stable prefix: it is VOLATILE, and " +
			"rewriting the top of the prompt on every intervention is what ADR-0012 §1 avoids")
	}
}

// Montar o mesmo turn duas vezes tem de dar exatamente os mesmos bytes: sem
// clock, no volatile id, no collection iterated out of order.
func TestMontagemDeterministica(t *testing.T) {
	pkg := fullPackage()
	card := agent.AgentCard{Purpose: "investigar", Tools: []string{"zsh", "grep", "bash", "curl"}}
	base := buildTurn(pkg, "principal", card, "hi", "", 0)
	for i := 0; i < 50; i++ {
		if outro := buildTurn(pkg, "principal", card, "hi", "", 0); outro.StablePrefix != base.StablePrefix {
			t.Fatalf("non-deterministic prefix on attempt %d", i)
		}
	}
	// The caller's brief must not be touched as a side effect: it is the thread's
	// data, and reordering it here would change the prefix's bytes on the next
	// turn without anybody having asked.
	if card.Tools[0] != "zsh" {
		t.Fatalf("BuildTurn reordenou o slice do chamador: %v", card.Tools)
	}
	// And what the brief ANNOUNCES are the DECLARED tools, not the granted ones:
	// none of these four exists in the runtime's catalogue, so the prefix does not
	// pode prometer nenhuma. Anunciar uma ferramenta inexistente faz o agente
	// plan on top of it and promise the human it will use it.
	if strings.Contains(base.StablePrefix, "Granted tools") {
		t.Fatalf("the brief announced a tool the runtime does not declare:\n%s", base.StablePrefix)
	}
}

// The brief announces what was DECLARED, and the declaration goes in the turn's
// own field — not interpolated into the prefix's text, because a schema described
// schema que nenhum fornecedor valida.
func TestToolsEnterTheThreadBrief(t *testing.T) {
	card := agent.AgentCard{Purpose: "implementar", Tools: []string{agent.ToolRunCommand}}
	specs, unknown := agent.ToolCatalog(card.Tools)
	if len(specs) != 1 || len(unknown) != 0 {
		t.Fatalf("the catalogue returned %d spec(s) and %d unknown", len(specs), len(unknown))
	}

	turn := agent.BuildTurn(agent.ContextPackage{}, "principal", card, "hi", "", 0, specs)
	if len(turn.Tools) != 1 || turn.Tools[0].Name != agent.ToolRunCommand {
		t.Fatalf("the tools did not reach the turn: %+v", turn.Tools)
	}
	if !strings.Contains(turn.StablePrefix, "Granted tools: "+agent.ToolRunCommand) {
		t.Fatalf("the brief did not announce the declared tool:\n%s", turn.StablePrefix)
	}
	// The prefix stays the PREFIX: the declaration travels in the field, and the
	// schema must not have leaked into the text.
	if strings.Contains(turn.StablePrefix, "additionalProperties") {
		t.Fatal("the tool schema was interpolated into the prefix: it belongs to the Tools field, " +
			"and it is the provider that has to validate it")
	}
	// Determinism, which is prompt.go's layer 3 holding here too.
	for i := 0; i < 20; i++ {
		outro := agent.BuildTurn(agent.ContextPackage{}, "principal", card, "hi", "", 0, specs)
		if outro.StablePrefix != turn.StablePrefix {
			t.Fatalf("the prefix with tools is not deterministic (attempt %d)", i)
		}
	}
}

// The core's order is PRIORITY, not a suggestion: sorting here would buy
// stability at the price of undoing the curation (ADR-0009 §3).
func TestTheCurationOrderIsPreserved(t *testing.T) {
	p := buildTurn(fullPackage(), "principal", agent.AgentCard{}, "hi", "", 0)
	iSpecific := strings.Index(p.StablePrefix, "uphold the project standard")
	iGeneric := strings.Index(p.StablePrefix, "open the PR against develop")
	if iSpecific < 0 || iGeneric < 0 {
		t.Fatal("the rules did not enter the prefix")
	}
	if iSpecific > iGeneric {
		t.Fatal("the rules were REORDERED: the core's order is its priority")
	}
}

// An empty block does not become an orphan heading — and the layout stays
// contrato → ficha → contexto.
func TestAnEmptyPackageProducesNoOrphanHeading(t *testing.T) {
	p := buildTurn(agent.ContextPackage{}, "principal", agent.AgentCard{}, "hi", "", 0)
	for _, title := range []string{"Project rules", "Repository index",
		"Project memory", "Findings already published"} {
		if strings.Contains(p.StablePrefix, title) {
			t.Fatalf("orphan heading %q in an empty package", title)
		}
	}
	if !strings.HasPrefix(p.StablePrefix, agent.RuntimeContract) {
		t.Fatal("the runtime contract is not the first thing in the prefix")
	}
	if !strings.Contains(p.StablePrefix, "No brief") {
		t.Fatal("a thread with no brief has to SAY it has none, so the agent knows the scope")
	}
}

// An externalized artifact enters through its REFERENCE: omitting it would give
// the impression there is no material at all.
func TestArtefatoExternalizadoEntraPelaReferencia(t *testing.T) {
	p := buildTurn(fullPackage(), "principal", agent.AgentCard{}, "hi", "", 0)
	if !strings.Contains(p.StablePrefix, "gs://artefatos/dop-api") {
		t.Fatalf("the externalized artifact's reference disappeared:\n%s", p.StablePrefix)
	}
}

// The operator's intervention is VOLATILE, it comes AFTER the user's turn and it
// has a role of its own — never `user` (D3).
func TestIntervencaoDoOperadorTemCanalProprio(t *testing.T) {
	p := buildTurn(agent.ContextPackage{}, "principal", agent.AgentCard{},
		"pergunta", "  pare de mexer no schema  ", 0)
	if len(p.Messages) != 2 {
		t.Fatalf("esperava 2 mensagens, veio %d", len(p.Messages))
	}
	if p.Messages[0].Role != agent.RoleUser || p.Messages[1].Role != agent.RoleOperator {
		t.Fatalf("wrong roles: %+v", p.Messages)
	}
	if p.Messages[1].Text != "pare de mexer no schema" {
		t.Fatalf("the instruction was not normalized: %q", p.Messages[1].Text)
	}
	// A blank note does not become an empty message — which would go out as an
	// operator instruction with no content, and the model would have to guess.
	vazio := buildTurn(agent.ContextPackage{}, "principal", agent.AgentCard{}, "hi", "   ", 0)
	if len(vazio.Messages) != 1 {
		t.Fatalf("nota em branco virou mensagem: %+v", vazio.Messages)
	}
}

// The two truncation warnings talk to different AUDIENCES, and both disappear
// when there was no cut.
func TestTruncationWarnings(t *testing.T) {
	sem := agent.ContextPackage{}
	if s := agent.TruncationNotice(sem); s != "" {
		t.Fatalf("aviso na thread sem truncamento: %q", s)
	}
	if p := buildTurn(sem, "principal", agent.AgentCard{}, "hi", "", 0); strings.Contains(
		p.StablePrefix, "TRUNCADO") {
		t.Fatal("aviso no prefixo sem truncamento")
	}

	com := agent.ContextPackage{Dropped: agent.ContextDropped{Rules: 1, Index: 2, Memories: 3, Findings: 4}}
	inThread := agent.TruncationNotice(com)
	inPrefix := buildTurn(com, "principal", agent.AgentCard{}, "hi", "", 0).StablePrefix
	if !strings.Contains(inThread, "1 rule(s)") || !strings.Contains(inThread, "4 finding(s)") {
		t.Fatalf("the thread warning does not say what was left out: %q", inThread)
	}
	if !strings.Contains(inPrefix, "Do not conclude about what is not here") {
		t.Fatal("the prefix does not instruct the agent about the partial context")
	}
	if strings.Contains(inPrefix, "The reply below") {
		t.Fatal("the HUMAN's warning entered the agent's prefix: they are different audiences")
	}
}

// The output schema is a FUNCTION, not a shared variable: a mutable package
// variable would change the contract of every turn of every account the day
// em que um adaptador mexesse nela.
func TestSchemaDeSaidaNaoEhCompartilhado(t *testing.T) {
	a, b := agent.OutputSchema(), agent.OutputSchema()
	a["type"] = "vandalizado"
	if b["type"] != "object" {
		t.Fatal("OutputSchema returns the SAME instance: touching one would change the contract of all")
	}
}

func TestTetoDeSaidaTemPadrao(t *testing.T) {
	if p := buildTurn(agent.ContextPackage{}, "k", agent.AgentCard{}, "hi", "", 0); p.MaxOutputTokens != agent.DefaultMaxOutputTokens {
		t.Fatalf("a zero output ceiling did not fall back to the default: %d", p.MaxOutputTokens)
	}
}

// buildTurn is BuildTurn WITHOUT tools — the shape this file exercises.
//
// Prompt assembly and the tool catalogue are separate things on purpose, and the
// tests follow the separation: here the prefix layout is proven, in tools_test.go
// the catalogue is, and the meeting of the two has a test of its own
// (ver TestToolsEnterTheThreadBrief).
func buildTurn(pkg agent.ContextPackage, threadKey string, card agent.AgentCard,
	text, operatorNote string, maxOutputTokens int) agent.Turn {
	return agent.BuildTurn(pkg, threadKey, card, text, operatorNote, maxOutputTokens, nil)
}
