// Package mailer implementa a porta ports.Mailer sobre o SendGrid e sobre SMTP.
//
// Padrão desta casa, pelos mesmos motivos dos adaptadores de gitprovider,
// secretstore e sandbox: `net/http` e `net/smtp` puros, sem SDK de fornecedor.
// O SDK traria o vocabulário do provedor de volta para dentro do processo e
// arrastaria uma árvore de dependências para fazer UMA chamada HTTP.
//
// ── O que mora AQUI e não no domínio ─────────────────────────────────────────
//
// O ÍNDICE de templates, a RESOLUÇÃO e o ENVIO (ADR-0025). A porta fala
// INTENÇÃO — "convite criado, para este endereço, com estes dados" — e cada
// adaptador decide o que isso vira:
//
//	SendGrid  mapeia tipo → template_id e manda dynamic_template_data
//	SMTP      renderiza local, dos arquivos versionados neste pacote
//
// É a mesma separação que o roteador de custo já faz: `routingTable` é
// POLÍTICA, `ModelCatalog` é CATÁLOGO. Aqui a política ("que notificação existe
// e quando") está em internal/domain/notification; o catálogo ("qual template
// dela neste fornecedor") está em cada arquivo deste pacote. Trocar de
// fornecedor troca o catálogo, não a política.
//
// É o SMTP que FORÇA essa separação a ser real: com o SendGrid sozinho, nada
// impediria a porta de vazar `template_id`. Como o SMTP não tem template de
// provedor nenhum, ele renderiza dos arquivos daqui — e a porta é obrigada a
// falar tipo, não identificador de fornecedor.
//
// ── Os dois índices são INDEPENDENTES de propósito ──────────────────────────
//
// Um índice compartilhado pelos dois adaptadores pareceria menos repetitivo e
// destruiria a única garantia que importa: a suíte de contrato existe para
// pegar "acrescentei um tipo e esqueci o template do SendGrid", e com índice
// único esse esquecimento seria impossível de cometer — e o do OneSignal, no
// dia em que ele existir, continuaria possível. A duplicação é o preço; a suíte
// é quem impede que um fique para trás.
//
// ── Onde mora o segredo ─────────────────────────────────────────────────────
//
// A chave do SendGrid e a senha do SMTP são CREDENCIAL: vivem no cofre e chegam
// aqui já resolvidas pelo composition root. Este pacote não importa
// ports.SecretStore e não sabe que cofre existe.
//
// E, uma vez dentro, o segredo NÃO SAI: ele é capturado num CLOSURE, nunca
// guardado em campo de struct. Campo — mesmo não exportado — é impresso por
// `%+v`, porque o fmt lê campos não exportados por reflexão e não consegue
// chamar o String() deles. Closure imprime como endereço. É a diferença entre
// "prometemos não logar a chave" e "não há chave para logar".
package mailer

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// DefaultTimeout cobre a chamada única ao fornecedor. Enviar e-mail é uma
// requisição curta; um envio que demora 30s está quebrado, não lento.
const DefaultTimeout = 30 * time.Second

// DefaultFrom é o remetente de último recurso. Existe para o ensaio local e
// para a suíte de contrato não precisarem de configuração; em produção o
// composition root informa o remetente da instalação.
const DefaultFrom = "noreply@dop.local"

// DefaultFromName idem.
const DefaultFromName = "DOP"

// httpDoer permite injetar o cliente na suíte de contrato sem expor o
// transporte para o composition root — mesma escolha dos outros adaptadores.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// redactor devolve a função que apaga o segredo de qualquer texto que vá subir.
// Segredo vazio devolve identidade — sem isso, `strings.ReplaceAll(s, "", x)`
// espalharia o marcador entre TODOS os caracteres da mensagem.
func redactor(segredos ...string) func(string) string {
	var reais []string
	for _, s := range segredos {
		if s != "" {
			reais = append(reais, s)
		}
	}
	if len(reais) == 0 {
		return func(s string) string { return s }
	}
	return func(s string) string {
		for _, seg := range reais {
			s = strings.ReplaceAll(s, seg, "***")
		}
		return s
	}
}

// validar é a garantia 5: endereço vazio, sem "@", ou tipo vazio são
// KindInvalid decididos SEM I/O. Quem chama sem endereço não pode custar uma
// ida ao fornecedor.
func validar(m ports.Mail) error {
	if strings.TrimSpace(m.Kind) == "" {
		return errs.Invalid("aviso sem tipo: o canal não tem o que resolver")
	}
	to := strings.TrimSpace(m.To)
	i := strings.LastIndex(to, "@")
	if i <= 0 || i >= len(to)-1 || strings.ContainsAny(to, " \t\r\n") {
		// A mensagem NÃO ecoa o endereço inteiro: erro sobe para log, e log é
		// onde dado pessoal vira dado em repouso.
		return errs.Invalid("destinatário inválido para o aviso %q", m.Kind)
	}
	return nil
}

// desconhecido é a garantia 1, escrita uma vez para os dois adaptadores.
//
// A mensagem diz o que FAZER, e não só o que faltou, porque este é o erro que a
// suíte de contrato existe para provocar: quem o vê acabou de acrescentar um
// tipo de notificação e ainda não deu template a ele em algum fornecedor.
func desconhecido(quem, kind string, conhecidos []string) error {
	return errs.NotFound(
		"o canal %s não tem template para o aviso %q (conhece: %s) — um tipo que existe "+
			"na política e não tem template no fornecedor falha em SILÊNCIO: o evento "+
			"acontece, o consumidor roda e ninguém recebe (ADR-0025)",
		quem, kind, strings.Join(conhecidos, ", "))
}

// ensaio é o modo de desenvolvimento do projeto irmão: sem credencial, imprime
// em vez de enviar.
//
// Ele acontece DEPOIS da resolução do template, nunca antes (garantia 3). Um
// ensaio que pulasse a resolução esconderia exatamente o defeito da garantia 1
// em todo ambiente sem chave — que é onde a suíte roda, e onde o dev trabalha.
//
// Sai pelo logger, e não por `fmt.Println`, porque o formato de saída desta
// casa é JSON canônico: um print solto seria a única linha que a coleta de log
// não conseguiria ler.
func ensaio(ctx context.Context, quem string, m ports.Mail, assunto string, corpo string) *ports.MailReceipt {
	log := logging.From(ctx)
	log.Info("ENSAIO de e-mail: sem credencial, nada foi enviado",
		"canal", quem,
		"kind", m.Kind,
		"to", m.To,
		"assunto", assunto,
		"dados", m.Data,
		"corpo_bytes", len(corpo),
	)
	return &ports.MailReceipt{State: ports.MailSentLocal, Provider: quem}
}

// chaves devolve os tipos de um índice, em ordem estável. Mapa em Go itera
// aleatoriamente, e mensagem de erro que muda de ordem entre execuções é
// impossível de casar em teste e irritante de ler em log.
func chaves[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func naoVazio(v, padrao string) string {
	if strings.TrimSpace(v) == "" {
		return padrao
	}
	return v
}
