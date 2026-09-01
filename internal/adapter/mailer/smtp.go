// Adaptador de ports.Mailer sobre SMTP.
//
// É o caminho do self-hosted — o mesmo par GCP/OKD das outras portas —, e é ele
// que FORÇA a resolução local de template. Com o SendGrid sozinho, nada
// impediria a porta de vazar `template_id`: o campo estaria lá, o domínio
// acabaria preenchendo, e a "porta que fala intenção" viraria uma porta que
// fala SendGrid. Como aqui não existe template de provedor, o adaptador
// renderiza dos arquivos versionados em templates/smtp/ — e a porta é obrigada
// a falar TIPO.
//
// O preço, assumido pela ADR-0025: perde-se o editor visual do fornecedor.
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Os arquivos são EMBUTIDOS no binário, não lidos do disco em tempo de
// execução. Um caminho de sistema de arquivos faria o mesmo binário mandar
// e-mail diferente conforme onde ele foi montado — e o dia em que o volume não
// estivesse lá, o aviso falharia em produção por um motivo invisível no build.
//
//go:embed templates/smtp/*.html
var smtpFiles embed.FS

// smtpTemplate é uma linha do ÍNDICE deste fornecedor. Índice INDEPENDENTE do
// SendGrid de propósito — ver o cabeçalho do pacote.
type smtpTemplate struct {
	// Subject é template de texto, e não literal, porque o assunto é o único
	// lugar do e-mail onde um número muda a decisão de abrir: "3 pendências
	// esperando você" é lido; "Você tem pendências" é arquivado.
	Subject string
	File    string
}

var smtpIndex = map[string]smtpTemplate{
	string(notification.KindInvite): {
		Subject: "Você foi convidado para uma conta no DOP",
		File:    "templates/smtp/invite.html",
	},
	string(notification.KindAttentionDigest): {
		Subject: "{{.total}} pendência(s) esperando você no DOP",
		File:    "templates/smtp/attention_digest.html",
	},
}

type SMTPConfig struct {
	// Addr é host:porta. VAZIO liga o ENSAIO LOCAL: imprime em vez de enviar.
	// É o mesmo gesto da chave vazia no SendGrid, e é ele que faz o ambiente de
	// desenvolvimento não precisar de servidor de e-mail nenhum.
	Addr     string
	Username string
	// Password é o valor JÁ RESOLVIDO da credencial. Ele NÃO vira campo deste
	// adaptador — ver o construtor.
	Password string
	From     string
	FromName string
	// StartTLS pede a promoção da conexão antes de autenticar. Não é
	// automático: um servidor interno de laboratório costuma não oferecer, e
	// tentar sempre transformaria "sem TLS" em "sem e-mail".
	StartTLS  bool
	TLSConfig *tls.Config
	Timeout   time.Duration
	// Dial existe para a suíte de contrato falar com um servidor local de
	// teste, sem expor transporte para o composition root — mesma escolha do
	// `Client` nos adaptadores HTTP.
	Dial func(ctx context.Context) (net.Conn, error)
}

type SMTP struct {
	addr string
	// autenticar carrega a senha em CLOSURE. Não há campo `password` neste
	// struct, e essa ausência é a garantia 4: `%+v` não tem o que imprimir.
	autenticar func(*smtp.Client) error
	redigir    func(string) string
	ensaiando  bool
	from       string
	fromName   string
	startTLS   bool
	tlsCfg     *tls.Config
	timeout    time.Duration
	dial       func(ctx context.Context) (net.Conn, error)
	corpos     *template.Template
	assuntos   *texttemplate.Template
}

func NewSMTP(cfg SMTPConfig) *SMTP {
	senha := cfg.Password
	usuario := cfg.Username

	// missingkey=zero: chave ausente vira vazio, não "<no value>" nem erro. É a
	// garantia 8 da porta — derrubar um convite porque um campo cosmético não
	// veio trocaria um problema de aparência por um bloqueio de acesso.
	corpos := template.Must(template.New("smtp").
		Option("missingkey=zero").
		ParseFS(smtpFiles, "templates/smtp/*.html"))

	assuntos := texttemplate.New("assuntos").Option("missingkey=zero")
	for kind, spec := range smtpIndex {
		texttemplate.Must(assuntos.New(kind).Parse(spec.Subject))
	}

	t := cfg.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	s := &SMTP{
		addr:      cfg.Addr,
		redigir:   redactor(senha),
		ensaiando: strings.TrimSpace(cfg.Addr) == "",
		from:      naoVazio(cfg.From, DefaultFrom),
		fromName:  naoVazio(cfg.FromName, DefaultFromName),
		startTLS:  cfg.StartTLS,
		tlsCfg:    cfg.TLSConfig,
		timeout:   t,
		dial:      cfg.Dial,
		corpos:    corpos,
		assuntos:  assuntos,
	}
	s.autenticar = func(c *smtp.Client) error {
		if usuario == "" || senha == "" {
			// Servidor de relay interno sem autenticação é caso legítimo, não
			// erro. Exigir credencial aqui inviabilizaria o Postfix do cluster.
			return nil
		}
		host, _, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			host = cfg.Addr
		}
		return c.Auth(smtp.PlainAuth("", usuario, senha, host))
	}
	return s
}

var _ ports.Mailer = (*SMTP)(nil)

// String: receptor por VALOR, para valer também em `%+v` de um valor.
func (s SMTP) String() string { return "mailer.SMTP{" + s.addr + "}" }

// Resolve é a garantia 2: responde sem I/O e sem enviar.
func (s *SMTP) Resolve(_ context.Context, kind string) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return errs.Invalid("aviso sem tipo: o canal não tem o que resolver")
	}
	spec, ok := smtpIndex[kind]
	if !ok {
		return desconhecido("SMTP", kind, chaves(smtpIndex))
	}
	// Índice sem arquivo é o mesmo silêncio, um passo adiante: a linha existe,
	// o `go:embed` não trouxe o arquivo, e o envio falharia só em produção.
	if s.corpos.Lookup(nomeDoArquivo(spec.File)) == nil {
		return errs.Precondition(
			"o aviso %q está no índice do SMTP mas o arquivo %q não foi embutido",
			kind, spec.File)
	}
	return nil
}

func (s *SMTP) Send(ctx context.Context, m ports.Mail) (*ports.MailReceipt, error) {
	if err := validar(m); err != nil {
		return nil, err
	}
	// RESOLVE PRIMEIRO — inclusive no ensaio (garantia 3).
	if err := s.Resolve(ctx, m.Kind); err != nil {
		return nil, err
	}
	spec := smtpIndex[m.Kind]

	assunto, err := s.renderAssunto(m.Kind, m.Data)
	if err != nil {
		return nil, err
	}
	var corpo bytes.Buffer
	if err := s.corpos.ExecuteTemplate(&corpo, nomeDoArquivo(spec.File), m.Data); err != nil {
		// Falha de renderização é erro NOSSO, não do fornecedor nem de quem
		// chamou: o template está no binário.
		return nil, errs.Wrap(errs.KindInternal, err,
			"falha ao renderizar o aviso %q", m.Kind)
	}

	if s.ensaiando {
		return ensaio(ctx, "smtp", m, assunto, corpo.String()), nil
	}
	if err := s.entregar(ctx, m, assunto, corpo.Bytes()); err != nil {
		return nil, err
	}
	// Sem Reference: o SMTP não devolve identificador nenhum, e inventar um
	// aqui faria o registro afirmar uma rastreabilidade que não existe
	// (garantia da porta: Reference vazio é NORMAL).
	return &ports.MailReceipt{State: ports.MailSent, Provider: "smtp"}, nil
}

func (s *SMTP) renderAssunto(kind string, data map[string]any) (string, error) {
	var b bytes.Buffer
	if err := s.assuntos.ExecuteTemplate(&b, kind, data); err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "falha ao montar o assunto de %q", kind)
	}
	return strings.TrimSpace(b.String()), nil
}

// entregar fala SMTP na mão, em vez de smtp.SendMail, por dois motivos:
// SendMail não aceita contexto (e um servidor pendurado seguraria o worker até
// o timeout do sistema operacional) e ele decide sozinho sobre STARTTLS.
func (s *SMTP) entregar(ctx context.Context, m ports.Mail, assunto string, corpo []byte) error {
	conn, err := s.conectar(ctx)
	if err != nil {
		return err
	}
	// Prazo na CONEXÃO: é o único ponto onde o contexto alcança o net/smtp,
	// que não conhece context.Context.
	prazo := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(prazo) {
		prazo = d
	}
	_ = conn.SetDeadline(prazo)

	host, _, errSplit := net.SplitHostPort(s.addr)
	if errSplit != nil {
		host = s.addr
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return s.indisponivel("handshake", err)
	}
	defer func() { _ = c.Close() }()

	if s.startTLS {
		cfg := s.tlsCfg
		if cfg == nil {
			cfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		}
		if err := c.StartTLS(cfg); err != nil {
			return s.indisponivel("STARTTLS", err)
		}
	}
	if err := s.autenticar(c); err != nil {
		// Credencial recusada é KindUnauthorized, e não Unavailable: mandar a
		// equipe caçar rede quando o problema é senha custa horas.
		return errs.New(errs.KindUnauthorized,
			"o servidor SMTP recusou a credencial desta instalação: %s", s.redigir(err.Error()))
	}
	if err := c.Mail(s.from); err != nil {
		return s.recusa("remetente", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return s.recusa("destinatário", err)
	}
	w, err := c.Data()
	if err != nil {
		return s.indisponivel("abertura do corpo", err)
	}
	if _, err := w.Write(s.mensagem(m, assunto, corpo)); err != nil {
		return s.indisponivel("escrita do corpo", err)
	}
	if err := w.Close(); err != nil {
		return s.recusa("corpo", err)
	}
	if err := c.Quit(); err != nil {
		// QUIT falho depois de o corpo ter sido aceito NÃO é falha de entrega:
		// o servidor já assumiu a mensagem. Tratar como erro faria o registro
		// dizer "não enviou" para um e-mail que chegou — e a retomada mandaria
		// o segundo.
		return nil
	}
	return nil
}

func (s *SMTP) conectar(ctx context.Context) (net.Conn, error) {
	if s.dial != nil {
		conn, err := s.dial(ctx)
		if err != nil {
			return nil, s.indisponivel("conexão", err)
		}
		return conn, nil
	}
	d := net.Dialer{Timeout: s.timeout}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, s.indisponivel("conexão", err)
	}
	return conn, nil
}

func (s *SMTP) indisponivel(etapa string, err error) error {
	return errs.New(errs.KindUnavailable, "servidor SMTP indisponível em %s: %s",
		etapa, s.redigir(err.Error()))
}

// recusa distingue 5xx (permanente: reenviar dá o mesmo resultado) de 4xx
// (temporário: vale tentar de novo). Confundir os dois ou reenvia para sempre
// um endereço que não existe — o que queima a reputação do remetente — ou
// desiste de um servidor que só estava ocupado.
func (s *SMTP) recusa(oque string, err error) error {
	msg := s.redigir(err.Error())
	var proto *textproto.Error
	if errors.As(err, &proto) && proto.Code >= 400 && proto.Code < 500 {
		return errs.New(errs.KindUnavailable, "o servidor SMTP adiou o %s: %s", oque, msg)
	}
	return errs.Invalid("o servidor SMTP recusou o %s: %s", oque, msg)
}

// mensagem monta o RFC 5322. Cabeçalho mínimo de propósito: cada cabeçalho a
// mais é uma chance a mais de um filtro antispam encontrar defeito.
func (s *SMTP) mensagem(m ports.Mail, assunto string, corpo []byte) []byte {
	var b bytes.Buffer
	// mime.QEncoding: assunto em português tem acento, e assunto não codificado
	// chega com caractere trocado nos clientes mais antigos.
	fmt.Fprintf(&b, "From: %s <%s>\r\n", mime.QEncoding.Encode("utf-8", s.fromName), s.from)
	if m.ToName != "" {
		fmt.Fprintf(&b, "To: %s <%s>\r\n", mime.QEncoding.Encode("utf-8", m.ToName), m.To)
	} else {
		fmt.Fprintf(&b, "To: %s\r\n", m.To)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", assunto))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/html; charset=utf-8\r\n")
	fmt.Fprintf(&b, "Content-Transfer-Encoding: 8bit\r\n")
	// X-DOP-Kind existe para a operação: "por que recebi isto?" e "quais avisos
	// deste tipo saíram?" viram uma busca no servidor de e-mail.
	fmt.Fprintf(&b, "X-DOP-Kind: %s\r\n", m.Kind)
	b.WriteString("\r\n")
	b.Write(corpo)
	return b.Bytes()
}

// nomeDoArquivo devolve o nome com que ParseFS registrou o template: ele usa o
// BASE do caminho, não o caminho inteiro.
func nomeDoArquivo(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// SMTPTemplateSource devolve o FONTE embutido de um template do SMTP, pelo nome
// base do arquivo.
//
// Exportada só para teste, e vale a pena: é ela que permite comparar quais
// campos cada um dos dois templates do mesmo aviso consome. Sem isso, a
// duplicação que a ADR-0025 assume como custo ("dois lugares para o template do
// mesmo aviso") não teria quem a vigiasse — e a divergência entre os dois é
// silenciosa: o e-mail sai nos dois fornecedores, só que um deles sem a
// informação que importa.
func SMTPTemplateSource(arquivo string) (string, error) {
	b, err := smtpFiles.ReadFile("templates/smtp/" + nomeDoArquivo(arquivo))
	if err != nil {
		return "", errs.NotFound("template SMTP %q não foi embutido", arquivo)
	}
	return string(b), nil
}

// SMTPTemplateFile devolve o arquivo que o ÍNDICE do SMTP associa a um tipo.
//
// Exportada para teste, e pelo mesmo motivo do índice ser exercitado: a suíte
// prova que o adaptador resolve o tipo, mas "resolver" e "resolver para o
// artefato CERTO" são coisas diferentes. Trocar os arquivos de dois tipos no
// índice mandaria a mensagem errada com o rótulo certo — e isso é pior que
// template ausente, porque alguém RECEBE algo.
func SMTPTemplateFile(kind string) (string, error) {
	spec, ok := smtpIndex[kind]
	if !ok {
		return "", desconhecido("SMTP", kind, chaves(smtpIndex))
	}
	return spec.File, nil
}
