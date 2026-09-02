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

// fakeRepo mimics the unique index's semantics: the second claim of the SAME key
// returns false, and only a key in error is resumed. It is the only part of the
// repository the domain depends on, and mimicking it wrongly here would let the
// test approve a service that sends duplicate emails.
type fakeRepo struct {
	mu         sync.Mutex
	claims     map[string]State
	tentativas map[string]int
	settled    []Outcome
	membros    []Recipient
	maduros    []AttentionNotice
	contas     []string
	corte      time.Time
	erroClaim  error
}

func novoRepo() *fakeRepo {
	return &fakeRepo{claims: map[string]State{}, tentativas: map[string]int{}}
}

func chaveDe(k DeliveryKey) string {
	return k.EventID + "|" + k.Rule + "|" + string(k.Action)
}

func (r *fakeRepo) Claim(_ context.Context, c Claim, maxAttempts int) (bool, error) {
	if r.erroClaim != nil {
		return false, r.erroClaim
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := chaveDe(c.DeliveryKey)
	estado, existe := r.claims[k]
	switch {
	case !existe:
	case estado == StateError && r.tentativas[k] < maxAttempts:
	default:
		return false, nil
	}
	r.claims[k] = StatePending
	r.tentativas[k]++
	return true, nil
}

func (r *fakeRepo) Settle(_ context.Context, o Outcome) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range o.Keys {
		r.claims[chaveDe(k)] = o.State
	}
	r.settled = append(r.settled, o)
	return "lote-1", nil
}

func (r *fakeRepo) Recipients(_ context.Context, _ string) ([]Recipient, error) {
	return r.membros, nil
}

func (r *fakeRepo) AccountsWithRipeAttention(_ context.Context, _ string, _ Action,
	olderThan time.Time, _ int) ([]string, error) {
	r.corte = olderThan
	return r.contas, nil
}

func (r *fakeRepo) RipeAttention(_ context.Context, accountID, _ string, _ Action,
	olderThan time.Time, _, limit int) ([]AttentionNotice, error) {
	r.corte = olderThan
	var out []AttentionNotice
	for _, n := range r.maduros {
		// The double applies the SAME predicate as the SQL: only what opened
		// before the cutoff. Without that, the delay test would prove only that
		// the service calls the repository, not that it computes the right
		// cutoff.
		if n.AccountID == accountID && !n.OpenedAt.After(olderThan) {
			out = append(out, n)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type fakeMailer struct {
	mu     sync.Mutex
	sent   []ports.Mail
	err    error
	estado ports.MailState
}

func (m *fakeMailer) Resolve(context.Context, string) error { return nil }

func (m *fakeMailer) Send(_ context.Context, mail ports.Mail) (*ports.MailReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	m.sent = append(m.sent, mail)
	estado := m.estado
	if estado == "" {
		estado = ports.MailSent
	}
	return &ports.MailReceipt{State: estado, Provider: "fake", Reference: "ref-1"}, nil
}

type relogioFake struct{ t time.Time }

func (r relogioFake) Now() time.Time { return r.t }

// ── testes ──────────────────────────────────────────────────────────────────

func TestANilClockIsRefused(t *testing.T) {
	// A nil clock would switch the abstraction off in silence — and here it is BY
	// the clock that the digest's delay is measured.
	defer func() {
		if recover() == nil {
			t.Fatal("NewService accepted a nil clock")
		}
	}()
	NewService(novoRepo(), &fakeMailer{}, nil, Config{})
}

func eventoDeConvite(id string) Event {
	return Event{
		ID: id, AccountID: "acct-1", Aggregate: "invite", AggregateID: "inv-1",
		Type: EvInviteCreated, OccurredAt: time.Now().UTC(),
		// This payload has to be WHAT THE PRODUCER EMITS, field by field. It is
		// montado em adapter/postgres/identity.go:CreateInvite, e um teste de
		// cross-domain integration test checks that the two have not diverged —
		// there is no way to know that from here.
		Payload: map[string]any{
			"invite_id": "inv-1",
			"email":     "convidado@exemplo.test",
			"role":      "member",
		},
	}
}

func TestAnInviteBecomesAnEmail(t *testing.T) {
	repo, mail := novoRepo(), &fakeMailer{}
	s := NewService(repo, mail, relogioFake{time.Now()},
		Config{BaseURL: "https://cockpit.test"})

	if err := s.HandleEvent(context.Background(), eventoDeConvite("ev-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(mail.sent) != 1 {
		t.Fatalf("expected 1 send, got %d", len(mail.sent))
	}
	m := mail.sent[0]
	if m.Kind != string(KindInvite) || m.To != "convidado@exemplo.test" {
		t.Fatalf("mensagem errada: %+v", m)
	}
	// The link addresses THE INVITE, not the list: whoever receives it is not yet
	// a user and has no list to look at.
	if m.Data["link"] != "https://cockpit.test/invites/inv-1" {
		t.Fatalf("the digest's link: %v", m.Data["link"])
	}
	if len(repo.settled) != 1 || repo.settled[0].State != StateSent {
		t.Fatalf("registro: %+v", repo.settled)
	}
}

// THE GUARANTEE WITH NO UNDO. JetStream delivery is at-least-once.
func TestRedeliveryOfTheSameEventSendsNoSecondEmail(t *testing.T) {
	repo, mail := novoRepo(), &fakeMailer{}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.HandleEvent(ctx, eventoDeConvite("ev-1")); err != nil {
			t.Fatalf("entrega %d: %v", i, err)
		}
	}
	if len(mail.sent) != 1 {
		t.Fatalf("redelivery sent %d emails; a duplicate email is visible to the "+
			"user and has no undo", len(mail.sent))
	}
}

func TestADifferentEventOfTheSameKindSendsAnotherEmail(t *testing.T) {
	// The mirror of the test above: an idempotency that swallowed the second
	// INVITE would be worse than the duplicate — somebody invited would never
	// know.
	repo, mail := novoRepo(), &fakeMailer{}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{})
	_ = s.HandleEvent(context.Background(), eventoDeConvite("ev-1"))
	_ = s.HandleEvent(context.Background(), eventoDeConvite("ev-2"))
	if len(mail.sent) != 2 {
		t.Fatalf("expected 2 sends, got %d", len(mail.sent))
	}
	_ = repo
}

func TestASendFailureRecordsErrorAndAllowsResume(t *testing.T) {
	repo := novoRepo()
	mail := &fakeMailer{err: errs.New(errs.KindUnavailable, "fornecedor fora do ar")}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{})
	ctx := context.Background()

	err := s.HandleEvent(ctx, eventoDeConvite("ev-1"))
	if err == nil {
		t.Fatal("a send failure has to propagate: redelivery is what gives a second chance")
	}
	if len(repo.settled) != 1 || repo.settled[0].State != StateError {
		t.Fatalf("the record should be in error: %+v", repo.settled)
	}
	if !strings.Contains(repo.settled[0].Error, "fora do ar") {
		t.Fatalf("o registro perdeu o motivo: %q", repo.settled[0].Error)
	}

	// Redelivery now RESUMES (the claim is in error) and the send succeeds.
	mail.err = nil
	if err := s.HandleEvent(ctx, eventoDeConvite("ev-1")); err != nil {
		t.Fatalf("retomada: %v", err)
	}
	if len(mail.sent) != 1 {
		t.Fatalf("the resume did not resend: %d", len(mail.sent))
	}
}

func TestExhaustedAttemptsStopResending(t *testing.T) {
	repo := novoRepo()
	mail := &fakeMailer{err: errors.New("caiu")}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{MaxAttempts: 2})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_ = s.HandleEvent(ctx, eventoDeConvite("ev-1"))
	}
	if n := len(repo.settled); n != 2 {
		t.Fatalf("%d attempts: with no ceiling, an invalid address becomes a traffic "+
			"generator aimed at the provider — and that is how sender reputation is "+
			"remetente", n)
	}
}

func TestLocalRehearsalBecomesItsOwnStateInTheRecord(t *testing.T) {
	repo := novoRepo()
	mail := &fakeMailer{estado: ports.MailSentLocal}
	s := NewService(repo, mail, relogioFake{time.Now()}, Config{})
	_ = s.HandleEvent(context.Background(), eventoDeConvite("ev-1"))
	if repo.settled[0].State != StateSentLocal {
		t.Fatalf("state %q: the difference between 'we notified' and 'we pretended "+
			"to notify' must not depend on whoever reads the log remembering which "+
			"environment that ran in",
			repo.settled[0].State)
	}
}

// ── o atraso do resumo ──────────────────────────────────────────────────────

func itemMaduro(evento string, aberto time.Time) AttentionNotice {
	return AttentionNotice{
		AccountID: "acct-1", EventID: evento, ItemID: "it-" + evento,
		Kind: "thread_blocked", Title: "Um agente precisa de resposta",
		OpenedAt: aberto,
	}
}

func TestATooFreshItemDoesNotBecomeAnEmail(t *testing.T) {
	// The item opens, waits, and only becomes an email if it is still open.
	// Whoever was in the cockpit has already handled it.
	agora := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	repo := novoRepo()
	repo.contas = []string{"acct-1"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{itemMaduro("ev-novo", agora.Add(-5*time.Minute))}

	mail := &fakeMailer{}
	s := NewService(repo, mail, relogioFake{agora}, Config{DigestDelay: 15 * time.Minute})

	contas, avisos, err := s.SweepDigest(context.Background())
	if err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	if avisos != 0 || len(mail.sent) != 0 {
		t.Fatalf("a 5-minute-old item became an e-mail (accounts=%d, digests=%d, sends=%d): "+
			"o atraso existe para o trivial se resolver sozinho",
			contas, avisos, len(mail.sent))
	}
	if got := agora.Sub(repo.corte); got != 15*time.Minute {
		t.Fatalf("the cut-off was computed with a delay of %v, expected 15m", got)
	}
}

func TestAMatureItemBecomesAnEmail(t *testing.T) {
	agora := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	repo := novoRepo()
	repo.contas = []string{"acct-1"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{itemMaduro("ev-velho", agora.Add(-20*time.Minute))}

	mail := &fakeMailer{}
	s := NewService(repo, mail, relogioFake{agora},
		Config{DigestDelay: 15 * time.Minute, BaseURL: "https://cockpit.test"})

	if _, avisos, err := s.SweepDigest(context.Background()); err != nil || avisos != 1 {
		t.Fatalf("avisos=%d err=%v", avisos, err)
	}
	if len(mail.sent) != 1 {
		t.Fatalf("expected 1 send, got %d", len(mail.sent))
	}
	m := mail.sent[0]
	if m.Kind != string(KindAttentionDigest) {
		t.Fatalf("tipo %q", m.Kind)
	}
	if m.Data["total"] != 1 {
		t.Fatalf("total nos dados: %v", m.Data["total"])
	}
	if m.Data["link"] != "https://cockpit.test/attention" {
		t.Fatalf("link: %v", m.Data["link"])
	}
}

func TestManyItemsBecomeOneEmailWithOneClaimPerItem(t *testing.T) {
	// "One email per item makes the inbox useless." But idempotency stays PER
	// ITEM, otherwise a new item would reopen the old ones.
	agora := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	velho := agora.Add(-30 * time.Minute)
	repo := novoRepo()
	repo.contas = []string{"acct-1"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{
		itemMaduro("ev-1", velho), itemMaduro("ev-2", velho), itemMaduro("ev-3", velho),
	}

	mail := &fakeMailer{}
	s := NewService(repo, mail, relogioFake{agora}, Config{DigestDelay: 15 * time.Minute})
	if _, _, err := s.SweepDigest(context.Background()); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	if len(mail.sent) != 1 {
		t.Fatalf("3 itens viraram %d e-mails", len(mail.sent))
	}
	if n := len(repo.settled[0].Keys); n != 3 {
		t.Fatalf("the e-mail covered %d keys, expected 3", n)
	}
	itens, _ := mail.sent[0].Data["items"].([]map[string]any)
	if len(itens) != 3 {
		t.Fatalf("o corpo listou %d itens", len(itens))
	}

	// Segunda varredura: nada novo, e nenhum e-mail.
	if _, avisos, _ := s.SweepDigest(context.Background()); avisos != 0 {
		t.Fatalf("the second sweep reported %d item(s) again", avisos)
	}
	if len(mail.sent) != 1 {
		t.Fatalf("the second sweep sent another e-mail: %d", len(mail.sent))
	}
}

func TestOneEmailPerRecipient(t *testing.T) {
	// One message with everybody in copy would hand the account's address list
	// conta a cada membro.
	agora := time.Now().UTC()
	repo := novoRepo()
	repo.contas = []string{"acct-1"}
	repo.membros = []Recipient{{Email: "a@exemplo.test"}, {Email: "b@exemplo.test"}}
	repo.maduros = []AttentionNotice{itemMaduro("ev-1", agora.Add(-time.Hour))}

	mail := &fakeMailer{}
	s := NewService(repo, mail, relogioFake{agora}, Config{})
	if _, _, err := s.SweepDigest(context.Background()); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	if len(mail.sent) != 2 {
		t.Fatalf("expected 2 messages (one per recipient), got %d", len(mail.sent))
	}
	if mail.sent[0].To == mail.sent[1].To {
		t.Fatal("both messages went to the same address")
	}
}

func TestAnAccountWithNoRecipientClaimsNothing(t *testing.T) {
	// Claiming here would record "notified" for items nobody saw — and the day a
	// member gained a verified address, they would start in debt to their own
	// box.
	agora := time.Now().UTC()
	repo := novoRepo()
	repo.contas = []string{"acct-1"}
	repo.maduros = []AttentionNotice{itemMaduro("ev-1", agora.Add(-time.Hour))}

	s := NewService(repo, &fakeMailer{}, relogioFake{agora}, Config{})
	if _, avisos, err := s.SweepDigest(context.Background()); err != nil || avisos != 0 {
		t.Fatalf("avisos=%d err=%v", avisos, err)
	}
	if len(repo.claims) != 0 {
		t.Fatalf("it reserved %d key(s) with nobody to send to", len(repo.claims))
	}
}

func TestTheSweepVisitsEachAccountWithThatAccountInContext(t *testing.T) {
	// The scheduler has no active account, and the whole rest of the domain
	// requires one. The way out is to visit account by account — isolation is not
	// loosened, what changes is who
	// decide a ordem de visita.
	agora := time.Now().UTC()
	repo := &repoEspiaDeConta{fakeRepo: novoRepo()}
	repo.contas = []string{"acct-1", "acct-2"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{
		{AccountID: "acct-1", EventID: "ev-1", Title: "a", OpenedAt: agora.Add(-time.Hour)},
		{AccountID: "acct-2", EventID: "ev-2", Title: "b", OpenedAt: agora.Add(-time.Hour)},
	}
	s := NewService(repo, &fakeMailer{}, relogioFake{agora}, Config{})
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
	*fakeRepo
	contasNoContexto []string
}

func (r *repoEspiaDeConta) RipeAttention(ctx context.Context, accountID, rule string,
	action Action, olderThan time.Time, maxAttempts, limit int) ([]AttentionNotice, error) {
	c, _ := ctxutil.From(ctx)
	r.contasNoContexto = append(r.contasNoContexto, c.AccountID)
	return r.fakeRepo.RipeAttention(ctx, accountID, rule, action, olderThan, maxAttempts, limit)
}

func TestOneBrokenAccountDoesNotStopTheOthers(t *testing.T) {
	agora := time.Now().UTC()
	repo := novoRepo()
	repo.contas = []string{"acct-1", "acct-2"}
	repo.membros = []Recipient{{Email: "dev@exemplo.test"}}
	repo.maduros = []AttentionNotice{
		{AccountID: "acct-1", EventID: "ev-1", Title: "a", OpenedAt: agora.Add(-time.Hour)},
		{AccountID: "acct-2", EventID: "ev-2", Title: "b", OpenedAt: agora.Add(-time.Hour)},
	}
	mail := &selectiveMailer{falhaPara: "ev-1"}
	s := NewService(repo, mail, relogioFake{agora}, Config{})
	contas, _, err := s.SweepDigest(context.Background())
	if err != nil {
		t.Fatalf("the whole sweep stopped because of one account: %v", err)
	}
	if contas != 1 {
		t.Fatalf("accounts that succeeded: %d, expected 1", contas)
	}
	if len(mail.sent) != 1 {
		t.Fatalf("the second account was not notified: %d send(s)", len(mail.sent))
	}
}

// selectiveMailer fails only when the digest covers one specific item.
type selectiveMailer struct {
	falhaPara string
	sent      []ports.Mail
}

func (m *selectiveMailer) Resolve(context.Context, string) error { return nil }

func (m *selectiveMailer) Send(_ context.Context, mail ports.Mail) (*ports.MailReceipt, error) {
	itens, _ := mail.Data["items"].([]map[string]any)
	for _, it := range itens {
		if it["title"] == "a" && m.falhaPara != "" {
			return nil, errs.New(errs.KindUnavailable, "refused")
		}
	}
	m.sent = append(m.sent, mail)
	return &ports.MailReceipt{State: ports.MailSent, Provider: "fake"}, nil
}
