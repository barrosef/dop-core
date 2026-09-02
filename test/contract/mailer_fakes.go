package contract

// Local doubles of SendGrid and of an SMTP server.
//
// ── Why they exist, and where their limit is ─────────────────────────────────
//
// We have neither SendGrid nor an email relay in CI or on the laptop of whoever
// touches the adapter — and SendGrid's API is NEVER really called in a test,
// because a test that sends email is a test nobody runs twice. With no double,
// the Mailer's suite would be a decorative file, which is the worst thing a test
// file can be.
//
// The double's RISK is the opposite and it is worse: a double written from what
// MY adapter expects proves nothing — it confirms my own assumptions. Hence:
//
//   - SendGrid's responses come from the published documentation of
//     `/v3/mail/send` (202 with no body, `X-Message-Id` in the header, an error
//     in `{"errors":[…]}`);
//   - the SMTP server speaks RFC 5321's protocol by hand, and what validates
//     that it is a real server is not the double: it is the standard library's
//     `net/smtp` on the adapter's side, which refuses any deviation from the
//     sequence.
//
// What they do NOT prove: deliverability, sender reputation, and what each
// provider does with malformed HTML. That is not verifiable without sending real
// email, and it is not what this port promises.

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

// NewSendGridDouble brings up an httptest.Server that answers `/v3/mail/send`.
//
// `idsByKind` is the map the RUNNER configured on the adapter; the double
// inverts it to say, in the Inbox, WHICH KIND arrived. Without that inversion
// the suite could not prove the `invite` notice was not sent with the digest's
// template — which is the missing template's sibling defect, and the more
// unpleasant of the two: somebody receives the wrong message.
func NewSendGridDouble(t *testing.T, idsByKind map[string]string, f Failure, secret string) (string, *Inbox) {
	t.Helper()
	inbox := &Inbox{}
	tipoPorID := map[string]string{}
	for kind, id := range idsByKind {
		tipoPorID[id] = kind
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbox.Touched()
		if r.URL.Path != "/v3/mail/send" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch f {
		case FailureCredential:
			// The body ECHOES the key received. It is deliberate: it is how the
			// suite proves the adapter redacts instead of passing it along.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			writeError(w, "key "+r.Header.Get("Authorization")+" not authorized")
			return
		case FailureUnavailable:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			writeError(w, "instabilidade ao processar a key "+secret)
			return
		case FailureContent:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			writeError(w, "does not contain a valid address")
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
		inbox.Received(SentMail{
			To: to, Kind: tipoPorID[body.TemplateID], Subject: assunto,
			Body: body.TemplateID,
		})
		// 202 with no body, with the id in the header: it is what the documentation publishes.
		w.Header().Set("X-Message-Id", "msg-"+body.TemplateID)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, inbox
}

func writeError(w http.ResponseWriter, msg string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"message": msg}},
	})
}

// ── SMTP ────────────────────────────────────────────────────────────────────

// NewSMTPDouble brings up a local SMTP server.
//
// It speaks RFC 5321's protocol by hand because the standard library ships no
// server, and because pulling in a test dependency for this would go against
// this house's deliberately lean go.mod. It is ~60 lines; the `net/smtp` on the
// adapter's side is what checks they are right.
func NewSMTPDouble(t *testing.T, f Failure, secret string) (string, *Inbox) {
	t.Helper()
	inbox := &Inbox{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("the test SMTP server did not come up: %v", err)
	}
	addr := ln.Addr().String()

	if f == FailureUnavailable {
		// A closed port: the adapter cannot even connect. It is the most common
		// failure of an internal relay, and the error path least exercised.
		_ = ln.Close()
		return addr, inbox
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSMTP(conn, inbox, f, secret)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return addr, inbox
}

func serveSMTP(conn net.Conn, inbox *Inbox, f Failure, secret string) {
	defer conn.Close()
	inbox.Touched()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	reply := func(s string) {
		_, _ = w.WriteString(s + "\r\n")
		_ = w.Flush()
	}
	reply("220 dop-teste ESMTP")

	var to string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			// Multi-line, as the RFC requires: the `-` continues, the space
			// ends. AUTH is always announced; the one who decides to
			// authenticate is the adapter.
			reply("250-dop-teste")
			reply("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(cmd, "HELO"):
			reply("250 dop-teste")
		case strings.HasPrefix(cmd, "AUTH"):
			if f == FailureCredential {
				// The refusal ECHOES what was received — the password included,
				// in base64 in the AUTH PLAIN argument and in clear here. It is
				// how the suite proves the adapter redacts.
				reply("535 5.7.8 the credential was refused: " + secret)
				continue
			}
			reply("235 2.7.0 autenticado")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			reply("250 2.1.0 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			if f == FailureContent {
				// 550 is PERMANENT: resending gives the same result. It is what
				// separates KindInvalid from KindUnavailable in guarantee 7.
				reply("550 5.1.1 no such inbox")
				continue
			}
			to = extractAddress(cmd)
			reply("250 2.1.5 ok")
		case cmd == "DATA":
			reply("354 send the body, end with <CRLF>.<CRLF>")
			body, ok := readSMTPBody(r)
			if !ok {
				return
			}
			inbox.Received(SentMail{
				To:      to,
				Kind:    headerOf(body, "X-DOP-Kind"),
				Subject: decodeSubject(headerOf(body, "Subject")),
				Body:    body,
			})
			reply("250 2.0.0 aceito")
		case cmd == "QUIT":
			reply("221 2.0.0 tchau")
			return
		case cmd == "RSET":
			to = ""
			reply("250 2.0.0 ok")
		default:
			reply("500 5.5.2 comando desconhecido")
		}
	}
}

// readSMTPBody reads up to the lone "." and undoes RFC 5321's dot-stuffing.
func readSMTPBody(r *bufio.Reader) (string, bool) {
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		s := strings.TrimRight(line, "\r\n")
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

func headerOf(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			return "" // the end of the header
		}
		if strings.HasPrefix(strings.ToLower(line), strings.ToLower(name)+":") {
			return strings.TrimSpace(line[len(name)+1:])
		}
	}
	return ""
}

// decodeSubject undoes RFC 2047's encoded-word just enough for the suite's
// assertion. It is not a complete decoder — it is the minimum needed to prove
// the subject arrived non-empty and with the right text.
func decodeSubject(s string) string {
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

func extractAddress(cmd string) string {
	i, j := strings.Index(cmd, "<"), strings.LastIndex(cmd, ">")
	if i >= 0 && j > i {
		return strings.ToLower(cmd[i+1 : j])
	}
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(cmd, "RCPT TO:")))
}
