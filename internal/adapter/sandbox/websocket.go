// A MINIMAL WebSocket client, for Kubernetes's `pods/exec`.
//
// ── Why written by hand ─────────────────────────────────────────────────────
//
// By the same rule that already holds in this repository's other adapters: no
// SDK, and the go.mod is deliberately lean. Here the arithmetic is even more
// favourable than usual — of RFC 6455 this file needs a fraction: the handshake,
// reading the server's frames (which never come masked), the pong and the close.
// There is no data sending beyond the pong: this port's exec is a COMMAND, not a
// session, and `stdin` goes off. A complete WebSocket library would bring
// compression, extensions and a concurrency model this use does not have.
//
// ── Why WebSocket and not SPDY ──────────────────────────────────────────────
//
// `pods/exec` accepts both. Kubernetes's SPDY is a protocol of its own, dead
// outside there, and it would require implementing whole stream multiplexing.
// WebSocket with the `v4.channel.k8s.io` subprotocol delivers the same thing
// with a channel byte in front of each message: 1 = stdout, 2 = stderr, 3 = the
// final status (a `metav1.Status` in JSON, and it is THERE that the exit code
// comes).
//
// ── The `kubectl proxy` catch ───────────────────────────────────────────────
//
// `kubectl proxy` refuses the exec and attach paths by default — the
// `--reject-paths` default includes `^/api/.*/pods/.*/exec`. Outside the
// cluster, the contract suite needs `kubectl proxy --port=8001
// --reject-paths='^$'`, and without it the handshake comes back 403 before any
// WebSocket exists. It is recorded here and in test/contract/sandbox_k8s_test.go's
// header because it is the kind of detail that costs an afternoon when it is
// written down nowhere.
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

// wsGUID is RFC 6455's constant used in the handshake's confirmation.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// The opcodes this client knows. The rest is ignored.
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// wsConn is an already negotiated WebSocket connection.
//
// It is not safe for concurrent use and does not need to be: each exec opens its
// own, reads to the end and closes it.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// wsDial performs the handshake and returns the connection ready to read.
//
// The context governs the WHOLE connection, and not only the handshake: the
// goroutine below closes the socket when it ends. That is how the command's
// deadline and the caller's cancellation reach a read that would otherwise hang
// waiting for bytes from a process that will not speak again.
func wsDial(ctx context.Context, rawURL string, header http.Header,
	subprotocol string, tlsCfg *tls.Config) (*wsConn, *http.Response, error) {

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, errs.Wrap(errs.KindInternal, err, "invalid address for exec")
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
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "failed to open the exec connection")
	}

	// Closing on the context is what makes the deadline apply to the READ, and
	// not only to the connection. Without it, a command that hangs without
	// writing anything would leave the caller's goroutine stuck until the
	// process ended.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		close(done)
		_ = conn.Close()
		return nil, nil, errs.Wrap(errs.KindInternal, err, "failed to draw the WebSocket key")
	}
	keyB64 := base64.StdEncoding.EncodeToString(key)

	var req strings.Builder
	path := u.RequestURI()
	req.WriteString("GET " + path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + u.Host + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	req.WriteString("Sec-WebSocket-Key: " + keyB64 + "\r\n")
	if subprotocol != "" {
		req.WriteString("Sec-WebSocket-Protocol: " + subprotocol + "\r\n")
	}
	for k, vs := range header {
		for _, v := range vs {
			req.WriteString(k + ": " + v + "\r\n")
		}
	}
	req.WriteString("\r\n")

	if _, err := io.WriteString(conn, req.String()); err != nil {
		close(done)
		_ = conn.Close()
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "failed to send the exec handshake")
	}

	br := bufio.NewReader(conn)
	base, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	resp, err := http.ReadResponse(br, base)
	if err != nil {
		close(done)
		_ = conn.Close()
		return nil, nil, errs.Wrap(errs.KindUnavailable, err, "unreadable response in the exec handshake")
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The error body is the apiserver's and says what was missing
		// (permission, namespace, pod). It goes up for the caller to
		// translate — see Exec.
		close(done)
		defer conn.Close()
		body, _ := io.ReadAll(io.LimitReader(br, 8<<10))
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return nil, resp, nil
	}
	// The confirmation exists to prove that whoever answered SPEAKS WebSocket,
	// and not to prove identity — that is TLS's job. Without the check, a proxy
	// answering 101 by mistake would make the frame reading interpret HTML as
	// binary and produce an invented command output.
	sum := sha1.Sum([]byte(keyB64 + wsGUID))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		close(done)
		_ = conn.Close()
		return nil, nil, errs.New(errs.KindUnavailable,
			"the server accepted the upgrade without confirming the WebSocket: whoever answered does not speak the protocol")
	}

	// On success, `done` is left OPEN on purpose. The goroutine above owns this
	// connection's deadline and has to stay alive until the context ends — which
	// is what it does. It does not leak because every exec caller creates the
	// context with a deadline and cancels it at the end.
	return &wsConn{conn: conn, br: br}, resp, nil
}

// Close closes politely and drops the connection.
//
// The close frame is a courtesy to the apiserver, which then does not record the
// connection as aborted; dropping the socket is what really ends it. No error
// here matters: there is nothing left to save.
func (w *wsConn) Close() {
	_ = w.writeFrame(wsOpClose, []byte{0x03, 0xE8}) // 1000 = normal
	_ = w.conn.Close()
}

// ReadMessage returns the NEXT complete message, already reassembled from the
// frames. A ping is answered in here and does not go up: the caller wants data.
func (w *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		op, payload, final, err := w.readFrame()
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
			if final {
				return msg, nil
			}
		default:
			// An opcode this client does not know: ignoring is safer than
			// interpreting. k8s uses none beyond the ones above.
			continue
		}
	}
}

func (w *wsConn) readFrame() (op byte, payload []byte, final bool, err error) {
	head := make([]byte, 2)
	if _, err = io.ReadFull(w.br, head); err != nil {
		return 0, nil, false, err
	}
	final = head[0]&0x80 != 0
	op = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	size := int64(head[1] & 0x7f)
	switch size {
	case 126:
		b := make([]byte, 2)
		if _, err = io.ReadFull(w.br, b); err != nil {
			return 0, nil, false, err
		}
		size = int64(binary.BigEndian.Uint16(b))
	case 127:
		b := make([]byte, 8)
		if _, err = io.ReadFull(w.br, b); err != nil {
			return 0, nil, false, err
		}
		size = int64(binary.BigEndian.Uint64(b))
	}
	// A frame cap. A hostile server (or a confused proxy) announcing a frame of
	// gigabytes must not become an allocation of gigabytes here.
	const maxFrame = 32 << 20
	if size < 0 || size > maxFrame {
		return 0, nil, false, errs.New(errs.KindUnavailable,
			"WebSocket frame too large (%d bytes)", size)
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(w.br, mask[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, size)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return 0, nil, false, err
	}
	if masked {
		// The server should not mask; unmasking anyway is cheap and avoids
		// delivering rubbish if some intermediary decides to do it.
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, final, nil
}

// writeFrame sends a CLIENT frame, and therefore a MASKED one — RFC 6455
// requires it, and a server that follows the RFC drops the connection of anyone
// who does not mask.
func (w *wsConn) writeFrame(op byte, payload []byte) error {
	_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = w.conn.SetWriteDeadline(time.Time{}) }()

	head := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		head = append(head, byte(0x80|n))
	case n < 1<<16:
		head = append(head, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(head[2:], uint16(n))
	default:
		head = append(head, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	head = append(head, mask[:]...)
	body := make([]byte, n)
	for i := range payload {
		body[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(append(head, body...)); err != nil {
		return err
	}
	return nil
}
