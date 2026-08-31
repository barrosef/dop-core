package contract

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ════════════════════════════════════════════════════════════════════════════
// OS DUPLOS DOS FORNECEDORES DE AGENTE.
//
// Eles ficam do outro lado do FIO: um `httptest.Server` que fala o protocolo do
// fornecedor. O adaptador sob teste é o REAL — o `net/http` dele, os cabeçalhos
// dele, a decodificação dele. Um mock do adaptador passaria mesmo com o adaptador
// errado; um duplo do fornecedor não.
//
// O QUE ESTES DUPLOS PROVAM: que os dois adaptadores concordam com a MESMA
// leitura da documentação de cada fornecedor. O QUE ELES NÃO PROVAM: que a
// leitura estava certa. As duas apostas deste código — o texto exato do 400
// quando o modelo não aceita mensagem de sistema no meio, e o formato da
// contabilidade de cache de cada fornecedor — estão marcadas como tal nos
// comentários, porque é onde quem for conferir contra a documentação vai olhar.
// Não há caminho automatizado contra a API real, e de propósito: um teste que
// gasta tokens por execução é um teste que alguém desliga.
//
// A tradução de `RespostaProgramada` para o formato do fornecedor é o coração do
// duplo: a suíte fala em parcelas DISJUNTAS e cada duplo escreve no dialeto dele
// — inclusive o dialeto INCLUSIVO da OpenAI, que é a armadilha do D2.
// ════════════════════════════════════════════════════════════════════════════

// fakeAgente é o estado compartilhado pelos dois duplos.
type fakeAgente struct {
	mu             sync.Mutex
	srv            *httptest.Server
	token          string
	resposta       RespostaProgramada
	ultimoCorpo    []byte
	chamadas       int
	modeloSemCanal string
}

func (f *fakeAgente) URL() string { return f.srv.URL }

func (f *fakeAgente) Programar(t *testing.T, r RespostaProgramada) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resposta = r
}

func (f *fakeAgente) UltimoCorpo() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.ultimoCorpo...)
}

func (f *fakeAgente) Chamadas() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chamadas
}

// registrar guarda o que chegou e devolve a resposta programada.
func (f *fakeAgente) registrar(corpo []byte) RespostaProgramada {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chamadas++
	f.ultimoCorpo = corpo
	return f.resposta
}

// escrever emite o corpo cru de um erro programado, se houver.
func erroProgramado(w http.ResponseWriter, r RespostaProgramada) bool {
	if r.Status < 400 {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.Status)
	_, _ = io.WriteString(w, r.Corpo)
	return true
}

// ── duplo da Anthropic ──────────────────────────────────────────────────────

// NewAnthropicFake sobe o duplo da API da Anthropic.
//
// `modeloSemCanal` é o modelo que recusa `role:"system"` no meio de `messages` —
// é o que exercita o RECUO do D3, que é a única parte do adaptador que só aparece
// depois de um erro do fornecedor.
func NewAnthropicFake(t *testing.T, token, modeloSemCanal string) *fakeAgente {
	t.Helper()
	f := &fakeAgente{token: token, modeloSemCanal: modeloSemCanal}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		corpo, _ := io.ReadAll(r.Body)
		prog := f.registrar(corpo)

		// A credencial vai em `x-api-key` neste fornecedor. Chave errada é 401,
		// e o corpo ECOA o que chegou — que é o comportamento real e é
		// justamente onde uma credencial vaza para o log.
		if r.Header.Get("x-api-key") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid x-api-key: `+
				r.Header.Get("x-api-key")+`"}}`)
			return
		}
		if erroProgramado(w, prog) {
			return
		}

		var pedido struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(corpo, &pedido)
		if f.modeloSemCanal != "" && pedido.Model == f.modeloSemCanal {
			for _, m := range pedido.Messages {
				if m.Role == "system" {
					// O 400 que a Anthropic devolve nos modelos sem canal de
					// operador. O texto é aposta — ver o cabeçalho.
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":`+
						`"invalid_request_error","message":"messages: Unexpected role `+
						`\"system\". Allowed roles are user and assistant"}}`)
					return
				}
			}
		}

		// ── D8, o dialeto DESTE fornecedor ─────────────────────────────────
		// A chamada é um bloco DENTRO de `content`, ao lado do texto, e
		// `input` é OBJETO. O duplo do outro fornecedor escreve a mesma
		// `RespostaProgramada` com os argumentos em STRING — é essa diferença
		// que permite à garantia 17 perguntar se o adaptador normalizou.
		conteudo := []any{map[string]any{"type": "text", "text": prog.Texto}}
		for _, c := range prog.Ferramentas {
			bloco := map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name}
			if prog.ArgumentoIlegivel {
				// Aqui o caso degenerado é `input` que não é objeto. Não há
				// como mandar "string que não é JSON" — `input` já vem
				// decodificado —, então o duplo manda uma string onde o
				// adaptador espera um objeto. Do ponto de vista da porta é o
				// MESMO fato: o fornecedor mandou algo que não vira mapa.
				bloco["input"] = "isto-nao-e-um-objeto"
			} else {
				entrada := c.Input
				if entrada == nil {
					entrada = map[string]any{}
				}
				bloco["input"] = entrada
			}
			conteudo = append(conteudo, bloco)
		}

		// As parcelas são DISJUNTAS neste fornecedor, e o duplo as escreve como
		// tal: `input_tokens` EXCLUI o que veio do cache.
		resp := map[string]any{
			"model":       pedido.Model,
			"stop_reason": prog.ParadaNativa,
			"content":     conteudo,
			"usage": map[string]any{
				"input_tokens":                prog.Uso.InputTokens,
				"output_tokens":               prog.Uso.OutputTokens,
				"cache_read_input_tokens":     prog.Uso.CacheReadTokens,
				"cache_creation_input_tokens": prog.Uso.CacheCreationTokens,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// ── duplo da OpenAI ─────────────────────────────────────────────────────────

// NewOpenAIFake sobe o duplo da API da OpenAI.
func NewOpenAIFake(t *testing.T, token string) *fakeAgente {
	t.Helper()
	f := &fakeAgente{token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		corpo, _ := io.ReadAll(r.Body)
		prog := f.registrar(corpo)

		autorizacao := r.Header.Get("Authorization")
		if !strings.HasPrefix(autorizacao, "Bearer ") ||
			strings.TrimPrefix(autorizacao, "Bearer ") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"incorrect api key: `+autorizacao+`"}}`)
			return
		}
		if erroProgramado(w, prog) {
			return
		}

		var pedido struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(corpo, &pedido)

		// ── A ARMADILHA DO D2, escrita de propósito ─────────────────────────
		// Neste fornecedor `prompt_tokens` INCLUI os cacheados, e
		// `cached_tokens` é subconjunto dele. O duplo reescreve as parcelas
		// disjuntas da suíte nesse formato inclusivo — é o que permite à
		// garantia 6 perguntar se o adaptador subtraiu.
		//
		// E a criação de cache simplesmente NÃO EXISTE aqui: não há campo onde
		// escrevê-la. É a divergência D1, e o duplo a torna concreta em vez de
		// só documentada.
		prompt := prog.Uso.InputTokens + prog.Uso.CacheReadTokens

		// ── D8, o dialeto DESTE fornecedor ─────────────────────────────────
		// A chamada vem em `tool_calls`, FORA de `content`, e os argumentos são
		// uma STRING que ainda precisa de parse. É a armadilha do D8 escrita de
		// propósito: um adaptador que não decodifique entrega `Input` nulo, e a
		// garantia 17 vê a diferença.
		mensagem := map[string]any{"content": prog.Texto}
		if len(prog.Ferramentas) > 0 {
			chamadas := make([]any, 0, len(prog.Ferramentas))
			for _, c := range prog.Ferramentas {
				args := "{}"
				if prog.ArgumentoIlegivel {
					// O caso real: o modelo emitiu uma string que não fecha.
					args = `{"alvo": "corta`
				} else if c.Input != nil {
					b, _ := json.Marshal(c.Input)
					args = string(b)
				}
				chamadas = append(chamadas, map[string]any{
					"id": c.ID, "type": "function",
					"function": map[string]any{"name": c.Name, "arguments": args},
				})
			}
			mensagem["tool_calls"] = chamadas
		}

		resp := map[string]any{
			"model": pedido.Model,
			"choices": []any{map[string]any{
				"message":       mensagem,
				"finish_reason": prog.ParadaNativa,
			}},
			"usage": map[string]any{
				"prompt_tokens":         prompt,
				"completion_tokens":     prog.Uso.OutputTokens,
				"prompt_tokens_details": map[string]any{"cached_tokens": prog.Uso.CacheReadTokens},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}
