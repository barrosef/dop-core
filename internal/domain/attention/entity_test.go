package attention_test

import (
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
)

var agora = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func item(k attention.Kind, idadeHoras int) attention.Item {
	return attention.Item{Kind: k, OpenedAt: agora.Add(-time.Duration(idadeHoras) * time.Hour)}
}

// A regra da spec: "produção da fila de merge > pergunta exploratória".
// Este é o teste que a codifica.
func TestConflitoDeMergeVemAntesDePerguntaExploratoria(t *testing.T) {
	conflito := item(attention.KindMergeConflict, 0)  // três minutos atrás
	pergunta := item(attention.KindThreadBlocked, 72) // três dias atrás

	if conflito.Priority(agora) >= pergunta.Priority(agora) {
		t.Fatalf("conflito de produção recém-aberto (%d) deveria vir antes de pergunta de três dias (%d)",
			conflito.Priority(agora), pergunta.Priority(agora))
	}
}

// A idade desempata DENTRO da faixa, e é isso que impede a caixa de esquecer
// item antigo — sem deixá-lo atravessar faixas.
func TestIdadeDesempataDentroDaMesmaFaixa(t *testing.T) {
	velho := item(attention.KindPRReview, 48)
	novo := item(attention.KindPRReview, 1)

	if velho.Priority(agora) >= novo.Priority(agora) {
		t.Fatal("entre itens do mesmo tipo, o mais velho tem que vir primeiro")
	}
}

// Se a idade atravessasse faixas, uma pergunta esquecida passaria na frente de
// um conflito de produção. É a inversão que a spec proíbe.
func TestIdadeNaoAtravessaFaixa(t *testing.T) {
	perguntaAntiquissima := item(attention.KindThreadBlocked, 24*365)
	conflitoAgora := item(attention.KindMergeConflict, 0)

	if perguntaAntiquissima.Priority(agora) <= conflitoAgora.Priority(agora) {
		t.Fatal("idade não pode atravessar faixa de impacto")
	}
}

func TestTipoDesconhecidoVaiParaOFimDaFila(t *testing.T) {
	desconhecido := item(attention.Kind("inventado"), 0)
	pior := item(attention.KindThreadBlocked, 0)

	if desconhecido.Priority(agora) <= pior.Priority(agora) {
		t.Fatal("tipo desconhecido não pode empurrar item classificado para baixo")
	}
}

func TestTodoTipoTemImpactoDeclarado(t *testing.T) {
	for _, k := range attention.Kinds() {
		if attention.ImpactOf(k) >= 999 {
			t.Errorf("tipo %q sem impacto na tabela — cairia no fim da fila em silêncio", k)
		}
	}
}

// Relógio andando para trás não pode virar prioridade máxima.
func TestRelogioParaTrasNaoViraUrgencia(t *testing.T) {
	futuro := attention.Item{Kind: attention.KindPRReview, OpenedAt: agora.Add(time.Hour)}
	presente := attention.Item{Kind: attention.KindPRReview, OpenedAt: agora}

	if futuro.Priority(agora) < presente.Priority(agora) {
		t.Fatal("item com OpenedAt no futuro não pode ser mais urgente que um de agora")
	}
}
