package contract_test

// A suíte de contrato do AgentProvider contra os DOIS duplos locais.
//
//	go test ./test/contract/ -run AgentProvider -v
//
// Roda sempre, sem infra e sem chave de API — e é a única forma de a suíte
// existir de verdade: nem a esteira nem o laptop de quem mexe no adaptador têm
// credencial de Anthropic ou OpenAI, e uma suíte que só roda com credencial de
// produção é uma suíte que não roda. Ver o cabeçalho dos duplos para o limite do
// que eles provam.
//
// NÃO existe aqui um caminho que chame os fornecedores de VERDADE, e isso é
// escolha, não pendência: uma suíte que gasta tokens ao rodar acaba não rodando,
// e um teste que cobra por execução é um teste que alguém desliga. O que os
// duplos não conseguem provar — o texto exato do 400 sem canal de operador e o
// formato da contabilidade de cache de cada fornecedor — está anotado como
// APOSTA no adaptador e nos duplos, no lugar onde quem for conferir vai olhar.
// A execução com `-tags=integration` roda esta mesma suíte: os arquivos sem tag
// compilam nos dois modos, e é de propósito que o resultado seja o mesmo.

import (
	"net/http/httptest"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/agentprovider"
	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// chaveFalsa é a SENTINELA da garantia 10. Precisa ser uma sequência improvável
// e reconhecível: a suíte varre toda saída atrás dela.
const chaveFalsa = "sk-SENTINELA-NAO-PODE-APARECER-EM-LUGAR-NENHUM-0001"

// enderecoMorto devolve uma URL que ninguém atende — é o caso da razão
// UNREACHABLE. Um servidor criado e fechado dá a garantia de que a porta está
// livre; um número escolhido à mão daria um teste que falha na máquina de quem
// tiver algo escutando lá.
func enderecoMorto(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(nil)
	url := s.URL
	s.Close()
	return url
}

func TestAgentProviderContractAnthropic(t *testing.T) {
	// O Sonnet 5 é o modelo que recusa `role:"system"` no meio de `messages`
	// (D3) — é ele que exercita o recuo.
	const semCanal = "claude-sonnet-5"
	f := contract.NewAnthropicFake(t, chaveFalsa, semCanal)

	contract.AgentProviderSuite(t, "anthropic", func(t *testing.T) contract.AgentProviderEnv {
		novo := func(base, chave string) agent.AgentProvider {
			return agentprovider.NewAnthropic(agentprovider.AnthropicConfig{
				APIBase: base + "/v1",
				APIKey:  chave,
			})
		}
		return contract.AgentProviderEnv{
			Conectar:       func(t *testing.T) agent.AgentProvider { return novo(f.URL(), chaveFalsa) },
			SemCredencial:  func(t *testing.T) agent.AgentProvider { return novo(f.URL(), "") },
			Inalcancavel:   func(t *testing.T) agent.AgentProvider { return novo(enderecoMorto(t), chaveFalsa) },
			TokenSentinela: chaveFalsa,
			Programar:      f.Programar,
			UltimoCorpo:    f.UltimoCorpo,
			Chamadas:       f.Chamadas,
			Paradas: map[string]agent.StopReason{
				"end_turn":                      agent.StopCompleted,
				"stop_sequence":                 agent.StopCompleted,
				"max_tokens":                    agent.StopMaxTokens,
				"model_context_window_exceeded": agent.StopMaxTokens,
				"refusal":                       agent.StopRefused,
				"tool_use":                      agent.StopToolUse,
				"pause_turn":                    agent.StopToolUse,
			},
			// Os cinco níveis existem aqui: nada é rebaixado (D4).
			EffortAplicado: map[agent.Effort]agent.Effort{
				agent.EffortLow:    agent.EffortLow,
				agent.EffortMedium: agent.EffortMedium,
				agent.EffortHigh:   agent.EffortHigh,
				agent.EffortXHigh:  agent.EffortXHigh,
				agent.EffortMax:    agent.EffortMax,
			},
			ModeloSemCanalDeOperador: semCanal,
			ModeloComPreco:           "claude-opus-5",
			// Cache EXPLÍCITO: o breakpoint é um marcador no corpo, e a suíte
			// confere que ele existe e que fica no FIM do prefixo (D1).
			MarcadorDeCache: "cache_control",
			// D7: o schema vai NO TOPO do objeto da ferramenta, sem casca.
			MarcadorDeFerramenta: "input_schema",
		}
	})
}

func TestAgentProviderContractOpenAI(t *testing.T) {
	f := contract.NewOpenAIFake(t, chaveFalsa)

	contract.AgentProviderSuite(t, "openai", func(t *testing.T) contract.AgentProviderEnv {
		novo := func(base, chave string) agent.AgentProvider {
			return agentprovider.NewOpenAI(agentprovider.OpenAIConfig{
				APIBase: base + "/v1",
				APIKey:  chave,
			})
		}
		return contract.AgentProviderEnv{
			Conectar:       func(t *testing.T) agent.AgentProvider { return novo(f.URL(), chaveFalsa) },
			SemCredencial:  func(t *testing.T) agent.AgentProvider { return novo(f.URL(), "") },
			Inalcancavel:   func(t *testing.T) agent.AgentProvider { return novo(enderecoMorto(t), chaveFalsa) },
			TokenSentinela: chaveFalsa,
			Programar:      f.Programar,
			UltimoCorpo:    f.UltimoCorpo,
			Chamadas:       f.Chamadas,
			Paradas: map[string]agent.StopReason{
				"stop":           agent.StopCompleted,
				"length":         agent.StopMaxTokens,
				"tool_calls":     agent.StopToolUse,
				"content_filter": agent.StopRefused,
			},
			// Só três níveis: `xhigh` e `max` são REBAIXADOS para `high`, e a
			// suíte exige o aviso junto (D4).
			EffortAplicado: map[agent.Effort]agent.Effort{
				agent.EffortLow:    agent.EffortLow,
				agent.EffortMedium: agent.EffortMedium,
				agent.EffortHigh:   agent.EffortHigh,
				agent.EffortXHigh:  agent.EffortHigh,
				agent.EffortMax:    agent.EffortHigh,
			},
			// Sem modelo sem canal de operador: `role:"developer"` é aceito em
			// qualquer posição neste fornecedor.
			ModeloSemCanalDeOperador: "",
			// Sem tabela de preço, e de propósito: preço inventado alimentaria
			// o orçamento da ADR-0011 com ficção convincente. O subteste 11
			// INVERTE aqui e exige que `PriceFor` diga que não sabe.
			ModeloComPreco: "",
			// Cache AUTOMÁTICO: não há breakpoint para marcar (D1). Vazio aqui
			// e `CapExplicitPrefixCache` ausente na ficha são a MESMA afirmação,
			// e a suíte exige que as duas concordem.
			MarcadorDeCache: "",
			// D7: aqui a ferramenta vem embrulhada em `function`, e o schema
			// chama `parameters`. Mesmo fato, outro nome — é o que a porta
			// normaliza.
			MarcadorDeFerramenta: "parameters",
		}
	})
}
