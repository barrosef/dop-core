// Package gitprovider implementa a porta delivery.GitProvider sobre as APIs do
// GitHub e do GitLab.
//
// Padrão desta casa, pelos mesmos motivos dos adaptadores de secretstore e
// sandbox: `net/http` puro, sem SDK de fornecedor. O SDK traria o vocabulário do
// provedor de volta para dentro do processo (tipos `github.PullRequest`,
// `gitlab.MergeRequest`) e a primeira vez que alguém os passasse adiante a porta
// teria vazado. Fora isso, os dois SDKs oficiais arrastam árvores de dependência
// grandes para usar seis chamadas HTTP.
//
// ── Onde mora o token ────────────────────────────────────────────────────────
//
// O token do provedor é CREDENCIAL DE RECURSO (ADR-0013): vive no cofre, atrás
// de ports.SecretStore, e quem o resolve é o composition root. Este pacote NÃO
// importa ports.SecretStore, não recebe cofre por parâmetro e não sabe que
// cofre existe — recebe o valor pronto no construtor. A razão é a mesma da
// ADR-0001: um adaptador de git que soubesse consultar o cofre passaria a ter
// duas responsabilidades e uma delas seria impossível de testar sem infra.
//
// E o token, uma vez dentro, não sai: ele é capturado num CLOSURE de
// autorização em vez de guardado num campo. Campo de struct — mesmo não
// exportado — é impresso por `%+v`, porque o fmt lê campos não exportados por
// reflexão e não consegue chamar o String() deles. Closure imprime como
// endereço. É a diferença entre "prometemos não logar o token" e "não há token
// para logar".
package gitprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// DefaultTimeout cobre a chamada única. O rebase, que é assíncrono nos dois
// provedores (garantia 9 da porta), tem prazo próprio — ver RebaseTimeout.
const DefaultTimeout = 30 * time.Second

// DefaultRebaseTimeout é quanto o adaptador espera o provedor TERMINAR de
// reaplicar antes de desistir. Precisa ser generoso: no GitLab o rebase é uma
// tarefa de fila do Sidekiq, e num repositório grande em hora cheia ela demora.
// Desistir cedo devolve KindUnavailable — o que é honesto — mas transforma
// fila lenta em falha, então o default é alto de propósito.
const DefaultRebaseTimeout = 3 * time.Minute

// defaultPoll é o intervalo entre duas leituras do estado do rebase.
const defaultPoll = 2 * time.Second

// httpDoer permite injetar o cliente na suíte de contrato sem expor o
// transporte para o composition root — mesma escolha do adaptador de
// secretstore, onde `Client` existe "para teste".
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// client é a plumbing compartilhada entre GitHub e GitLab.
//
// `authorize` é o closure que carrega o token (ver o cabeçalho do pacote).
// `redact` é a rede de segurança: se um dia alguém montar uma URL com o token
// dentro, ou o provedor devolver o token no corpo do erro, a mensagem sai
// redigida em vez de vazar. Cinto E suspensório, porque o custo de errar aqui é
// alguém abrir PR e mergear como o dono da conta.
type client struct {
	base      string
	http      httpDoer
	authorize func(*http.Request)
	redact    func(string) string
	who       string // "GitHub" | "GitLab", para a mensagem de erro
}

// String impede que a instância inteira revele qualquer coisa quando alguém
// escreve `log.Info("...", "provider", p)`. Receptor por VALOR de propósito:
// com receptor por ponteiro, `%+v` de um valor não chamaria este método.
func (c client) String() string { return "gitprovider.client{" + c.who + "}" }

func newClient(base, who string, doer httpDoer, timeout time.Duration, authorize func(*http.Request), token string) *client {
	if doer == nil {
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		// Sem CA customizada, ao contrário dos adaptadores de k8s: aqui o
		// certificado é público (api.github.com) ou de uma instalação
		// self-hosted que já precisa estar no bundle do sistema. Inventar um
		// pool aqui só criaria um lugar a mais para o TLS quebrar em silêncio.
		doer = &http.Client{Timeout: timeout}
	}
	return &client{base: strings.TrimRight(base, "/"), http: doer, authorize: authorize,
		redact: redactor(token), who: who}
}

// redactor devolve a função que apaga o token de qualquer texto que vá subir.
// Token vazio devolve identidade — sem isso, `strings.ReplaceAll(s, "", x)`
// espalharia o marcador entre TODOS os caracteres da mensagem.
func redactor(token string) func(string) string {
	if token == "" {
		return func(s string) string { return s }
	}
	return func(s string) string { return strings.ReplaceAll(s, token, "***") }
}

// do executa a chamada e devolve status + corpo. Nunca devolve o corpo junto de
// um erro de transporte: erro de transporte não tem corpo, e devolver os dois
// convida quem chama a inspecionar bytes que não existem.
func (c *client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, errs.Wrap(errs.KindInternal, err, "pedido ilegível para o %s", c.who)
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, nil, errs.Wrap(errs.KindInternal, err, "requisição inválida para o %s", c.who)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		// A mensagem do net/http carrega a URL; a URL não carrega o token
		// (autorização vai em cabeçalho), mas o redator passa por cima assim
		// mesmo — é barato e cobre o dia em que alguém mudar isso.
		return 0, nil, errs.New(errs.KindUnavailable, "falha ao falar com o %s: %s",
			c.who, c.redact(err.Error()))
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, errs.New(errs.KindUnavailable,
			"resposta truncada do %s: %s", c.who, c.redact(err.Error()))
	}
	return resp.StatusCode, out, nil
}

// fail traduz o status HTTP para o Kind da porta (garantia 11).
//
// A tradução é a MESMA nos dois provedores porque a garantia é da porta, não do
// fornecedor. O que muda é de onde sai a mensagem legível — e isso cada
// adaptador resolve com o seu `explain`.
func (c *client) fail(code int, msg, what string) error {
	msg = strings.TrimSpace(c.redact(msg))
	switch code {
	case http.StatusUnauthorized:
		// Sem detalhe do provedor de propósito: 401 é sempre a mesma decisão
		// para quem opera — a credencial não serve — e o corpo do 401 é o
		// lugar mais provável de um provedor ecoar o que recebeu.
		return errs.New(errs.KindUnauthorized,
			"o %s recusou a credencial deste recurso (HTTP 401)", c.who)
	case http.StatusForbidden:
		return errs.Permission("o %s negou %s: %s", c.who, what, msg)
	case http.StatusNotFound:
		// Ver garantia 11: o GitHub responde 404 para repositório privado que o
		// token não enxerga, e não há como saber daqui se sumiu ou se é
		// invisível. A mensagem diz as duas possibilidades para quem for
		// investigar.
		return errs.NotFound("%s no %s — inexistente ou fora do alcance desta credencial", what, c.who)
	}
	if code == http.StatusTooManyRequests || code >= 500 {
		return errs.New(errs.KindUnavailable, "o %s não atendeu %s (HTTP %d): %s", c.who, what, code, msg)
	}
	return errs.Internal("o %s recusou %s (HTTP %d): %s", c.who, what, code, msg)
}

// decode desserializa com mensagem útil. Resposta ilegível é KindUnavailable, e
// não Internal: quase sempre é um proxy ou portal de autenticação respondendo
// HTML no lugar do provedor — problema do caminho, não do nosso código.
func (c *client) decode(body []byte, v any, what string) error {
	if err := json.Unmarshal(body, v); err != nil {
		return errs.New(errs.KindUnavailable,
			"resposta ilegível do %s em %s: %s", c.who, what, c.redact(err.Error()))
	}
	return nil
}

// esperar dorme respeitando o contexto. Devolve KindUnavailable no
// cancelamento, que é a garantia 9: "não sei" nunca vira "não conflitou".
func esperar(ctx context.Context, d time.Duration, what string) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return errs.New(errs.KindUnavailable, "%s: %v", what, ctx.Err())
	case <-t.C:
		return nil
	}
}

// conferirAtor é a garantia 13, escrita uma vez para os dois adaptadores.
//
// Recusar é melhor que ignorar: uma instância fala por UMA credencial, e um
// pedido em nome de outro ator abriria o PR assinado por quem quer que seja o
// dono do token fiado. A ADR-0003 exige que quem conduziu assine — silenciar
// aqui transformaria essa exigência numa mentira que ninguém veria.
func conferirAtor(instancia, pedido, op string) error {
	if pedido == "" || instancia == "" || pedido == instancia {
		return nil
	}
	return errs.Permission(
		"esta conexão com o provedor fala pelo ator %q; %s foi pedido em nome de %q "+
			"(ADR-0003: quem conduziu assina) — monte a conexão com a credencial do ator certo",
		instancia, op, pedido)
}

// textoDe achata o que os provedores devolvem em `message`: o GitHub manda
// string, o GitLab manda ora string, ora lista de strings, ora objeto de campo
// para lista de erros. Achatar aqui evita três decodificadores diferentes.
func textoDe(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		partes := make([]string, 0, len(t))
		for _, e := range t {
			if s := textoDe(e); s != "" {
				partes = append(partes, s)
			}
		}
		return strings.Join(partes, "; ")
	case map[string]any:
		partes := make([]string, 0, len(t))
		for k, e := range t {
			if s := textoDe(e); s != "" {
				partes = append(partes, k+": "+s)
			}
		}
		// Ordem estável: mapa em Go itera aleatoriamente, e mensagem de erro
		// que muda de ordem entre execuções é impossível de casar em teste e
		// irritante de ler em log.
		sortStrings(partes)
		return strings.Join(partes, "; ")
	default:
		return fmt.Sprint(t)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// jsonUnmarshalTolerante existe só para o caminho de ERRO: quando o provedor
// (ou um proxy no meio) devolve algo que não é o JSON esperado, queremos a
// mensagem que der para extrair, não um segundo erro por cima do primeiro.
func jsonUnmarshalTolerante(b []byte, v any) error { return json.Unmarshal(b, v) }

// instante converte a data ISO-8601 dos provedores. Data ilegível vira o
// instante ZERO em vez de erro: a garantia da porta é sobre identidade e
// conflito, e derrubar a abertura de um PR por causa de um fuso mal formatado
// seria trocar um problema cosmético por um bloqueio de entrega.
func instante(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func instantePtr(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	return instante(*s)
}

// unixDe devolve 0 — e não o Unix do instante zero, que é -62135596800 — para
// data ausente. É a diferença entre "não mergeou ainda" e "mergeou no ano 1".
func unixDe(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
