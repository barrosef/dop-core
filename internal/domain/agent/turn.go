package agent

import (
	"context"
	"strings"
)

// ════════════════════════════════════════════════════════════════════════════
// O LAÇO DO TURNO — conversar com o modelo e INTERPRETAR o que voltou.
//
// A divisão com service.go é deliberada e sobreviveu à travessia do Python:
//
//   - AQUI fica tudo o que não toca os domínios vizinhos: chamar a porta do
//     fornecedor e transformar a resposta estruturada em FATOS do domínio (o que
//     responder, se concluiu, qual achado);
//   - LÁ fica o ciclo com os vizinhos — contexto, roteamento, medição, mensagem,
//     achado, pausa por orçamento.
//
// A consequência prática que vale a separação: a conversa com o modelo é
// testável sem vizinho nenhum, e o ciclo é testável sem fornecedor nenhum. As
// duas metades falham por motivos diferentes — fornecedor fora do ar não é banco
// fora do ar — e juntá-las faria toda falha parecer a mesma.
//
// Sobre "CONCLUIR EXIGE PUBLICAR ACHADO": a spec de conversação §1 diz que a
// thread não morre em silêncio, e `demand.Service.ConcludeThread` já recusa
// concluir sem achado publicado. Aqui a regra aparece uma etapa antes: se o
// modelo marcou `concluded` mas não escreveu título nem resumo, a conclusão é
// RECUSADA — a thread continua ativa e a resposta ganha um aviso. Aceitar a
// conclusão vazia seria deixar a thread morrer em silêncio com um `true` de
// enfeite, e o próximo agente refaria o trabalho.
// ════════════════════════════════════════════════════════════════════════════

// Finding é o achado tal como o modelo o produziu, antes de virar registro.
type Finding struct {
	Title   string
	Payload map[string]any
}

// TurnExecution é o que o turno produziu, em fatos do domínio.
//
// `Reply` é SEMPRE texto para a thread — mesmo quando o modelo devolveu JSON
// estruturado, porque quem lê a thread é gente. `Finding` é nulo enquanto a
// thread não concluiu, e não um achado vazio: achado vazio publicado seria o
// registro durável de nada.
type TurnExecution struct {
	Reply      string
	Concluded  bool
	Finding    *Finding
	Turn       Turn
	ModelReply *Reply
	Warnings   []string
}

// textoDaResposta é a fala que vai para a thread.
//
// Quando há saída estruturada, é o campo `reply`. Quando não há — o fornecedor
// devolveu texto solto porque o schema falhou, por exemplo —, é o texto cru:
// devolver vazio esconderia do humano a única coisa que o modelo produziu.
func textoDaResposta(r *Reply) string {
	if s := strings.TrimSpace(texto(r.Data["reply"])); s != "" {
		return s
	}
	return strings.TrimSpace(r.Text)
}

func texto(v any) string {
	s, _ := v.(string)
	return s
}

func achadoDe(dados map[string]any) *Finding {
	titulo := strings.TrimSpace(texto(dados["finding_title"]))
	resumo := strings.TrimSpace(texto(dados["finding_summary"]))
	if titulo == "" || resumo == "" {
		return nil
	}
	// As evidências chegam como `[]any` da decodificação JSON. Item que não é
	// string é DESCARTADO em vez de virar erro: um achado com uma evidência a
	// menos ainda vale mais do que um turno perdido por causa de um tipo.
	var evidencias []string
	if lista, ok := dados["finding_evidence"].([]any); ok {
		for _, e := range lista {
			if s := strings.TrimSpace(texto(e)); s != "" {
				evidencias = append(evidencias, s)
			}
		}
	}
	if evidencias == nil {
		evidencias = []string{}
	}
	return &Finding{
		Title:   titulo,
		Payload: map[string]any{"summary": resumo, "evidence": evidencias},
	}
}

// executeTurn é UMA volta: manda a conversa, lê a resposta, decide se concluiu.
//
// Não retenta e não faz laço de ferramenta — ferramentas estão fora da porta
// nesta entrega. O que este "laço" faz é a volta completa de UM turno; o próximo
// turno é decisão de quem chamou, e é assim que o orçamento consegue interrompê-lo
// ENTRE uma volta e outra (ADR-0011 §2) em vez de no meio de uma.
func executeTurn(ctx context.Context, p AgentProvider, t Turn,
	model string, effort Effort) (*TurnExecution, error) {

	resposta, err := p.Send(ctx, t, model, effort)
	if err != nil {
		return nil, err
	}
	dados := resposta.Data
	if dados == nil {
		dados = map[string]any{}
	}
	avisos := append([]string(nil), resposta.Warnings...)

	concluiu, _ := dados["concluded"].(bool)
	var achado *Finding
	if concluiu {
		achado = achadoDe(dados)
	}
	if concluiu && achado == nil {
		// Concluir EXIGE achado (spec de conversação §1). Sem ele a conclusão
		// não vale: a thread continua ativa e o agente é cobrado no próximo
		// turno, em vez de sumir do radar com um `true` de enfeite.
		concluiu = false
		avisos = append(avisos,
			"o modelo marcou conclusão sem achado (título e resumo): a conclusão "+
				"foi RECUSADA — concluir exige publicar achado (spec §1)")
	}

	switch resposta.StopReason {
	case StopMaxTokens:
		// Resposta cortada não é resposta concluída. Sem este aviso, um turno
		// truncado pareceria apenas uma resposta curta.
		avisos = append(avisos,
			"a resposta foi CORTADA pelo limite de tokens de saída: o conteúdo "+
				"abaixo está incompleto")
	case StopRefused:
		avisos = append(avisos, "o provedor RECUSOU a solicitação por política própria")
	}

	if !resposta.Capabilities.Has(CapCacheCreationAccounting) {
		// ADR-0012 §1: sem esta capacidade, CacheCreationTokens vem zerado e
		// isso significa "não dá para saber", não "nada foi escrito no cache".
		avisos = append(avisos,
			"o provedor '"+resposta.Provider+"' não reporta criação de cache: o campo "+
				"cache_creation_tokens vem zerado por AUSÊNCIA de informação (D1)")
	}

	return &TurnExecution{
		Reply:      textoDaResposta(resposta),
		Concluded:  concluiu,
		Finding:    achado,
		Turn:       t,
		ModelReply: resposta,
		Warnings:   avisos,
	}, nil
}
