package contract

// Duplos locais do SendGrid e de um servidor SMTP.
//
// ── Por que eles existem, e qual é o limite deles ────────────────────────────
//
// Não temos SendGrid nem relay de e-mail na esteira nem no laptop de quem mexe
// no adaptador — e a API do SendGrid NUNCA é chamada de verdade em teste, porque
// um teste que manda e-mail é um teste que ninguém roda duas vezes. Sem duplo, a
// suíte do Mailer seria um arquivo decorativo, que é a pior coisa que um
// arquivo de teste pode ser.
//
// O RISCO do duplo é o oposto e é pior: um duplo escrito a partir do que o MEU
// adaptador espera não prova nada — ele confirma as minhas suposições. Por isso:
//
//   - as respostas do SendGrid vêm da documentação publicada do `/v3/mail/send`
//     (202 sem corpo, `X-Message-Id` no cabeçalho, erro em `{"errors":[…]}`);
//   - o servidor SMTP fala o protocolo do RFC 5321 na mão, e quem valida que
//     ele é um servidor de verdade não é o duplo: é o `net/smtp` da biblioteca
//     padrão do lado do adaptador, que recusa qualquer desvio de sequência.
//
// O que eles NÃO provam: entregabilidade, reputação de remetente, e o que cada
// fornecedor faz com HTML mal formado. Isso não é verificável sem mandar e-mail
// de verdade, e não é o que esta porta promete.

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── SendGrid ────────────────────────────────────────────────────────────────

// NovoDuploSendGrid sobe um httptest.Server que responde `/v3/mail/send`.
//
// `idsPorTipo` é o mapa que o RUNNER configurou no adaptador; o duplo o inverte
// para dizer, na Caixa, QUE TIPO chegou. Sem essa inversão a suíte não
// conseguiria provar que o aviso `invite` não foi enviado com o template do
// resumo — que é o defeito irmão do template ausente, e o mais desagradável dos
// dois: alguém recebe a mensagem errada.
func NovoDuploSendGrid(t *testing.T, idsPorTipo map[string]string, f Falha, segredo string) (string, *Caixa) {
	t.Helper()
	caixa := &Caixa{}
	tipoPorID := map[string]string{}
	for kind, id := range idsPorTipo {
		tipoPorID[id] = kind
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caixa.Bateu()
		if r.URL.Path != "/v3/mail/send" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch f {
		case FalhaCredencial:
			// O corpo ECOA a chave recebida. É deliberado: é assim que a suíte
			// prova que o adaptador redige em vez de repassar.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			respErro(w, "chave "+r.Header.Get("Authorization")+" não autorizada")
			return
		case FalhaIndisponivel:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			respErro(w, "instabilidade ao processar a chave "+segredo)
			return
		case FalhaConteudo:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			respErro(w, "does not contain a valid address")
			return
		}

		var body struct {
			From             map[string]any `json:"from"`
			TemplateID       string         `json:"template_id"`
			Personalizations []struct {
				To []struct {
					Email string `json:"email"`
				} `json:"to"`
				Data map[string]any `json:"dynamic_template_data"`
			} `json:"personalizations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		to, assunto := "", ""
		if len(body.Personalizations) > 0 {
			if len(body.Personalizations[0].To) > 0 {
				to = body.Personalizations[0].To[0].Email
			}
			assunto, _ = body.Personalizations[0].Data["subject"].(string)
		}
		caixa.Recebeu(SentMail{
			To: to, Kind: tipoPorID[body.TemplateID], Subject: assunto,
			Body: body.TemplateID,
		})
		// 202 sem corpo, com o id no cabeçalho: é o que a documentação publica.
		w.Header().Set("X-Message-Id", "msg-"+body.TemplateID)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, caixa
}

func respErro(w http.ResponseWriter, msg string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"message": msg}},
	})
}

// ── SMTP ────────────────────────────────────────────────────────────────────

// NovoDuploSMTP sobe um servidor SMTP local.
//
// Fala o protocolo do RFC 5321 na mão porque a biblioteca padrão não traz
// servidor, e porque trazer uma dependência de teste para isto contrariaria o
// go.mod deliberadamente enxuto desta casa. São ~60 linhas; o `net/smtp` do
// lado do adaptador é quem confere que elas estão certas.
func NovoDuploSMTP(t *testing.T, f Falha, segredo string) (string, *Caixa) {
	t.Helper()
	caixa := &Caixa{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("não subiu o servidor SMTP de teste: %v", err)
	}
	addr := ln.Addr().String()

	if f == FalhaIndisponivel {
		// Porta fechada: o adaptador não consegue nem conectar. É a falha mais
		// comum de relay interno, e o caminho de erro que menos se exercita.
		_ = ln.Close()
		return addr, caixa
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go atenderSMTP(conn, caixa, f, segredo)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return addr, caixa
}

func atenderSMTP(conn net.Conn, caixa *Caixa, f Falha, segredo string) {
	defer conn.Close()
	caixa.Bateu()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	responder := func(s string) {
		_, _ = w.WriteString(s + "\r\n")
		_ = w.Flush()
	}
	responder("220 dop-teste ESMTP")

	var para string
	for {
		linha, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(linha))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			// Multi-linha, como manda o RFC: o `-` continua, o espaço encerra.
			// AUTH é anunciado sempre; quem decide autenticar é o adaptador.
			responder("250-dop-teste")
			responder("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(cmd, "HELO"):
			responder("250 dop-teste")
		case strings.HasPrefix(cmd, "AUTH"):
			if f == FalhaCredencial {
				// A recusa ECOA o que foi recebido — inclusive a senha, em
				// base64 no argumento do AUTH PLAIN e em claro aqui. É assim
				// que a suíte prova que o adaptador redige.
				responder("535 5.7.8 credencial recusada: " + segredo)
				continue
			}
			responder("235 2.7.0 autenticado")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			responder("250 2.1.0 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			if f == FalhaConteudo {
				// 550 é PERMANENTE: reenviar dá o mesmo resultado. É o que
				// separa KindInvalid de KindUnavailable na garantia 7.
				responder("550 5.1.1 caixa inexistente")
				continue
			}
			para = extrairEndereco(cmd)
			responder("250 2.1.5 ok")
		case cmd == "DATA":
			responder("354 manda o corpo, termina com <CRLF>.<CRLF>")
			corpo, ok := lerCorpoSMTP(r)
			if !ok {
				return
			}
			caixa.Recebeu(SentMail{
				To:      para,
				Kind:    cabecalho(corpo, "X-DOP-Kind"),
				Subject: decodificarAssunto(cabecalho(corpo, "Subject")),
				Body:    corpo,
			})
			responder("250 2.0.0 aceito")
		case cmd == "QUIT":
			responder("221 2.0.0 tchau")
			return
		case cmd == "RSET":
			para = ""
			responder("250 2.0.0 ok")
		default:
			responder("500 5.5.2 comando desconhecido")
		}
	}
}

// lerCorpoSMTP lê até o "." sozinho e desfaz o dot-stuffing do RFC 5321.
func lerCorpoSMTP(r *bufio.Reader) (string, bool) {
	var b strings.Builder
	for {
		linha, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		s := strings.TrimRight(linha, "\r\n")
		if s == "." {
			return b.String(), true
		}
		if strings.HasPrefix(s, "..") {
			s = s[1:]
		}
		b.WriteString(s)
		b.WriteString("\n")
	}
}

func cabecalho(corpo, nome string) string {
	for _, linha := range strings.Split(corpo, "\n") {
		if linha == "" {
			return "" // fim do cabeçalho
		}
		if strings.HasPrefix(strings.ToLower(linha), strings.ToLower(nome)+":") {
			return strings.TrimSpace(linha[len(nome)+1:])
		}
	}
	return ""
}

// decodificarAssunto desfaz o encoded-word do RFC 2047 o suficiente para a
// asserção da suíte. Não é um decodificador completo — é o mínimo para provar
// que o assunto chegou não vazio e com o texto certo.
func decodificarAssunto(s string) string {
	if !strings.HasPrefix(s, "=?") {
		return s
	}
	if i := strings.Index(s, "?Q?"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimSuffix(s, "?=")
		s = strings.ReplaceAll(s, "_", " ")
		return s
	}
	return s
}

func extrairEndereco(cmd string) string {
	i, j := strings.Index(cmd, "<"), strings.LastIndex(cmd, ">")
	if i >= 0 && j > i {
		return strings.ToLower(cmd[i+1 : j])
	}
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(cmd, "RCPT TO:")))
}
