// Package agentprovider implementa a porta agent.AgentProvider sobre as APIs da
// Anthropic e da OpenAI.
//
// ── Sobre SDK ───────────────────────────────────────────────────────────────
//
// `net/http` puro, sem SDK de fornecedor — o mesmo padrão dos adaptadores de
// gitprovider, secretstore, objectstore e sandbox, e pelos mesmos dois motivos.
// O SDK traria o vocabulário do fornecedor de volta para dentro do processo
// (`anthropic.Message`, `openai.ChatCompletion`) e a primeira vez que alguém o
// passasse adiante a porta teria vazado; e o go.mod deste repositório é
// deliberadamente enxuto, enquanto os SDKs oficiais arrastam árvores de
// dependência grandes para o que aqui é UMA rota HTTP por fornecedor.
//
// Isto é uma DIVERGÊNCIA CONSCIENTE em relação à implementação Python que este
// pacote porta: lá o adaptador Anthropic usava o SDK oficial e só o da OpenAI
// falava HTTP na mão. A regra da casa aqui é a outra, e a suíte de contrato é o
// que garante que a troca de tática não mudou o contrato: ela roda igual nos
// dois adaptadores.
//
// ── Onde mora a credencial ──────────────────────────────────────────────────
//
// A chave do fornecedor é CREDENCIAL DE RECURSO (ADR-0013): vive no cofre, atrás
// de `ports.SecretStore`, e quem a resolve é o composition root
// (internal/app/agentproviders.go). Este pacote NÃO importa `ports.SecretStore`,
// não recebe cofre por parâmetro e não sabe que cofre existe — recebe o valor
// pronto no construtor. É o ponto inteiro da ADR-0023: a credencial é lida e
// usada no MESMO processo, e não atravessa fronteira de rede nenhuma.
//
// E a chave, uma vez dentro, não sai: ela é capturada num CLOSURE de autorização
// em vez de guardada num campo. Campo de struct — mesmo não exportado — é
// impresso por `%+v`, porque o fmt lê campos não exportados por reflexão e não
// consegue chamar o String() deles. Closure imprime como endereço. É a diferença
// entre "prometemos não logar a chave" e "não há chave para logar".
//
// ── Sobre prazo e conexões ──────────────────────────────────────────────────
//
// A ADR-0023 anotou o preço da mudança: "o núcleo passa a fazer chamadas externas
// LONGAS". Uma geração com effort alto leva minutos, e um prazo de 30s — que é o
// certo para uma chamada de API de git — transformaria todo turno caro em falha.
// Daí `DefaultTimeout` generoso. O orçamento de CONEXÕES é resolvido reusando o
// `http.DefaultTransport` (é o que um `&http.Client{Timeout: …}` sem Transport
// faz): o pool é do processo, e não um por adaptador construído por requisição —
// caso contrário, uma conta por requisição viraria um pool por requisição.
package agentprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/agent"
)

// DefaultTimeout é o prazo de UMA geração. Ver o cabeçalho: generoso de
// propósito, porque a alternativa é transformar trabalho caro em falha.
const DefaultTimeout = 10 * time.Minute

// httpDoer permite injetar o cliente sem expor o transporte para o composition
// root — mesma escolha dos adaptadores de gitprovider e secretstore.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// client é a plumbing compartilhada entre os adaptadores.
//
// `authorize` é o closure que carrega a chave (ver o cabeçalho do pacote).
// `redact` é a rede de segurança: se um dia alguém montar uma URL com a chave
// dentro, ou o fornecedor ecoar um prefixo dela no corpo do erro, a mensagem sai
// redigida em vez de vazar. Cinto E suspensório, porque o custo de errar aqui é
// alguém gastar na conta de outra pessoa.
type client struct {
	base      string
	http      httpDoer
	authorize func(*http.Request)
	redact    func(string) string
	who       string // "anthropic" | "openai", para a mensagem de erro
	// temCredencial é BOOLEANO, e não a chave: o adaptador precisa saber se
	// tem com que falar, e guardar a chave para responder essa pergunta seria
	// desfazer o closure.
	temCredencial bool
}

// String impede que a instância revele qualquer coisa quando alguém escreve
// `log.Info("…", "provider", p)`. Receptor por VALOR de propósito: com receptor
// por ponteiro, `%+v` de um valor não chamaria este método.
func (c client) String() string { return "agentprovider.client{" + c.who + "}" }

func newClient(base, who string, doer httpDoer, timeout time.Duration,
	authorize func(*http.Request), token string) *client {
	if doer == nil {
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		// Sem Transport próprio: o pool de conexões é o do processo. Ver o
		// cabeçalho — um adaptador é construído POR REQUISIÇÃO (o provedor é
		// recurso de conta), e um Transport por adaptador seria um pool novo
		// por turno.
		doer = &http.Client{Timeout: timeout}
	}
	return &client{
		base: strings.TrimRight(base, "/"), http: doer, authorize: authorize,
		redact: redactor(token), who: who, temCredencial: token != "",
	}
}

// redactor devolve a função que apaga a chave de qualquer texto que vá subir.
// Chave vazia devolve identidade — sem isso, `strings.ReplaceAll(s, "", x)`
// espalharia o marcador entre TODOS os caracteres da mensagem.
func redactor(token string) func(string) string {
	if token == "" {
		return func(s string) string { return s }
	}
	return func(s string) string { return strings.ReplaceAll(s, token, "***") }
}

// post executa a chamada e devolve status + corpo.
//
// Erro de transporte NUNCA vem com corpo: erro de transporte não tem corpo, e
// devolver os dois convida quem chama a inspecionar bytes que não existem.
func (c *client) post(ctx context.Context, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path,
		strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, agent.Unavailability(c.who, agent.ReasonUnreachable,
			"requisição inválida: "+c.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		// Prazo esgotado e cancelamento chegam aqui como erro de transporte, e
		// os dois são INALCANÇÁVEL do ponto de vista do domínio: em nenhum dos
		// dois casos sabemos o que o fornecedor fez. "Não sei" nunca pode virar
		// "não aconteceu".
		return 0, nil, agent.Unavailability(c.who, agent.ReasonUnreachable,
			c.redact(err.Error()))
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, agent.Unavailability(c.who, agent.ReasonUnreachable,
			"resposta truncada: "+c.redact(err.Error()))
	}
	return resp.StatusCode, out, nil
}

// falha traduz o status HTTP para a razão da porta (D6).
//
// A tradução é a MESMA nos dois fornecedores porque a garantia é da porta, não do
// fornecedor. E o CORPO da resposta não entra na mensagem que sobe: ele costuma
// ecoar cabeçalhos e, em alguns erros, um prefixo da chave. Ele vai para o
// detalhe, que é log — e ainda passa pelo redator antes.
func (c *client) falha(status int, corpo []byte) *agent.Unavailable {
	var razao agent.UnavailableReason
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		razao = agent.ReasonRejectedCredential
	case status == http.StatusNotFound:
		// 404 aqui é quase sempre nome de modelo que não existe no catálogo do
		// fornecedor: a rota é fixa e única.
		razao = agent.ReasonUnknownModel
	default:
		razao = agent.ReasonProviderError
	}
	return agent.Unavailability(c.who, razao,
		"HTTP "+itoa(status)+": "+c.redact(strings.TrimSpace(string(corpo))))
}

// itoa evita importar strconv só para isto no caminho de erro.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// decodificarTexto tenta ler a saída estruturada do modelo.
//
// Falha de decodificação NÃO é erro do turno: o fornecedor pode ter devolvido
// texto solto porque o schema não pegou, e nesse caso a fala crua ainda vale para
// o humano que lê a thread (ver `textoDaResposta` no domínio). Devolver erro aqui
// jogaria fora a única coisa que o modelo produziu — e os tokens já pagos por ela.
func decodificarTexto(texto string) map[string]any {
	t := strings.TrimSpace(texto)
	if !strings.HasPrefix(t, "{") {
		return nil
	}
	var dados map[string]any
	if err := json.Unmarshal([]byte(t), &dados); err != nil {
		return nil
	}
	return dados
}
