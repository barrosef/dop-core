package notification

import (
	"context"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// Config é o AJUSTE da instalação, não vocabulário do domínio.
type Config struct {
	// BaseURL é o endereço do cockpit desta instalação. Vazio faz o aviso sair
	// sem link — degradação declarada, não esquecimento: um link para
	// "/atencao" sem base é um link quebrado, e link quebrado num e-mail custa
	// mais confiança do que a ausência dele.
	BaseURL string
	// DigestDelay sobrescreve DefaultDigestDelay. Ver lá por que ele existe e
	// por que é palpite.
	DigestDelay time.Duration
	// MaxAttempts limita o reenvio do que falhou. Zero usa DefaultMaxAttempts.
	MaxAttempts int
	// DigestLimit é o teto de itens que UM e-mail carrega. Existe porque
	// resumo de duzentos itens não é resumo — é a caixa de atenção mal
	// impressa, e ninguém lê.
	DigestLimit int
}

// DefaultDigestLimit é quantos itens cabem num resumo antes de ele virar ruído.
const DefaultDigestLimit = 20

// Service é o EXECUTOR: pega o comando que o decisor produziu, reserva a chave,
// dispara pelo canal e registra o desfecho.
//
// Ele não decide nada — a decisão inteira está na tabela (rules.go). Essa
// separação é o que o P-29 vai cobrar: quando a reação virar dado, troca-se o
// decisor e este arquivo não muda.
type Service struct {
	repo   Repository
	mailer ports.Mailer
	clock  ports.Clock
	cfg    Config
}

func NewService(repo Repository, mailer ports.Mailer, clock ports.Clock, cfg Config) *Service {
	if repo == nil || mailer == nil {
		panic("notification.NewService: repositório e mailer são obrigatórios")
	}
	// Relógio é PORTA e recusa nil, como em todo serviço desta casa: aceitar
	// nil e cair no time.Now() desliga a abstração sem ninguém perceber e
	// devolve ao teste a dependência do relógio de parede. E aqui isso seria
	// pior que o normal — o atraso do resumo é medido POR ELE, e um teste que
	// não controla o relógio não consegue provar que o item resolvido em cinco
	// minutos não virou e-mail.
	if clock == nil {
		panic("notification.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	if cfg.DigestDelay <= 0 {
		cfg.DigestDelay = DefaultDigestDelay
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.DigestLimit <= 0 {
		cfg.DigestLimit = DefaultDigestLimit
	}
	return &Service{repo: repo, mailer: mailer, clock: clock, cfg: cfg}
}

// ── o gatilho transacional: consumidor da espinha de eventos ────────────────

// HandleEvent é o CONSUMIDOR. Roda no worker, ao lado da timeline e da caixa de
// atenção (ADR-0025: "roda no núcleo").
//
// É idempotente porque precisa ser: a entrega do JetStream é ao-menos-uma-vez
// (ADR-0019) e e-mail duplicado não tem desfazer. A idempotência não está numa
// checagem no começo — está na RESERVA, que é atômica no banco.
func (s *Service) HandleEvent(ctx context.Context, e Event) error {
	cmds := Apply(e, func(spec recipientSpec, ev Event) []Recipient {
		return s.resolver(ctx, spec, ev)
	})
	for _, c := range cmds {
		if !c.Valid() {
			continue
		}
		if err := s.executar(ctx, c); err != nil {
			// Sobe o erro: quem chama é o barramento, e a reentrega é o que dá
			// segunda chance ao que falhou. A reserva já está em StateError, e
			// só ela é retomável — a reentrega não vira e-mail duplicado.
			return err
		}
	}
	return nil
}

// resolver traduz a origem declarada na regra em endereços de verdade. É a
// única parte da decisão que precisa de I/O, e é por isso que ela mora aqui e
// não na tabela: tabela que faz consulta deixa de ser dado.
func (s *Service) resolver(ctx context.Context, spec recipientSpec, e Event) []Recipient {
	switch spec.Source {
	case fromPayload:
		return payloadEmail(spec, e)
	case fromAccountMembers:
		dest, err := s.repo.Recipients(ctx, e.AccountID)
		if err != nil {
			// Falha ao resolver destinatário não pode virar comando sem
			// destinatário: seria a mesma saída de "não há ninguém para
			// avisar", e as duas situações exigem respostas opostas.
			logging.From(ctx).Error("falha ao resolver destinatários da conta",
				logging.FieldError, err.Error())
			return nil
		}
		return dest
	}
	return nil
}

// ── o aviso de atenção: varredura de minuto, sem agendador ──────────────────

// SweepDigest é o aviso de atenção com atraso, rodando como SISTEMA.
//
// ── Como o atraso é implementado sem inventar agendador ─────────────────────
//
// Não há timer, não há fila de trabalho futura e não há nada a cancelar quando
// o item se resolve. O scheduler já roda de minuto em minuto
// (`RunScheduledTasks`), e o atraso vira um PREDICADO DE CONSULTA: "itens
// AINDA ABERTOS, abertos antes de agora−atraso, que ainda não viraram aviso".
//
// Item resolvido antes do corte simplesmente nunca entra no resultado — e é
// exatamente o comportamento que a ADR pede ("quem estava no cockpit já
// resolveu"). Um agendador daria o mesmo resultado com uma peça a mais, um
// estado a mais e um cancelamento a mais para errar; e o cancelamento é o
// caminho onde esse tipo de desenho falha, porque ele é a parte que só executa
// no caso raro.
//
// O preço é honesto: o aviso sai com granularidade de minuto, e um atraso de
// 15 minutos vira algo entre 15 e 16. Para um resumo cuja finalidade é esperar
// o trivial se resolver, isso não é erro — é a própria unidade.
func (s *Service) SweepDigest(ctx context.Context) (contas, avisos int, err error) {
	regra, ok := DigestRule()
	if !ok {
		// Sem regra de resumo na tabela não há varredura. Não é erro: é a
		// política dizendo que a caixa não gera e-mail nesta instalação.
		return 0, 0, nil
	}
	atraso := regra.Delay
	if s.cfg.DigestDelay > 0 {
		atraso = s.cfg.DigestDelay
	}
	if atraso <= 0 {
		atraso = DefaultDigestDelay
	}
	corte := s.clock.Now().Add(-atraso)

	ids, err := s.repo.AccountsWithRipeAttention(ctx, regra.Name, regra.Action, corte, s.cfg.MaxAttempts)
	if err != nil {
		return 0, 0, err
	}
	for _, accountID := range ids {
		// Ator de SISTEMA com a conta da vez — o mesmo Call que o interceptor
		// montaria numa chamada de usuário. É o que faz o filtro por conta
		// valer também aqui, sem abrir exceção no domínio.
		porConta := ctxutil.Into(ctx, ctxutil.Call{
			AccountID: accountID,
			ActorID:   "scheduler",
			ActorKind: ctxutil.ActorSystem,
			ActorName: "notifier",
		})
		n, err := s.digestDaConta(porConta, regra, accountID, corte)
		if err != nil {
			// Erro numa conta não interrompe as outras: aviso de uma conta não
			// pode ficar preso porque a conta anterior tem problema.
			logging.From(ctx).Error("resumo da caixa falhou nesta conta",
				"account_id", accountID, logging.FieldError, err.Error())
			continue
		}
		contas++
		avisos += n
	}
	return contas, avisos, nil
}

func (s *Service) digestDaConta(ctx context.Context, regra Rule, accountID string, corte time.Time) (int, error) {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return 0, err
	}
	itens, err := s.repo.RipeAttention(ctx, accountID, regra.Name, regra.Action, corte,
		s.cfg.MaxAttempts, s.cfg.DigestLimit)
	if err != nil {
		return 0, err
	}
	if len(itens) == 0 {
		return 0, nil
	}
	dest, err := s.repo.Recipients(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if len(dest) == 0 {
		// Ninguém com endereço conhecido. NÃO reserva: reservar aqui gravaria
		// "avisado" para itens que ninguém viu, e o dia em que um membro
		// ganhasse e-mail ele começaria já em dívida com a própria caixa.
		return 0, nil
	}

	// Uma reserva POR ITEM — a chave é (evento, regra, ação) e o evento é o que
	// ABRIU o item. O agrupamento em uma mensagem vem depois; a idempotência
	// não é agrupada, senão um item novo reabriria os antigos.
	enderecos := emails(dest)
	cobertos := make([]DeliveryKey, 0, len(itens))
	pegos := make([]AttentionNotice, 0, len(itens))
	for _, it := range itens {
		chave := DeliveryKey{EventID: it.EventID, Rule: regra.Name, Action: regra.Action}
		ok, err := s.repo.Claim(ctx, Claim{
			DeliveryKey: chave, AccountID: accountID, Kind: regra.Kind,
			Channel: string(regra.Action), Recipients: enderecos,
		}, s.cfg.MaxAttempts)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue // já avisado (ou esgotou tentativas)
		}
		cobertos = append(cobertos, chave)
		pegos = append(pegos, it)
	}
	if len(cobertos) == 0 {
		return 0, nil
	}

	dados := map[string]any{
		"account_id": accountID,
		"total":      len(pegos),
		"items":      itensParaDados(pegos),
		"link":       s.link(regra.LinkPath),
	}
	recibo, envErr := s.enviar(ctx, accountID, regra.Kind, dest, dados)
	if _, err := s.repo.Settle(ctx, desfecho(accountID, cobertos, regra.Kind, enderecos, recibo, envErr)); err != nil {
		return 0, err
	}
	if envErr != nil {
		return 0, envErr
	}
	return len(cobertos), nil
}

// ── execução comum aos dois gatilhos ────────────────────────────────────────

// executar é reservar → disparar → liquidar, nessa ordem e sem atalho.
func (s *Service) executar(ctx context.Context, c Command) error {
	chave := DeliveryKey{EventID: c.EventID, Rule: c.Rule, Action: c.Action}
	ok, err := s.repo.Claim(ctx, Claim{
		DeliveryKey: chave, AccountID: c.AccountID, Kind: c.Kind,
		Channel: string(c.Action), Recipients: emails(c.Recipients),
	}, s.cfg.MaxAttempts)
	if err != nil {
		return err
	}
	if !ok {
		// Reentrega do mesmo evento, ou tentativas esgotadas. Silêncio aqui é
		// correto: o registro já conta a história, e devolver erro faria o
		// barramento reentregar para sempre uma mensagem que já foi atendida.
		logging.From(ctx).Debug("aviso já registrado, nada a fazer",
			"rule", c.Rule, "action", string(c.Action), "kind", string(c.Kind))
		return nil
	}

	dados := map[string]any{"account_id": c.AccountID, "link": s.linkDaRegra(c.Rule)}
	for k, v := range c.Data {
		dados[k] = v
	}

	recibo, envErr := s.enviar(ctx, c.AccountID, c.Kind, c.Recipients, dados)
	if _, err := s.repo.Settle(ctx, desfecho(c.AccountID, []DeliveryKey{chave}, c.Kind,
		emails(c.Recipients), recibo, envErr)); err != nil {
		return err
	}
	return envErr
}

// enviar dispara UMA mensagem por destinatário.
//
// Uma por destinatário, e não uma com todo mundo em cópia, porque cópia
// entrega a lista de endereços da conta a cada membro — e um convite tem
// destinatário que ainda nem faz parte dela.
//
// O primeiro erro interrompe: se o fornecedor está fora do ar, insistir nos
// outros nove só multiplica o tempo até a linha ficar em StateError, e a
// retomada reenvia todos de qualquer jeito.
func (s *Service) enviar(ctx context.Context, accountID string, kind Kind, dest []Recipient, dados map[string]any) (*ports.MailReceipt, error) {
	var ultimo *ports.MailReceipt
	for _, r := range dest {
		rec, err := s.mailer.Send(ctx, ports.Mail{
			AccountID: accountID,
			Kind:      string(kind),
			To:        r.Email,
			ToName:    r.Name,
			Data:      dados,
		})
		if err != nil {
			return ultimo, err
		}
		ultimo = rec
	}
	if ultimo == nil {
		return nil, errs.Invalid("aviso sem destinatário")
	}
	return ultimo, nil
}

// desfecho monta o registro a partir do que o canal devolveu.
//
// Erro do envio vira StateError com a mensagem do adaptador — que já vem
// REDIGIDA (garantia 4 da porta). O domínio não redige nada, porque ele não
// sabe qual é o segredo; se soubesse, o segredo estaria no lugar errado.
func desfecho(accountID string, chaves []DeliveryKey, kind Kind, enderecos []string,
	rec *ports.MailReceipt, err error) Outcome {
	o := Outcome{
		AccountID: accountID, Keys: chaves, Kind: kind, Recipients: enderecos,
	}
	if err != nil {
		o.State = StateError
		o.Error = err.Error()
		if rec != nil {
			o.Provider = rec.Provider
		}
		return o
	}
	o.Provider = rec.Provider
	o.Reference = rec.Reference
	switch rec.State {
	case ports.MailSentLocal:
		o.State = StateSentLocal
	default:
		o.State = StateSent
	}
	return o
}

func (s *Service) link(path string) string {
	if s.cfg.BaseURL == "" || path == "" {
		return ""
	}
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (s *Service) linkDaRegra(nome string) string {
	for _, r := range Rules() {
		if r.Name == nome {
			return s.link(r.LinkPath)
		}
	}
	return ""
}

func emails(rs []Recipient) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Email)
	}
	return out
}

// itensParaDados achata os itens para o template. Mapa e não struct: o que
// atravessa a porta são DADOS do template, e um struct daqui obrigaria todo
// adaptador a conhecer o tipo deste pacote.
func itensParaDados(itens []AttentionNotice) []map[string]any {
	out := make([]map[string]any, 0, len(itens))
	for _, it := range itens {
		out = append(out, map[string]any{
			"kind": it.Kind, "title": it.Title, "summary": it.Summary,
			"opened_at": it.OpenedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}
