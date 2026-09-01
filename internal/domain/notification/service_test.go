package notification

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── duplos ──────────────────────────────────────────────────────────────────

// repoFake imita a semântica do índice único: a segunda reserva da MESMA chave
// devolve false, e só a chave em erro é retomada. É a única parte do
// repositório que o domínio depende, e imitá-la errado aqui faria o teste
// aprovar um serviço que manda e-mail duplicado.
type repoFake struct {
	mu         sync.Mutex
	reservas   map[string]State
	tentativas map[string]int
	liquidados []Outcome
	membros    []Recipient
	maduros    []AttentionNotice
	contas     []string
	corte      time.Time
	erroClaim  error
}

func novoRepo() *repoFake {
	return &repoFake{reservas: map[string]State{}, tentativas: map[string]int{}}
}

func chaveDe(k DeliveryKey) string {
	return k.EventID + "|" + k.Rule + "|" + string(k.Action)
}

func (r *repoFake) Claim(_ context.Context, c Claim, maxAttempts int) (bool, error) {
	if r.erroClaim != nil {
		return false, r.erroClaim
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := chaveDe(c.DeliveryKey)
	estado, existe := r.reservas[k]
	switch {
	case !existe:
	case estado == StateError && r.tentativas[k] < maxAttempts:
	default:
		return false, nil
	}
	r.reservas[k] = StatePending
	r.tentativas[k]++
	return true, nil
}

func (r *repoFake) Settle(_ context.Context, o Outcome) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range o.Keys {
		r.reservas[chaveDe(k)] = o.State
	}
	r.liquidados = append(r.liquidados, o)
	return "lote-1", nil
}

func (r *repoFake) Recipients(_ context.Context, _ string) ([]Recipient, error) {
	return r.membros, nil
}

func (r *repoFake) AccountsWithRipeAttention(_ context.Context, _ string, _ Action,
	olderThan time.Time, _ int) ([]string, error) {
	r.corte = olderThan
	return r.contas, nil
}

func (r *repoFake) RipeAttention(_ context.Context, accountID, _ string, _ Action,
	olderThan time.Time, _, limit int) ([]AttentionNotice, error) {
	r.corte = olderThan
	var out []AttentionNotice
	for _, n := range r.maduros {
		// O duplo aplica o MESMO predicado que o SQL: só o que abriu antes do
		// corte. Sem isso o teste do atraso provaria apenas que o serviço chama
		// o repositório, não que ele calcula o corte certo.
		if n.AccountID == accountID && !n.OpenedAt.After(olderThan) {
			out = append(out, n)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type mailerFake struct {
	mu       sync.Mutex
	enviados []ports.Mail
	erro     error
	estado   ports.MailState
}

func (m *mailerFake) Resolve(context.Context, string) error { return nil }

func (m *mailerFake) Send(_ context.Context, mail ports.Mail) (*ports.MailReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erro != nil {
		return nil, m.erro
	}
	m.enviados = append(m.enviados, mail)
	estado := m.estado
	if estado == "" {
		estado = ports.MailSent
	}
	return &ports.MailReceipt{State: estado, Provider: "fake", Reference: "ref-1"}, nil
}

type relogioFake struct{ t time.Time }

func (r relogioFake) Now() time.Time { return r.t }

// ── testes ──────────────────────────────────────────────────────────────────

func TestRelogioNuloEhRecusado(t *testing.T) {
	// Relógio nulo desligaria a abstração em silêncio — e aqui é PELO relógio
	// que o atraso do resumo é medido.
	defer func() {
		if recover() == nil {
			t.Fatal("NewService aceitou relógio nulo")
		}
	}()
	NewService(novoRepo(), &mailerFake{}, nil, Config{})
}

func eventoDeConvite(id string) Event {
	return Event{
		ID: id, AccountID: "conta-1", Aggregate: "invite", AggregateID: "inv-1",
		Type: EvInviteCreated, OccurredAt: time.Now().UTC(),
		Payload: map[string]any{"email": "convidado@exemplo.test", "role": "member"},
	}
}

func TestConviteViraUmEmail(t *testing.T) {
	repo, mail := novoRepo(), &mailerFake{}
	s := NewService(repo, mail, relogioFake{time.Now()},
		Config{BaseURL: "https://cockpit.test"})

	if err := s.HandleEvent(context.Background(), eventoDeConvite("ev-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(mail.enviados) != 1 {
		t.Fatalf("esperava 1 envio, veio %d", len(mail.enviados))
	}
	m := mail.enviados[0]
	if m.Kind != string(KindInvite) || m.To != "convidado@exemplo.test" {
		t.Fatalf("mensagem errada: %+v", m)
	}
	if m.Data["link"] != "https://cockpit.test/convites" {
		t.Fatalf("link do aviso: %v", m.Data["link"])
	}
	if len(repo.liquidados) != 1 || repo.liquidados[0].State != StateSent {
		t.Fatalf("registro: %+v", repo.liquidados)
	}
}

// A GARANTIA QUE NÃO TEM DESFAZER. A entrega do JetStream é ao-menos-uma-vez.
func TestReentregaDoMesmoEventoNaoManda2oEmail(t *testing.T) {
	repo, mail := novoRepo(), &mailerFake{}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.HandleEvent(ctx, eventoDeConvite("ev-1")); err != nil {
			t.Fatalf("entrega %d: %v", i, err)
		}
	}
	if len(mail.enviados) != 1 {
		t.Fatalf("a reentrega mandou %d e-mails; e-mail duplicado é visível para o "+
			"usuário e não tem desfazer", len(mail.enviados))
	}
}

func TestEventoDIFERENTEDoMesmoTipoMandaOutroEmail(t *testing.T) {
	// O espelho do teste acima: idempotência que engolisse o segundo CONVITE
	// seria pior que a duplicata — alguém convidado nunca saberia.
	repo, mail := novoRepo(), &mailerFake{}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{})
	_ = s.HandleEvent(context.Background(), eventoDeConvite("ev-1"))
	_ = s.HandleEvent(context.Background(), eventoDeConvite("ev-2"))
	if len(mail.enviados) != 2 {
		t.Fatalf("esperava 2 envios, veio %d", len(mail.enviados))
	}
	_ = repo
}

func TestFalhaDeEnvioRegistraErroEPermiteRetomada(t *testing.T) {
	repo := novoRepo()
	mail := &mailerFake{erro: errs.New(errs.KindUnavailable, "fornecedor fora do ar")}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{})
	ctx := context.Background()

	err := s.HandleEvent(ctx, eventoDeConvite("ev-1"))
	if err == nil {
		t.Fatal("falha de envio precisa subir: é a reentrega que dá segunda chance")
	}
	if len(repo.liquidados) != 1 || repo.liquidados[0].State != StateError {
		t.Fatalf("registro deveria estar em error: %+v", repo.liquidados)
	}
	if !strings.Contains(repo.liquidados[0].Error, "fora do ar") {
		t.Fatalf("o registro perdeu o motivo: %q", repo.liquidados[0].Error)
	}

	// A reentrega agora RETOMA (a reserva está em error) e o envio dá certo.
	mail.erro = nil
	if err := s.HandleEvent(ctx, eventoDeConvite("ev-1")); err != nil {
		t.Fatalf("retomada: %v", err)
	}
	if len(mail.enviados) != 1 {
		t.Fatalf("a retomada não reenviou: %d", len(mail.enviados))
	}
}

func TestTentativasEsgotadasParamDeReenviar(t *testing.T) {
	repo := novoRepo()
	mail := &mailerFake{erro: errors.New("caiu")}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{MaxAttempts: 2})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_ = s.HandleEvent(ctx, eventoDeConvite("ev-1"))
	}
	if n := len(repo.liquidados); n != 2 {
		t.Fatalf("%d tentativas: sem teto, um endereço inválido vira gerador de "+
			"tráfego contra o fornecedor — e é assim que se perde reputação de "+
			"remetente", n)
	}
}

func TestEnsaioLocalVirAEstadoProprioNoRegistro(t *testing.T) {
	repo := novoRepo()
	mail := &mailerFake{estado: ports.MailSentLocal}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{})
	_ = s.HandleEvent(context.Background(), eventoDeConvite("ev-1"))
	if repo.liquidados[0].State != StateSentLocal {
		t.Fatalf("estado %q: a diferença entre 'avisamos' e 'fingimos avisar' não "+
			"pode depender de quem lê o log lembrar em que ambiente aquilo rodou",
			repo.liquidados[0].State)
	}
}

// ── o atraso do resumo ──────────────────────────────────────────────────────

func itemMaduro(evento string, aberto time.Time) AttentionNotice {
	return AttentionNotice{
		AccountID: "conta-1", EventID: evento, ItemID: "it-" + evento,
		Kind: "thread_blocked", Title: "Um agente precisa de resposta",
		OpenedAt: aberto,
	}
}

func TestItemNOVODEMAISNaoViraEmail(t *testing.T) {
	// O item abre, espera, e só vira e-mail se ainda estiver aberto. Quem
	// estava no cockpit já resolveu.
	agora := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	repo := novoRepo()
	repo.contas = []string{"conta-1"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{itemMaduro("ev-novo", agora.Add(-5*time.Minute))}

	mail := &mailerFake{}
	s := NewService(repo, mail, relogioFake{agora}, Config{DigestDelay: 15 * time.Minute})

	contas, avisos, err := s.SweepDigest(context.Background())
	if err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	if avisos != 0 || len(mail.enviados) != 0 {
		t.Fatalf("item de 5 minutos virou e-mail (contas=%d, avisos=%d, envios=%d): "+
			"o atraso existe para o trivial se resolver sozinho",
			contas, avisos, len(mail.enviados))
	}
	if got := agora.Sub(repo.corte); got != 15*time.Minute {
		t.Fatalf("o corte foi calculado com atraso de %v, esperava 15m", got)
	}
}

func TestItemMADUROViraEmail(t *testing.T) {
	agora := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	repo := novoRepo()
	repo.contas = []string{"conta-1"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{itemMaduro("ev-velho", agora.Add(-20*time.Minute))}

	mail := &mailerFake{}
	s := NewService(repo, mail, relogioFake{agora},
		Config{DigestDelay: 15 * time.Minute, BaseURL: "https://cockpit.test"})

	if _, avisos, err := s.SweepDigest(context.Background()); err != nil || avisos != 1 {
		t.Fatalf("avisos=%d err=%v", avisos, err)
	}
	if len(mail.enviados) != 1 {
		t.Fatalf("esperava 1 envio, veio %d", len(mail.enviados))
	}
	m := mail.enviados[0]
	if m.Kind != string(KindAttentionDigest) {
		t.Fatalf("tipo %q", m.Kind)
	}
	if m.Data["total"] != 1 {
		t.Fatalf("total nos dados: %v", m.Data["total"])
	}
	if m.Data["link"] != "https://cockpit.test/atencao" {
		t.Fatalf("link: %v", m.Data["link"])
	}
}

func TestVariosItensViramUMEmailSoComUmaReservaPorItem(t *testing.T) {
	// "Um e-mail por item torna a caixa de entrada inútil." Mas a idempotência
	// continua sendo POR ITEM, senão um item novo reabriria os antigos.
	agora := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	velho := agora.Add(-30 * time.Minute)
	repo := novoRepo()
	repo.contas = []string{"conta-1"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{
		itemMaduro("ev-1", velho), itemMaduro("ev-2", velho), itemMaduro("ev-3", velho),
	}

	mail := &mailerFake{}
	s := NewService(repo, mail, relogioFake{agora}, Config{DigestDelay: 15 * time.Minute})
	if _, _, err := s.SweepDigest(context.Background()); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	if len(mail.enviados) != 1 {
		t.Fatalf("3 itens viraram %d e-mails", len(mail.enviados))
	}
	if n := len(repo.liquidados[0].Keys); n != 3 {
		t.Fatalf("o e-mail cobriu %d chaves, esperava 3", n)
	}
	itens, _ := mail.enviados[0].Data["items"].([]map[string]any)
	if len(itens) != 3 {
		t.Fatalf("o corpo listou %d itens", len(itens))
	}

	// Segunda varredura: nada novo, e nenhum e-mail.
	if _, avisos, _ := s.SweepDigest(context.Background()); avisos != 0 {
		t.Fatalf("a segunda varredura reavisou %d item(ns)", avisos)
	}
	if len(mail.enviados) != 1 {
		t.Fatalf("a segunda varredura mandou outro e-mail: %d", len(mail.enviados))
	}
}

func TestUmEmailPorDESTINATARIO(t *testing.T) {
	// Uma mensagem com todo mundo em cópia entregaria a lista de endereços da
	// conta a cada membro.
	agora := time.Now().UTC()
	repo := novoRepo()
	repo.contas = []string{"conta-1"}
	repo.membros = []Recipient{{Email: "a@exemplo.test"}, {Email: "b@exemplo.test"}}
	repo.maduros = []AttentionNotice{itemMaduro("ev-1", agora.Add(-time.Hour))}

	mail := &mailerFake{}
	s := NewService(repo, mail, relogioFake{agora}, Config{})
	if _, _, err := s.SweepDigest(context.Background()); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	if len(mail.enviados) != 2 {
		t.Fatalf("esperava 2 mensagens (uma por destinatário), veio %d", len(mail.enviados))
	}
	if mail.enviados[0].To == mail.enviados[1].To {
		t.Fatal("as duas mensagens foram para o mesmo endereço")
	}
}

func TestContaSemDestinatarioNaoReservaNada(t *testing.T) {
	// Reservar aqui gravaria "avisado" para itens que ninguém viu — e o dia em
	// que um membro ganhasse e-mail verificado, ele começaria em dívida com a
	// própria caixa.
	agora := time.Now().UTC()
	repo := novoRepo()
	repo.contas = []string{"conta-1"}
	repo.maduros = []AttentionNotice{itemMaduro("ev-1", agora.Add(-time.Hour))}

	s := NewService(repo, &mailerFake{}, relogioFake{agora}, Config{})
	if _, avisos, err := s.SweepDigest(context.Background()); err != nil || avisos != 0 {
		t.Fatalf("avisos=%d err=%v", avisos, err)
	}
	if len(repo.reservas) != 0 {
		t.Fatalf("reservou %d chave(s) sem ter para quem enviar", len(repo.reservas))
	}
}

func TestVarreduraVisitaCadaContaComAContaNoContexto(t *testing.T) {
	// O scheduler não tem conta ativa, e todo o resto do domínio exige uma. A
	// saída é visitar conta por conta — o isolamento não é afrouxado, muda quem
	// decide a ordem de visita.
	agora := time.Now().UTC()
	repo := &repoEspiaDeConta{repoFake: novoRepo()}
	repo.contas = []string{"conta-1", "conta-2"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{
		{AccountID: "conta-1", EventID: "ev-1", Title: "a", OpenedAt: agora.Add(-time.Hour)},
		{AccountID: "conta-2", EventID: "ev-2", Title: "b", OpenedAt: agora.Add(-time.Hour)},
	}
	s := NewService(repo, &mailerFake{}, relogioFake{agora}, Config{})
	if _, _, err := s.SweepDigest(context.Background()); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	for i, visto := range repo.contasNoContexto {
		if visto == "" {
			t.Fatalf("a visita %d rodou SEM conta no contexto: o filtro por conta "+
				"deixaria de valer dentro do scheduler", i)
		}
	}
	if len(repo.contasNoContexto) != 2 {
		t.Fatalf("visitou %d conta(s)", len(repo.contasNoContexto))
	}
	if repo.contasNoContexto[0] == repo.contasNoContexto[1] {
		t.Fatal("as duas visitas usaram a mesma conta no contexto")
	}
}

type repoEspiaDeConta struct {
	*repoFake
	contasNoContexto []string
}

func (r *repoEspiaDeConta) RipeAttention(ctx context.Context, accountID, rule string,
	action Action, olderThan time.Time, maxAttempts, limit int) ([]AttentionNotice, error) {
	c, _ := ctxutil.From(ctx)
	r.contasNoContexto = append(r.contasNoContexto, c.AccountID)
	return r.repoFake.RipeAttention(ctx, accountID, rule, action, olderThan, maxAttempts, limit)
}

func TestUmaContaComProblemaNaoParaAsOutras(t *testing.T) {
	agora := time.Now().UTC()
	repo := novoRepo()
	repo.contas = []string{"conta-1", "conta-2"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{
		{AccountID: "conta-1", EventID: "ev-1", Title: "a", OpenedAt: agora.Add(-time.Hour)},
		{AccountID: "conta-2", EventID: "ev-2", Title: "b", OpenedAt: agora.Add(-time.Hour)},
	}
	mail := &mailerSeletivo{falhaPara: "ev-1"}
	s := NewService(repo, mail, relogioFake{agora}, Config{})
	contas, _, err := s.SweepDigest(context.Background())
	if err != nil {
		t.Fatalf("a varredura inteira parou por causa de uma conta: %v", err)
	}
	if contas != 1 {
		t.Fatalf("contas com sucesso: %d, esperava 1", contas)
	}
	if len(mail.enviados) != 1 {
		t.Fatalf("a segunda conta não foi avisada: %d envio(s)", len(mail.enviados))
	}
}

// mailerSeletivo falha só quando o resumo cobre um item específico.
type mailerSeletivo struct {
	falhaPara string
	enviados  []ports.Mail
}

func (m *mailerSeletivo) Resolve(context.Context, string) error { return nil }

func (m *mailerSeletivo) Send(_ context.Context, mail ports.Mail) (*ports.MailReceipt, error) {
	itens, _ := mail.Data["items"].([]map[string]any)
	for _, it := range itens {
		if it["title"] == "a" && m.falhaPara != "" {
			return nil, errs.New(errs.KindUnavailable, "recusado")
		}
	}
	m.enviados = append(m.enviados, mail)
	return &ports.MailReceipt{State: ports.MailSent, Provider: "fake"}, nil
}
