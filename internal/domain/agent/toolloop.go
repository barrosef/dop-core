package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ════════════════════════════════════════════════════════════════════════════
// O LAÇO DO TURNO COM FERRAMENTAS — a ponte entre o agente e o sandbox.
//
// A divisão de trabalho deste domínio agora tem TRÊS peças, e a fronteira de
// cada uma é o que ela pode tocar:
//
//   - turn.go     — interpreta UMA resposta do modelo. Não toca vizinho nenhum;
//   - toolloop.go — o laço: manda, executa ferramenta, mede, decide se continua.
//     Toca DUAS portas estreitas (Routing, para medir, e Sandbox, para agir) e
//     nenhuma outra;
//   - service.go  — o ciclo completo: contexto, thread, mensagem, achado.
//
// O laço vive em arquivo próprio porque ele é a peça com a regra mais cara do
// pacote — a que decide quanto dinheiro um turno pode gastar — e essa regra não
// pode ficar diluída no meio de conversão de tipos.
//
// ── AS CINCO REGRAS DO LAÇO ─────────────────────────────────────────────────
//
// 1. TETO DE VOLTAS EXPLÍCITO. Laço de ferramenta sem teto é conta de token sem
//    teto, e a ADR-0011 existe para impedir isso. O custo de uma volta não é
//    constante: cada volta REENVIA a conversa inteira mais os resultados
//    acumulados, então a volta N custa mais que a N-1. Um agente em ciclo — roda
//    o teste, lê o erro, "conserta", roda de novo, mesmo erro — gasta muito e
//    converge nada, e ele não sabe que está em ciclo.
//
// 2. A PARADA É LEGÍVEL. "Terminei" e "bati o teto" são fatos diferentes e saem
//    diferentes (ver LoopStop): o primeiro é uma resposta, o segundo é uma
//    decisão para um humano. Um laço que devolve os dois como sucesso faz o
//    cockpit mostrar trabalho incompleto com cara de trabalho pronto.
//
// 3. TODA VOLTA É MEDIDA. `RecordUsage` acontece a CADA volta, não no fim. Duas
//    razões: o total do turno é a soma das voltas (registrar só a última
//    subestimaria o gasto em um fator igual ao número de voltas), e é o registro
//    que devolve o estado do orçamento — sem medir a cada volta, o teto só seria
//    consultado quando o dinheiro já tivesse sido gasto.
//
// 4. ORÇAMENTO ESTOURADO PARA O LAÇO, NÃO MATA O TURNO (ADR-0011 §2). O que já
//    rodou é entregue inteiro: a resposta vai para a thread, o achado é
//    publicado, o consumo está registrado. O que não acontece é a PRÓXIMA volta.
//
// 5. FALHA DE FERRAMENTA É RESULTADO; FALHA DE INFRAESTRUTURA SOBE. O modelo
//    precisa ver que o comando saiu com código 1 para corrigir — entregar isso
//    como erro do turno tiraria dele a única informação que resolve o problema.
//    Já um sandbox que caiu não é coisa que o modelo conserte: gastar mais
//    tokens pedindo para ele tentar de novo é queimar dinheiro contra uma parede.
// ════════════════════════════════════════════════════════════════════════════

// LoopStop é POR QUE o laço parou. Vocabulário fechado: quem lê o resultado
// decide a partir daqui, e um vocabulário aberto viraria comparação de string
// espalhada por três consumidores.
type LoopStop string

const (
	// LoopFinished: o modelo terminou de falar. É a única parada que significa
	// "a resposta abaixo está completa".
	LoopFinished LoopStop = "finished"
	// LoopMaxRounds: bateu o teto de voltas. O trabalho parou no meio.
	LoopMaxRounds LoopStop = "max_rounds"
	// LoopBudget: o orçamento estourou entre uma volta e outra (ADR-0011 §2).
	LoopBudget LoopStop = "budget_exceeded"
	// LoopNoSandbox: o modelo pediu ferramenta e não há substrato ligado.
	LoopNoSandbox LoopStop = "sandbox_unavailable"
)

// Concluded diz se a parada admite conclusão de thread.
//
// Só `LoopFinished`. Um agente que bateu o teto no meio de um laço pode ter
// marcado `concluded` na última fala — e aceitar isso publicaria um achado
// escrito antes de o trabalho terminar, que é pior do que não ter achado nenhum:
// ele entra na memória do projeto e no contexto dos irmãos como se fosse verdade.
func (s LoopStop) Concluded() bool { return s == LoopFinished }

// DefaultMaxToolRounds é o teto de voltas de ferramenta por turno.
//
// OITO, e a escolha tem conta por trás. O custo do laço é superlinear: a volta N
// reenvia o prefixo (cacheado, ~0,1×) mais TODA a conversa acumulada até ali
// (entrada nova, 1×), de modo que o gasto cresce com o quadrado do número de
// voltas na parte volátil. Com oito voltas, o pior caso de um turno é da ordem
// de oito chamadas ao modelo — auditável, e um número que cabe na cabeça de quem
// lê a fatura.
//
// Por que não menos: trabalho real de agente com ferramenta gasta de duas a
// cinco voltas no caso comum (olhar, agir, verificar, corrigir, verificar de
// novo). Um teto de três cortaria o caso NORMAL, e um teto que corta o caso
// normal é um teto que alguém sobe até virar enfeite.
//
// Por que não mais: acima disso, o padrão que aparece não é convergência, é
// ciclo — e ciclo não melhora com mais voltas, só fica mais caro. O teto não é
// para o agente competente; é para o agente que não percebeu que travou.
const DefaultMaxToolRounds = 8

// resultadoDoLaco é o que o laço produziu, em fatos do domínio.
type resultadoDoLaco struct {
	// exec é a interpretação da ÚLTIMA resposta do modelo.
	exec *TurnExecution
	// usoTotal é a soma das voltas — o que o turno de fato consumiu.
	usoTotal Usage
	// custoTotal é a soma dos custos de cada volta, calculada com o preço do
	// modelo que ATENDEU cada uma.
	custoTotal Micros
	// precoConhecido é falso quando ALGUMA volta rodou num modelo sem tabela de
	// preço. Falso derruba o total inteiro de propósito: um custo somado com
	// uma parcela faltando é mais perigoso que um custo ausente, porque parece
	// completo (ADR-0011 §2).
	precoConhecido bool
	moeda          string
	rodadas        int
	chamadas       int
	parada         LoopStop
	conta          Accounting
	avisos         []string
}

// laco é a bagagem do laço: as portas que ele pode tocar e os limites que valem.
type laco struct {
	provider  AgentProvider
	sandbox   Sandbox
	routing   Routing
	info      ProviderInfo
	demandID  string
	threadID  string
	modelo    string
	esforco   Effort
	maxVoltas int
	// chaveUso deriva a chave de idempotência da medição de CADA volta.
	chaveUso func(rodada int) string
	// permitidas são as ferramentas declaradas neste turno. Vazia = laço de uma
	// volta, que é exatamente o comportamento de antes de ferramentas existirem.
	permitidas []ToolSpec
}

// rodar executa o laço até o modelo terminar, o teto bater ou o orçamento
// estourar.
func (l laco) rodar(ctx context.Context, turno Turn) (*resultadoDoLaco, error) {
	res := &resultadoDoLaco{precoConhecido: true, parada: LoopFinished}

	for {
		res.rodadas++

		exec, err := executeTurn(ctx, l.provider, turno, l.modelo, l.esforco)
		if err != nil {
			// Falha do FORNECEDOR na volta 1 é falha do turno. Na volta N já
			// houve consumo registrado, e ele continua registrado — o erro sobe
			// mesmo assim, porque uma resposta pela metade de um laço que
			// quebrou no meio é pior que a falha honesta.
			return nil, err
		}
		res.exec = exec
		res.avisos = acrescentarAvisos(res.avisos, exec.Warnings...)

		// ── regra 3: TODA volta é medida ────────────────────────────────────
		conta, err := l.medir(ctx, res, exec.ModelReply)
		if err != nil {
			return nil, err
		}
		res.conta = conta

		chamadas := exec.ModelReply.ToolCalls
		if len(chamadas) == 0 {
			// O modelo falou e não pediu nada: acabou. Note que `StopToolUse`
			// SEM chamada nenhuma cai aqui de propósito — parar de trabalhar
			// esperando ferramentas que o fornecedor não mandou seria travar
			// por causa de uma incoerência dele.
			res.parada = LoopFinished
			return res, nil
		}
		res.chamadas += len(chamadas)

		// ── regra 5, primeira metade: sem substrato não há ação ─────────────
		if l.sandbox == nil {
			res.parada = LoopNoSandbox
			res.avisos = append(res.avisos,
				"o modelo pediu ferramenta e esta instalação não tem substrato de execução "+
					"ligado: o turno parou aqui SEM executar nada")
			return res, nil
		}

		// ── regra 4: orçamento estourado para ANTES da próxima volta ────────
		if conta.BudgetExceeded {
			res.parada = LoopBudget
			res.avisos = append(res.avisos,
				"o orçamento estourou e o laço de ferramenta PAROU antes da próxima volta: "+
					"as chamadas pedidas na última resposta não foram executadas (ADR-0011 §2)")
			return res, nil
		}

		// ── regra 1: o teto ─────────────────────────────────────────────────
		if res.rodadas >= l.maxVoltas {
			res.parada = LoopMaxRounds
			res.avisos = append(res.avisos, fmt.Sprintf(
				"o laço de ferramenta bateu o teto de %d volta(s) e PAROU: o trabalho não "+
					"terminou, e as %d chamada(s) da última resposta não foram executadas. "+
					"Isto é decisão para um humano — continuar custa mais tokens (ADR-0011)",
				l.maxVoltas, len(chamadas)))
			return res, nil
		}

		resultados, err := l.executar(ctx, chamadas)
		if err != nil {
			return nil, err
		}

		// A história cresce: a fala do modelo COM as chamadas, e os resultados
		// logo depois. As duas mensagens são obrigatórias e nesta ordem — os
		// dois fornecedores recusam um resultado que não segue a chamada que o
		// justifica (D11).
		turno.Messages = append(turno.Messages,
			Message{Role: RoleAssistant, Text: exec.ModelReply.Text, ToolCalls: chamadas},
			Message{Role: RoleToolResult, ToolResults: resultados},
		)
	}
}

// medir registra o consumo DESTA volta e acumula o total do turno.
//
// A chave de idempotência é derivada da volta (`…:usage:1`, `…:usage:2`), e é o
// que faz repetir a requisição inteira repetir zero efeitos. Uma repetição que
// precise de MAIS voltas que a original grava linhas novas para as voltas novas
// — e está certo: aqueles tokens foram gastos de verdade.
func (l laco) medir(ctx context.Context, res *resultadoDoLaco, r *Reply) (Accounting, error) {
	modelo := r.Model
	if modelo == "" {
		modelo = l.modelo
	}
	res.usoTotal.InputTokens += r.Usage.InputTokens
	res.usoTotal.OutputTokens += r.Usage.OutputTokens
	res.usoTotal.CacheReadTokens += r.Usage.CacheReadTokens
	res.usoTotal.CacheCreationTokens += r.Usage.CacheCreationTokens

	preco, conhecido := l.info.PriceFor(modelo)
	var custo Micros
	if conhecido {
		custo = preco.CostMicros(r.Usage)
		res.custoTotal += custo
		if res.moeda == "" {
			res.moeda = preco.Currency
		}
	} else {
		res.precoConhecido = false
	}

	return l.routing.RecordUsage(ctx, Consumption{
		DemandID:            l.demandID,
		ThreadID:            l.threadID,
		Model:               modelo,
		InputTokens:         r.Usage.InputTokens,
		OutputTokens:        r.Usage.OutputTokens,
		CacheReadTokens:     r.Usage.CacheReadTokens,
		CacheCreationTokens: r.Usage.CacheCreationTokens,
		CostMicros:          custo,
		Currency:            preco.Currency,
	}, l.chaveUso(res.rodadas))
}

// executar roda as ferramentas pedidas, NA ORDEM (D10).
//
// Sequencial, e não em paralelo: as chamadas de uma volta compartilham o
// workspace da demanda, e `git checkout` rodando junto com `npm test` no mesmo
// diretório é uma corrida que o modelo não pediu e não consegue depurar. O
// paralelismo que vale é o do fornecedor pedir várias por volta — isso já
// economiza a volta, que é onde está o custo.
func (l laco) executar(ctx context.Context, chamadas []ToolCall) ([]ToolResult, error) {
	out := make([]ToolResult, 0, len(chamadas))
	for _, call := range chamadas {
		cmd, motivo := comandoDe(call, l.permitidas)
		if motivo != "" {
			out = append(out, resultadoDeErro(call, motivo))
			continue
		}
		saida, err := l.sandbox.RunCommand(ctx, l.demandID, cmd)
		if err != nil {
			if falhaDeInfra(err) {
				// Regra 5, segunda metade: isto não é assunto do modelo.
				return nil, err
			}
			// Recusa SOBRE A CHAMADA (nome, argumento, permissão): o modelo
			// consegue corrigir, então ela volta como resultado.
			out = append(out, resultadoDeErro(call,
				"a ferramenta recusou a chamada: "+err.Error()))
			continue
		}
		out = append(out, resultadoDe(call, saida))
	}
	return out, nil
}

// falhaDeInfra separa "o substrato caiu" de "a chamada estava errada".
//
// A régua é o `errs.Kind`, e a divisão é por QUEM CONSEGUE CONSERTAR:
//
//   - INDISPONÍVEL e INTERNO são nossos ou do cluster. Nenhuma quantidade de
//     tokens gastos pelo modelo resolve, e insistir é queimar dinheiro contra
//     uma parede — sobe e mata o turno;
//   - INVÁLIDO, NÃO ENCONTRADO, SEM PERMISSÃO e PRECONDIÇÃO são sobre a chamada
//     ou sobre o estado que o pedido pressupôs. O modelo lê, entende e tenta
//     outra coisa — viram resultado de erro.
//
// PRECONDIÇÃO é a fronteira discutível: "sandbox suspenso" cai aqui e o modelo
// não retoma sandbox. Fica como resultado mesmo assim, porque a alternativa —
// matar o turno — apagaria a resposta que o agente já tinha escrito, e a mensagem
// diz exatamente o que houve para quem ler a thread.
func falhaDeInfra(err error) bool {
	switch errs.KindOf(err) {
	case errs.KindUnavailable, errs.KindInternal:
		return true
	}
	return false
}

// acrescentarAvisos junta avisos SEM repetir.
//
// Existe porque os avisos de adaptador são por CHAMADA e o laço faz várias: o
// aviso de effort rebaixado e o de "este provedor não reporta criação de cache"
// sairiam idênticos oito vezes num turno de oito voltas. Repetição não informa
// nada e afoga o aviso que exige decisão — que é o problema que a caixa de
// atenção já tem sem ajuda.
//
// Comparação linear de propósito: são poucos avisos, e um mapa aqui trocaria a
// ORDEM deles, que é a ordem em que apareceram.
func acrescentarAvisos(dst []string, novos ...string) []string {
	for _, n := range novos {
		repetido := false
		for _, j := range dst {
			if j == n {
				repetido = true
				break
			}
		}
		if !repetido {
			dst = append(dst, n)
		}
	}
	return dst
}

// ── leitura humana da parada ────────────────────────────────────────────────

// LoopNotice é a frase que a thread mostra quando o laço NÃO terminou sozinho.
//
// Vazia quando terminou: a caixa de atenção só serve se o que entra nela exigir
// decisão, e ruído por turno a esvazia de sentido — mesma regra do aviso de
// truncamento de contexto.
func LoopNotice(parada LoopStop, rodadas, teto int) string {
	switch parada {
	case LoopMaxRounds:
		return "⚠️ O agente parou no teto de " + strconv.Itoa(teto) + " volta(s) de ferramenta " +
			"(ADR-0011). O trabalho NÃO terminou: a resposta acima é o estado em que ele " +
			"parou. Decida se vale continuar — cada volta custa uma chamada de modelo."
	case LoopBudget:
		return "⚠️ O orçamento estourou no meio do laço de ferramenta e ele parou na volta " +
			strconv.Itoa(rodadas) + ". O que já rodou está entregue; a próxima volta não sai."
	case LoopNoSandbox:
		return "⚠️ O agente pediu para executar um comando e não há substrato de execução " +
			"ligado nesta instalação. Ele respondeu sem executar nada."
	}
	return ""
}

// avisoDeFerramentasDesconhecidas redige o aviso de nome concedido que não
// existe no catálogo.
func avisoDeFerramentasDesconhecidas(nomes []string) string {
	if len(nomes) == 0 {
		return ""
	}
	return "a ficha desta thread concede ferramenta(s) que não existem no catálogo do " +
		"runtime e foram IGNORADAS: " + strings.Join(nomes, ", ")
}
