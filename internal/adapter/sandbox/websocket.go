// Cliente WebSocket MÍNIMO, para o `pods/exec` do Kubernetes.
//
// ── Por que escrito à mão ───────────────────────────────────────────────────
//
// Pela mesma regra que já vale nos outros adaptadores deste repositório: nada de
// SDK, e o go.mod é deliberadamente enxuto. Aqui a conta é ainda mais favorável
// que de costume — do RFC 6455 este arquivo precisa de uma fração: o aperto de
// mão, a leitura de quadros do servidor (que nunca vêm mascarados), o pong e o
// fechamento. Não há envio de dados: o exec desta porta é COMANDO, não sessão,
// e `stdin` vai desligado. Uma biblioteca de WebSocket completa traria
// compressão, extensões e um modelo de concorrência que este uso não tem.
//
// ── Por que WebSocket e não SPDY ────────────────────────────────────────────
//
// O `pods/exec` aceita os dois. O SPDY do Kubernetes é um protocolo próprio,
// morto fora dali, e exigiria implementar multiplexação de streams inteira. O
// WebSocket com o subprotocolo `v4.channel.k8s.io` entrega a mesma coisa com um
// byte de canal na frente de cada mensagem: 1 = stdout, 2 = stderr, 3 = status
// final (um `metav1.Status` em JSON, e é ALI que vem o código de saída).
//
// ── A pegadinha do `kubectl proxy` ──────────────────────────────────────────
//
// O `kubectl proxy` recusa por padrão os caminhos de exec e attach — o default
// de `--reject-paths` inclui `^/api/.*/pods/.*/exec`. Fora do cluster, a suíte
// de contrato precisa de `kubectl proxy --port=8001 --reject-paths='^$'`, e sem
// isso o aperto de mão volta 403 antes de qualquer WebSocket existir. Está
// anotado aqui e no cabeçalho de test/contract/sandbox_k8s_test.go porque é o
// tipo de detalhe que custa uma tarde quando não está escrito em lugar nenhum.
package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// wsGUID é a constante do RFC 6455 usada na confirmação do aperto de mão.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes que este cliente conhece. O resto é ignorado.
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// wsConn é uma conexão WebSocket já negociada.
//
// Não é segura para uso concorrente e não precisa ser: cada exec abre a sua,
// lê até o fim e fecha.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// wsDial faz o aperto de mão e devolve a conexão pronta para ler.
//
// O contexto governa a conexão INTEIRA, e não só o aperto de mão: a goroutine
// abaixo fecha o socket quando ele termina. É assim que o prazo do comando e o
// cancelamento do chamador chegam a uma leitura que, de outro modo, ficaria
// pendurada esperando bytes de um processo que não vai falar mais.
func wsDial(ctx context.Context, rawURL string, header http.Header,
	subprotocolo string, tlsCfg *tls.Config) (*wsConn, *http.Response, error) {

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, errs.Wrap(errs.KindInternal, err, "endereço inválido para exec")
	}

	host := u.Host
	var conn net.Conn
	d := &net.Dialer{}
	switch u.Scheme {
	case "https":
		if !strings.Contains(host, ":") {
			host += ":443"
		}
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", host)
	default:
		if !strings.Contains(host, ":") {
			host += ":80"
		}
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "falha ao abrir conexão de exec")
	}

	// O fechamento por contexto é o que faz o prazo valer para a LEITURA, não
	// só para a conexão. Sem isso, um comando que pendura sem escrever nada
	// deixaria a goroutine do chamador presa até o fim do processo.
	fim := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-fim:
		}
	}()

	chave := make([]byte, 16)
	if _, err := rand.Read(chave); err != nil {
		close(fim)
		_ = conn.Close()
		return nil, nil, errs.Wrap(errs.KindInternal, err, "falha ao sortear a chave do WebSocket")
	}
	chaveB64 := base64.StdEncoding.EncodeToString(chave)

	var req strings.Builder
	caminho := u.RequestURI()
	req.WriteString("GET " + caminho + " HTTP/1.1\r\n")
	req.WriteString("Host: " + u.Host + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	req.WriteString("Sec-WebSocket-Key: " + chaveB64 + "\r\n")
	if subprotocolo != "" {
		req.WriteString("Sec-WebSocket-Protocol: " + subprotocolo + "\r\n")
	}
	for k, vs := range header {
		for _, v := range vs {
			req.WriteString(k + ": " + v + "\r\n")
		}
	}
	req.WriteString("\r\n")

	if _, err := io.WriteString(conn, req.String()); err != nil {
		close(fim)
		_ = conn.Close()
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "falha ao enviar o aperto de mão de exec")
	}

	br := bufio.NewReader(conn)
	base, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	resp, err := http.ReadResponse(br, base)
	if err != nil {
		close(fim)
		_ = conn.Close()
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "resposta ilegível no aperto de mão de exec")
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// O corpo do erro é do apiserver e diz o que faltou (permissão,
		// namespace, pod). Ele sobe para quem chamou traduzir — ver Exec.
		close(fim)
		defer conn.Close()
		corpo, _ := io.ReadAll(io.LimitReader(br, 8<<10))
		resp.Body = io.NopCloser(strings.NewReader(string(corpo)))
		return nil, resp, nil
	}
	// A confirmação existe para provar que quem respondeu FALA WebSocket, e não
	// para provar identidade — isso é do TLS. Sem a checagem, um proxy que
	// respondesse 101 por engano faria a leitura de quadros interpretar HTML
	// como binário e produzir uma saída de comando inventada.
	soma := sha1.Sum([]byte(chaveB64 + wsGUID))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(soma[:]) {
		close(fim)
		_ = conn.Close()
		return nil, nil, errs.New(errs.KindUnavailable,
			"o servidor aceitou o upgrade sem confirmar o WebSocket: quem respondeu não fala o protocolo")
	}

	// Sucesso: `fim` fica ABERTO de propósito. A goroutine acima é a dona do
	// prazo desta conexão e precisa continuar viva até o contexto terminar —
	// que é o que ela faz. Ela não vaza porque todo chamador de exec cria o
	// contexto com prazo e o cancela no fim.
	return &wsConn{conn: conn, br: br}, resp, nil
}

// Close fecha educadamente e derruba a conexão.
//
// O quadro de fechamento é cortesia com o apiserver, que assim não registra a
// conexão como abortada; a queda do socket é o que realmente encerra. Erro
// nenhum aqui interessa: já não há o que salvar.
func (w *wsConn) Close() {
	_ = w.writeFrame(wsOpClose, []byte{0x03, 0xE8}) // 1000 = normal
	_ = w.conn.Close()
}

// ReadMessage devolve a PRÓXIMA mensagem completa, já remontada a partir dos
// quadros. Ping é respondido aqui dentro e não sobe: quem chama quer dados.
func (w *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		op, payload, fim, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case wsOpPing:
			if err := w.writeFrame(wsOpPong, payload); err != nil {
				return nil, err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			return nil, io.EOF
		case wsOpText, wsOpBinary, wsOpContinuation:
			msg = append(msg, payload...)
			if fim {
				return msg, nil
			}
		default:
			// Opcode que este cliente não conhece: ignorar é mais seguro que
			// interpretar. O k8s não usa nenhum além dos acima.
			continue
		}
	}
}

func (w *wsConn) readFrame() (op byte, payload []byte, fim bool, err error) {
	cab := make([]byte, 2)
	if _, err = io.ReadFull(w.br, cab); err != nil {
		return 0, nil, false, err
	}
	fim = cab[0]&0x80 != 0
	op = cab[0] & 0x0f
	mascarado := cab[1]&0x80 != 0
	tam := int64(cab[1] & 0x7f)
	switch tam {
	case 126:
		b := make([]byte, 2)
		if _, err = io.ReadFull(w.br, b); err != nil {
			return 0, nil, false, err
		}
		tam = int64(binary.BigEndian.Uint16(b))
	case 127:
		b := make([]byte, 8)
		if _, err = io.ReadFull(w.br, b); err != nil {
			return 0, nil, false, err
		}
		tam = int64(binary.BigEndian.Uint64(b))
	}
	// Teto de quadro. Um servidor hostil (ou um proxy confuso) anunciando um
	// quadro de gigabytes não pode virar uma alocação de gigabytes aqui.
	const maxQuadro = 32 << 20
	if tam < 0 || tam > maxQuadro {
		return 0, nil, false, errs.New(errs.KindUnavailable,
			"quadro de WebSocket grande demais (%d bytes)", tam)
	}
	var mascara [4]byte
	if mascarado {
		if _, err = io.ReadFull(w.br, mascara[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, tam)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return 0, nil, false, err
	}
	if mascarado {
		// O servidor não deveria mascarar; desmascarar mesmo assim é barato e
		// evita entregar lixo se algum intermediário resolver fazê-lo.
		for i := range payload {
			payload[i] ^= mascara[i%4]
		}
	}
	return op, payload, fim, nil
}

// writeFrame envia um quadro do CLIENTE, e portanto MASCARADO — o RFC 6455 exige,
// e servidor que segue o RFC derruba a conexão de quem não mascara.
func (w *wsConn) writeFrame(op byte, payload []byte) error {
	_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = w.conn.SetWriteDeadline(time.Time{}) }()

	cab := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		cab = append(cab, byte(0x80|n))
	case n < 1<<16:
		cab = append(cab, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(cab[2:], uint16(n))
	default:
		cab = append(cab, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(cab[2:], uint64(n))
	}
	var mascara [4]byte
	if _, err := rand.Read(mascara[:]); err != nil {
		return err
	}
	cab = append(cab, mascara[:]...)
	corpo := make([]byte, n)
	for i := range payload {
		corpo[i] = payload[i] ^ mascara[i%4]
	}
	if _, err := w.conn.Write(append(cab, corpo...)); err != nil {
		return err
	}
	return nil
}
