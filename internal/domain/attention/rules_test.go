package attention_test

import (
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
)

func ev(tipo string, payload map[string]any) attention.Event {
	return attention.Event{
		ID: "ev-1", AccountID: "acc-1", Aggregate: "demand", AggregateID: "dem-1",
		Type: tipo, OccurredAt: agora, Payload: payload,
	}
}

func TestThreadBloqueadaAbreItemQueLevaAThread(t *testing.T) {
	d := attention.Apply(ev(attention.EvThreadBlocked, map[string]any{
		"thread_id": "th-9", "question": "Posso apagar a coluna?",
	}))
	if d.Open == nil {
		t.Fatal("thread bloqueada deveria abrir item")
	}
	if d.Open.TargetKind != "thread" || d.Open.TargetID != "th-9" {
		t.Errorf("o clique tem que levar à thread; veio %s/%s", d.Open.TargetKind, d.Open.TargetID)
	}
	if d.Open.DemandID != "dem-1" {
		t.Error("o item precisa da demanda para a caixa poder agrupar")
	}
}

// O risco R-1 em forma de teste: progresso não é atenção.
func TestEtapaAvancandoNaoEnchaACaixa(t *testing.T) {
	d := attention.Apply(ev(attention.EvStageAdvanced, map[string]any{
		"stage_key": "implementacao", "status": "running",
	}))
	if d.Open != nil {
		t.Fatal("etapa avançando é progresso, e progresso é cockpit — não caixa")
	}
}

func TestEtapaBloqueadaEmPortaoHumanoAbreItem(t *testing.T) {
	d := attention.Apply(ev(attention.EvStageAdvanced, map[string]any{
		"stage_key": "spec", "status": "blocked", "gate": "human",
	}))
	if d.Open == nil || d.Open.Kind != attention.KindGatePending {
		t.Fatal("etapa parada em portão humano exige decisão — tem que abrir item")
	}
}

func TestEtapaBloqueadaSemPortaoNaoAbreItem(t *testing.T) {
	d := attention.Apply(ev(attention.EvStageAdvanced, map[string]any{
		"stage_key": "teste", "status": "blocked", "gate": "none",
	}))
	if d.Open != nil {
		t.Fatal("bloqueio sem portão não é decisão humana pendente")
	}
}

func TestThreadDestravadaFechaOItem(t *testing.T) {
	d := attention.Apply(ev(attention.EvThreadResumed, map[string]any{"thread_id": "th-9"}))
	if d.Close == nil {
		t.Fatal("thread destravada tem que fechar o item")
	}
	if d.Close.TargetID != "th-9" || d.Close.Kind != attention.KindThreadBlocked {
		t.Error("o fechamento é por ALVO: quem destrava não sabe o id do item")
	}
}

// Mudança de estado que não resolve conflito não pode fechar o item — senão a
// caixa mentiria dizendo que está resolvido.
func TestEstadoIntermediarioNaoFechaConflito(t *testing.T) {
	if d := attention.Apply(ev(attention.EvMergeState, map[string]any{
		"state": "rebasing", "pull_request_id": "pr-1",
	})); d.Close != nil {
		t.Fatal("rebasing não resolveu conflito nenhum")
	}
	if d := attention.Apply(ev(attention.EvMergeState, map[string]any{
		"state": "merged", "pull_request_id": "pr-1",
	})); d.Close == nil {
		t.Fatal("merge concluído resolve o conflito")
	}
}

func TestEventoSemAlvoNaoViraItem(t *testing.T) {
	d := attention.Apply(ev(attention.EvThreadBlocked, map[string]any{}))
	if d.Open != nil {
		t.Fatal("item sem alvo não dá para clicar — pior que item ausente")
	}
}

func TestEventoIrrelevanteEhIgnorado(t *testing.T) {
	d := attention.Apply(ev("dop.hierarchy.workspace.created", map[string]any{}))
	if d.Open != nil || d.Close != nil {
		t.Fatal("workspace criado não exige decisão de ninguém")
	}
}

// Os assuntos assinados têm que cobrir TODO evento que a regra sabe traduzir —
// senão o item simplesmente nunca chega, e ninguém percebe.
func TestAssuntosCobremTodosOsEventosTratados(t *testing.T) {
	tratados := []string{
		attention.EvThreadBlocked, attention.EvStageAdvanced,
		attention.EvPullRequestOpened, attention.EvMergeConflict,
		attention.EvDirectiveProposed, attention.EvBudgetExceeded,
		attention.EvThreadResumed, attention.EvThreadConcluded,
		attention.EvGateDecided, attention.EvDirectiveDecide,
		attention.EvMergeState, attention.EvBudgetSet,
	}
	for _, tipo := range tratados {
		if !cobertoPorAlgumAssunto(tipo, attention.Subjects()) {
			t.Errorf("evento %q é tratado pela regra mas nenhum assunto assinado o traz", tipo)
		}
	}
}

// casa a semântica do NATS que a porta documenta: ">" casa a cauda.
func cobertoPorAlgumAssunto(tipo string, assuntos []string) bool {
	for _, a := range assuntos {
		if len(a) > 2 && a[len(a)-2:] == ".>" && len(tipo) >= len(a)-1 &&
			tipo[:len(a)-1] == a[:len(a)-1] {
			return true
		}
		if a == tipo {
			return true
		}
	}
	return false
}

var _ = time.Now
