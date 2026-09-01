// Adaptador de ports.Mailer sobre o SendGrid.
//
// ── O que este adaptador resolve, e o que ele NÃO faz ────────────────────────
//
// Ele mapeia TIPO → `template_id` e manda `dynamic_template_data`. Ele NÃO
// renderiza: o HTML mora no SendGrid, publicado a partir dos arquivos
// versionados em templates/sendgrid/ pelo script idempotente
// (publish_templates.go). É essa escolha que preserva o editor visual, o
// versionamento e a localização do fornecedor — coisas que a plataforma
// perderia se o domínio renderizasse e a porta carregasse um blob de HTML.
//
// ── Por que o índice é compilado e os IDs são configuração ───────────────────
//
// São duas perguntas com prazos diferentes, exatamente como `routingTable` e
// `ModelCatalog` no roteador de custo:
//
//   - QUE TIPOS este adaptador atende é fato do CÓDIGO, e muda junto com a
//     política. Por isso `sendgridIndex` é literal, e é ele que a suíte de
//     contrato exercita: acrescentar um tipo em `notification` sem acrescentar
//     linha aqui reprova;
//   - QUAL É O ID de cada template é fato da INSTALAÇÃO — o `d-…` do SendGrid
//     do cliente não é o do nosso. Por isso vem por configuração, e a falta
//     dele é KindPrecondition (erro de montagem), não KindNotFound (erro de
//     política).
package mailer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// sgTemplate é uma linha do ÍNDICE: o que este fornecedor sabe montar.
type sgTemplate struct {
	// Name é o nome do template DENTRO do SendGrid. É a identidade que o script
	// de publicação usa para decidir entre criar e atualizar — trocá-lo cria um
	// template novo em vez de versionar o existente.
	Name string
	// Subject vai no `subject` da versão publicada. Fica aqui, e não no arquivo
	// HTML, porque o SendGrid guarda assunto separado do corpo.
	Subject string
	// File é o arquivo versionado neste repositório que o script publica.
	File string
}

// sendgridIndex — O ÍNDICE. Chaveado pelo vocabulário do domínio, não por
// string solta: renomear um Kind quebra a compilação aqui, que é onde precisa
// quebrar.
var sendgridIndex = map[string]sgTemplate{
	string(notification.KindInvite): {
		Name:    "DOP — Convite para conta",
		Subject: "Você foi convidado para uma conta no DOP",
		File:    "invite.html",
	},
	string(notification.KindAttentionDigest): {
		Name:    "DOP — Resumo da caixa de atenção",
		Subject: "Você tem pendências esperando",
		File:    "attention_digest.html",
	},
}

type SendGridConfig struct {
	// APIKey é o valor JÁ RESOLVIDO da credencial. Este pacote não conhece
	// ports.SecretStore. VAZIO liga o ENSAIO LOCAL: imprime em vez de enviar.
	APIKey string
	// BaseURL é https://api.sendgrid.com no serviço público. Existe para a
	// suíte de contrato apontar para um httptest.Server — a API do SendGrid
	// nunca é chamada de verdade em teste.
	BaseURL  string
	From     string
	FromName string
	// Templates é tipo → `template_id`, vindo da configuração da instalação.
	Templates map[string]string
	Timeout   time.Duration
	Client    httpDoer
}

type SendGrid struct {
	base string
	http httpDoer
	// autorizar carrega a chave em CLOSURE — ver o cabeçalho do pacote. Não há
	// campo `apiKey` neste struct, e essa ausência é a garantia 4.
	autorizar func(*http.Request)
	redigir   func(string) string
	// ensaiando é BOOLEANO, derivado da chave no construtor. Guardar o booleano
	// em vez da chave é o que permite decidir o modo sem manter o segredo.
	ensaiando bool
	from      string
	fromName  string
	templates map[string]string
}

func NewSendGrid(cfg SendGridConfig) *SendGrid {
	base := naoVazio(cfg.BaseURL, "https://api.sendgrid.com")
	chave := cfg.APIKey
	doer := cfg.Client
	if doer == nil {
		t := cfg.Timeout
		if t <= 0 {
			t = DefaultTimeout
		}
		doer = &http.Client{Timeout: t}
	}
	ids := map[string]string{}
	for k, v := range cfg.Templates {
		if strings.TrimSpace(v) != "" {
			ids[k] = v
		}
	}
	return &SendGrid{
		base: strings.TrimRight(base, "/"),
		http: doer,
		autorizar: func(r *http.Request) {
			if chave != "" {
				r.Header.Set("Authorization", "Bearer "+chave)
			}
		},
		redigir:   redactor(chave),
		ensaiando: chave == "",
		from:      naoVazio(cfg.From, DefaultFrom),
		fromName:  naoVazio(cfg.FromName, DefaultFromName),
		templates: ids,
	}
}

var _ ports.Mailer = (*SendGrid)(nil)

// String: receptor por VALOR, para valer também em `%+v` de um valor. Sem ele,
// o fmt percorreria os campos por reflexão — inclusive os não exportados, cujo
// String() ele não consegue chamar.
func (s SendGrid) String() string { return "mailer.SendGrid{}" }

// Resolve é a garantia 2: responde sem I/O e sem enviar.
func (s *SendGrid) Resolve(_ context.Context, kind string) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return errs.Invalid("aviso sem tipo: o canal não tem o que resolver")
	}
	if _, ok := sendgridIndex[kind]; !ok {
		return desconhecido("SendGrid", kind, chaves(sendgridIndex))
	}
	// No ensaio o id não é necessário: não há chamada ao fornecedor para fazer.
	// Cobrar o id aqui faria a suíte de contrato — e a máquina de todo dev —
	// exigir configuração de produção para provar uma garantia de política.
	if s.ensaiando {
		return nil
	}
	if s.templates[kind] == "" {
		return errs.Precondition(
			"o template do aviso %q não foi configurado neste SendGrid: publique com "+
				"publish_templates.go e informe o id resultante", kind)
	}
	return nil
}

// sgMail é a forma do `/v3/mail/send` — só o que a porta usa.
type sgMail struct {
	From             sgAddr            `json:"from"`
	Personalizations []sgPersonalizado `json:"personalizations"`
	TemplateID       string            `json:"template_id"`
}

type sgAddr struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type sgPersonalizado struct {
	To []sgAddr `json:"to"`
	// DynamicTemplateData é o que o template do fornecedor consome. É aqui que
	// o `Data` da porta desemboca, sem tradução: a porta fala intenção e dados,
	// e o SendGrid consome dados.
	DynamicTemplateData map[string]any `json:"dynamic_template_data,omitempty"`
}

type sgErro struct {
	Errors []struct {
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"errors"`
}

func (s *SendGrid) Send(ctx context.Context, m ports.Mail) (*ports.MailReceipt, error) {
	if err := validar(m); err != nil {
		return nil, err
	}
	// RESOLVE PRIMEIRO — inclusive no ensaio (garantia 3).
	if err := s.Resolve(ctx, m.Kind); err != nil {
		return nil, err
	}
	spec := sendgridIndex[m.Kind]

	if s.ensaiando {
		return ensaio(ctx, "sendgrid", m, spec.Subject, ""), nil
	}

	corpo := sgMail{
		From:       sgAddr{Email: s.from, Name: s.fromName},
		TemplateID: s.templates[m.Kind],
		Personalizations: []sgPersonalizado{{
			To:                  []sgAddr{{Email: m.To, Name: m.ToName}},
			DynamicTemplateData: comAssunto(m.Data, spec.Subject),
		}},
	}
	raw, err := json.Marshal(corpo)
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "aviso ilegível para o SendGrid")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v3/mail/send",
		strings.NewReader(string(raw)))
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "requisição inválida para o SendGrid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dop-core")
	s.autorizar(req)

	resp, err := s.http.Do(req)
	if err != nil {
		// A mensagem do net/http carrega a URL; a URL não carrega a chave
		// (autorização vai em cabeçalho), mas o redator passa por cima assim
		// mesmo — é barato e cobre o dia em que alguém mudar isso.
		return nil, errs.New(errs.KindUnavailable, "falha ao falar com o SendGrid: %s",
			s.redigir(err.Error()))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &ports.MailReceipt{
			State:    ports.MailSent,
			Provider: "sendgrid",
			// O SendGrid devolve o id no cabeçalho, não no corpo — 202 sem
			// corpo é a resposta normal dele.
			Reference: resp.Header.Get("X-Message-Id"),
		}, nil
	}
	return nil, s.falha(resp.StatusCode, body)
}

// falha traduz o status do fornecedor para o Kind da porta (garantia 7).
func (s *SendGrid) falha(code int, body []byte) error {
	msg := s.redigir(explicarSG(body))
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		// Sem detalhe do fornecedor de propósito: 401/403 é sempre a mesma
		// decisão para quem opera — a credencial não serve — e o corpo do 401 é
		// o lugar mais provável de um fornecedor ecoar o que recebeu.
		return errs.New(errs.KindUnauthorized,
			"o SendGrid recusou a credencial desta instalação (HTTP %d)", code)
	case code == http.StatusTooManyRequests || code >= 500:
		return errs.New(errs.KindUnavailable,
			"o SendGrid não aceitou o aviso (HTTP %d): %s", code, msg)
	case code >= 400:
		// 400 do SendGrid é conteúdo ou endereço recusado: é erro de quem
		// chamou, e reenviar igual dá o mesmo resultado.
		return errs.Invalid("o SendGrid recusou o aviso (HTTP %d): %s", code, msg)
	}
	return errs.Internal("resposta inesperada do SendGrid (HTTP %d): %s", code, msg)
}

// explicarSG achata o corpo de erro do SendGrid numa frase.
func explicarSG(body []byte) string {
	var e sgErro
	if err := json.Unmarshal(body, &e); err != nil || len(e.Errors) == 0 {
		s := strings.TrimSpace(string(body))
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		return s
	}
	partes := make([]string, 0, len(e.Errors))
	for _, it := range e.Errors {
		p := strings.TrimSpace(it.Message)
		if it.Field != "" {
			p = it.Field + ": " + p
		}
		if p != "" {
			partes = append(partes, p)
		}
	}
	return strings.Join(partes, "; ")
}

// comAssunto injeta o assunto nos dados, como o projeto irmão faz: o template
// publicado usa `{{subject}}`, o que permite trocar o assunto sem republicar
// versão nova do HTML.
//
// Cópia, e não escrita no mapa recebido: o chamador reusa o mesmo `Data` para
// todos os destinatários de um resumo, e mutar aquilo dentro do adaptador é o
// tipo de efeito colateral que só aparece no segundo destinatário.
func comAssunto(d map[string]any, assunto string) map[string]any {
	out := make(map[string]any, len(d)+1)
	for k, v := range d {
		out[k] = v
	}
	out["subject"] = assunto
	return out
}
