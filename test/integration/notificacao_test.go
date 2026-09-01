//go:build integration

// O teste que atravessa a espinha de eventos de verdade.
//
// O motivo é o mesmo de atencao_test.go, e a lição vem de um bug que já
// aconteceu nesta casa: a caixa de atenção lia `status` num evento que o
// domínio emitia com `to`. Os testes de unidade dos dois lados passavam, porque
// cada um usava o formato que SUPUNHA do outro.
//
// A comunicação tem exatamente a mesma exposição, e pior: o sintoma é a
// AUSÊNCIA de um e-mail. Se a regra ler `email` num payload que o domínio de
// identidade emite com outro nome, `Apply` devolve zero comandos, o consumidor
// devolve nil, o JetStream dá ack, e nada em lugar nenhum fica vermelho.
//
// Por isso este arquivo NÃO fabrica o evento: ele cria o convite pelo
// repositório REAL e lê o envelope que o outbox gravou.
package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/clock"
	"github.com/Digital-Business-One/dop-core/internal/adapter/notifier"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres/projection"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// mailerEspiao é o único duplo deste arquivo: tudo o mais é real. O canal já
// tem suíte de contrato própria (test/contract/mailer.go); o que se prova aqui
// é o GATILHO, e para isso basta saber o que teria sido enviado.
type mailerEspiao struct {
	mu       sync.Mutex
	enviados []ports.Mail
}

func (m *mailerEspiao) Resolve(context.Context, string) error { return nil }

func (m *mailerEspiao) Send(_ context.Context, mail ports.Mail) (*ports.MailReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enviados = append(m.enviados, mail)
	return &ports.MailReceipt{State: ports.MailSent, Provider: "espiao", Reference: "ref"}, nil
}

func (m *mailerEspiao) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.enviados)
}

func abrirPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("TEST_DATABASE_URL",
		"postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable"))
	if err != nil || pool.Ping(ctx) != nil {
		t.Skipf("Postgres indisponível: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// contaComMembro cria conta, usuário com e-mail VERIFICADO e vínculo.
func contaComMembro(t *testing.T, pool *pgxpool.Pool, prefixo string, verificado bool) (conta, email string) {
	t.Helper()
	ctx := context.Background()
	id := time.Now().UnixNano()
	handle := fmt.Sprintf("%s-%d", prefixo, id)
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
		handle).Scan(&conta); err != nil {
		t.Fatalf("criar conta: %v", err)
	}
	email = fmt.Sprintf("membro-%d@exemplo.test", id)
	var user string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (subject, email, email_verified, name)
		 VALUES ($1,$2,$3,'Membro') RETURNING id`,
		fmt.Sprintf("sub-%d", id), email, verificado).Scan(&user); err != nil {
		t.Fatalf("criar usuário: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (user_id, account_id, role) VALUES ($1,$2,'owner')`,
		user, conta); err != nil {
		t.Fatalf("criar vínculo: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM notification_deliveries WHERE account_id=$1`, conta)
		_, _ = pool.Exec(c, `DELETE FROM attention_items WHERE account_id=$1`, conta)
		_, _ = pool.Exec(c, `DELETE FROM invites WHERE account_id=$1`, conta)
		_, _ = pool.Exec(c, `DELETE FROM accounts WHERE id=$1`, conta)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id=$1`, user)
	})
	return conta, email
}

// ── o gatilho transacional ──────────────────────────────────────────────────

func TestConviteCriadoVIRAEmail(t *testing.T) {
	pool := abrirPool(t)
	ctx := context.Background()
	conta, _ := contaComMembro(t, pool, "notif-convite", true)

	// O convite é criado pelo repositório REAL — é ele quem decide o formato do
	// payload. Se `CreateInvite` deixar de emitir `email`, este teste quebra, e
	// é exatamente o que se quer: em produção o sintoma seria um e-mail que
	// simplesmente não chega.
	convidado := fmt.Sprintf("convidado-%d@exemplo.test", time.Now().UnixNano())
	inv := &identity.Invite{
		AccountID: conta, Email: convidado, Role: identity.RoleDeveloper,
		ExpiresAt: time.Now().Add(72 * time.Hour).UTC(),
	}
	if _, err := postgres.NewIdentityRepo(pool).CreateInvite(ctx, inv); err != nil {
		t.Fatalf("criar convite: %v", err)
	}

	envelope := envelopeDoOutbox(t, pool, conta, "dop.identity.invite.created")

	espiao := &mailerEspiao{}
	svc := notification.NewService(postgres.NewNotificationRepo(pool), espiao,
		clock.NewSystem(), notification.Config{BaseURL: "https://cockpit.test"})
	consumidor := notifier.NewConsumer(svc)

	if err := consumidor.Handle(ctx, ports.Event{Payload: envelope}); err != nil {
		t.Fatalf("consumidor: %v", err)
	}
	if espiao.total() != 1 {
		t.Fatalf("o convite NÃO virou e-mail (%d envios) — a regra está lendo um campo "+
			"que o evento de identidade não tem", espiao.total())
	}
	if to := espiao.enviados[0].To; to != convidado {
		t.Fatalf("e-mail foi para %q, esperava %q", to, convidado)
	}
	if k := espiao.enviados[0].Kind; k != string(notification.KindInvite) {
		t.Fatalf("tipo %q", k)
	}

	// O botão precisa endereçar ESTE convite, não a lista de convites: quem
	// recebe ainda não é usuário e não tem lista para olhar.
	//
	// Esta linha é a única que amarra as três fronteiras que ninguém enxerga de
	// dentro: `CreateInvite` emitir `invite_id` no payload, a regra copiá-lo
	// para os dados do template, e o LinkPath `/convites/{invite_id}` resolvê-lo.
	// Se qualquer uma ceder, o e-mail sai com um botão que leva a lugar nenhum —
	// e nada mais no sistema reclama.
	querLink := "https://cockpit.test/convites/" + inv.ID
	if got := fmt.Sprint(espiao.enviados[0].Data["link"]); got != querLink {
		t.Fatalf("link do e-mail: %q, esperado %q", got, querLink)
	}

	// ── idempotência CONTRA O ÍNDICE ÚNICO, não contra um duplo ─────────────
	for i := 0; i < 3; i++ {
		if err := consumidor.Handle(ctx, ports.Event{Payload: envelope}); err != nil {
			t.Fatalf("reentrega %d: %v", i, err)
		}
	}
	if espiao.total() != 1 {
		t.Fatalf("a reentrega mandou %d e-mails: o índice único (event_id, rule_name, "+
			"action_name) não está segurando", espiao.total())
	}

	var estado, kind, regra, acao string
	var recipients []string
	if err := pool.QueryRow(ctx, `
		SELECT state, kind, rule_name, action_name, recipients
		  FROM notification_deliveries WHERE account_id=$1`, conta).
		Scan(&estado, &kind, &regra, &acao, &recipients); err != nil {
		t.Fatalf("ler o registro: %v", err)
	}
	if estado != string(notification.StateSent) {
		t.Fatalf("estado do registro: %q", estado)
	}
	if regra == "" || acao == "" {
		t.Fatalf("a chave gravada não é composta: regra=%q ação=%q", regra, acao)
	}
	if len(recipients) != 1 || recipients[0] != convidado {
		t.Fatalf("destinatários gravados: %v", recipients)
	}

	// E o envio precisa ter virado EVENTO, na mesma transação do registro: é o
	// que o P-29 vai consumir quando a reação virar dado.
	var eventos int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE account_id=$1 AND type='dop.notification.sent'`,
		conta).Scan(&eventos); err != nil {
		t.Fatalf("contar eventos: %v", err)
	}
	if eventos != 1 {
		t.Fatalf("%d evento(s) dop.notification.sent — estado e evento saem na MESMA "+
			"transação (ADR-0019)", eventos)
	}
}

// ── o aviso de atenção com atraso ───────────────────────────────────────────

func TestResumoSoAvisaOQueSOBREVIVEUAoAtraso(t *testing.T) {
	pool := abrirPool(t)
	ctx := context.Background()
	conta, _ := contaComMembro(t, pool, "notif-resumo", true)
	agora := time.Now().UTC()

	// Três itens, e cada um responde uma pergunta diferente:
	velho := abrirItem(t, pool, conta, "thread-velha", agora.Add(-40*time.Minute))
	_ = abrirItem(t, pool, conta, "thread-nova", agora.Add(-2*time.Minute))
	resolvido := abrirItem(t, pool, conta, "thread-resolvida", agora.Add(-40*time.Minute))
	if _, err := pool.Exec(ctx,
		`UPDATE attention_items SET resolved_at=now() WHERE target_id=$1 AND account_id=$2`,
		"thread-resolvida", conta); err != nil {
		t.Fatalf("resolver item: %v", err)
	}

	espiao := &mailerEspiao{}
	svc := notification.NewService(postgres.NewNotificationRepo(pool), espiao,
		clock.NewSystem(), notification.Config{DigestDelay: 15 * time.Minute})

	// A varredura é a de TODAS as contas — a mesma que o scheduler chama.
	if _, _, err := svc.SweepDigest(ctx); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}

	meus := paraAConta(espiao, conta)
	if len(meus) != 1 {
		t.Fatalf("esperava 1 e-mail para esta conta, veio %d", len(meus))
	}
	itens, _ := meus[0].Data["items"].([]map[string]any)
	if len(itens) != 1 {
		t.Fatalf("o resumo listou %d itens, esperava 1 (só o que sobreviveu ao atraso): %+v",
			len(itens), itens)
	}

	// O registro: uma linha por ITEM, e só do item maduro.
	var chaves []string
	rows, err := pool.Query(ctx,
		`SELECT event_id::text FROM notification_deliveries WHERE account_id=$1`, conta)
	if err != nil {
		t.Fatalf("ler registros: %v", err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		chaves = append(chaves, s)
	}
	rows.Close()
	if len(chaves) != 1 || chaves[0] != velho {
		t.Fatalf("registros: %v (esperava só o evento do item maduro %q); o item novo "+
			"não pode ser marcado como avisado, senão ele nunca vira e-mail",
			chaves, velho)
	}
	_ = resolvido

	// Segunda varredura no mesmo minuto: nada novo.
	antes := espiao.total()
	if _, _, err := svc.SweepDigest(ctx); err != nil {
		t.Fatalf("2ª varredura: %v", err)
	}
	if espiao.total() != antes {
		t.Fatalf("a segunda varredura reavisou: %d → %d", antes, espiao.total())
	}
}

// Conta cujo membro NÃO verificou o e-mail não recebe o resumo — e, mais
// importante, o item dela NÃO fica marcado como avisado.
//
// A escolha é declarada em postgres/notification.go: o resumo carrega título de
// item da caixa (nome de demanda, de PR, de projeto), e mandar isso para um
// endereço que ninguém provou pertencer ao membro é vazar trabalho da conta.
func TestMembroNaoVerificadoNaoRecebeENaoConsomeOItem(t *testing.T) {
	pool := abrirPool(t)
	ctx := context.Background()
	conta, _ := contaComMembro(t, pool, "notif-naoverif", false)
	abrirItem(t, pool, conta, "thread-x", time.Now().UTC().Add(-40*time.Minute))

	espiao := &mailerEspiao{}
	svc := notification.NewService(postgres.NewNotificationRepo(pool), espiao,
		clock.NewSystem(), notification.Config{DigestDelay: 15 * time.Minute})
	if _, _, err := svc.SweepDigest(ctx); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	if n := len(paraAConta(espiao, conta)); n != 0 {
		t.Fatalf("mandou %d e-mail(s) para membro com endereço não verificado", n)
	}
	var registros int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notification_deliveries WHERE account_id=$1`, conta).
		Scan(&registros); err != nil {
		t.Fatalf("contar registros: %v", err)
	}
	if registros != 0 {
		t.Fatalf("%d registro(s) gravado(s) sem ter para quem enviar: no dia em que o "+
			"membro verificar o e-mail, ele começaria em dívida com a própria caixa",
			registros)
	}
}

// ── auxiliares ──────────────────────────────────────────────────────────────

// abrirItem abre um item da caixa pela PROJEÇÃO real, com um evento real, e
// depois recua `opened_at` — recuar é a única forma de testar um atraso de 15
// minutos sem esperar 15 minutos, e é honesto porque a coluna é justamente o
// que o predicado do atraso compara.
func abrirItem(t *testing.T, pool *pgxpool.Pool, conta, thread string, aberto time.Time) string {
	t.Helper()
	ctx := context.Background()
	eventID := uuidDeTeste()
	envelope := mustJSON(map[string]any{
		"id": eventID, "account_id": conta, "aggregate": "demand",
		"aggregate_id": uuidDeTeste(),
		"type":         "dop.demand.thread.blocked",
		"occurred_at":  aberto,
		"payload": map[string]any{
			"thread_id": thread, "question": "Um agente precisa de resposta",
		},
	})
	if err := projection.NewAttention(pool).Handle(ctx, ports.Event{Payload: envelope}); err != nil {
		t.Fatalf("abrir item: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE attention_items SET opened_at=$3 WHERE account_id=$1 AND target_id=$2`,
		conta, thread, aberto); err != nil {
		t.Fatalf("recuar opened_at: %v", err)
	}
	return eventID
}

// uuidDeTeste gera um UUID válido e único. O `uuidFrom` de outbox_test.go só
// serve para entradas hexadecimais curtas, e um id malformado faria estes
// testes falharem no INSERT — falha por motivo errado esconde a falha de
// verdade.
func uuidDeTeste() string {
	n := seqDeTeste.Add(1)
	h := fmt.Sprintf("%016x%016x", time.Now().UnixNano(), n)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

var seqDeTeste atomic.Int64

func envelopeDoOutbox(t *testing.T, pool *pgxpool.Pool, conta, tipo string) []byte {
	t.Helper()
	var payload []byte
	err := pool.QueryRow(context.Background(), `
		SELECT o.payload FROM outbox o
		  JOIN events e ON e.id = o.event_id
		 WHERE e.account_id = $1 AND e.type = $2
		 ORDER BY o.occurred_at DESC LIMIT 1`, conta, tipo).Scan(&payload)
	if err != nil {
		t.Fatalf("o evento %q não chegou ao outbox: %v", tipo, err)
	}
	return payload
}

// paraAConta filtra os envios desta conta: a varredura é global, e o ambiente
// local pode ter resíduo de outra execução. Filtrar aqui evita que este teste
// falhe por causa de dado alheio — ou, pior, passe por causa dele.
func paraAConta(m *mailerEspiao, conta string) []ports.Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ports.Mail
	for _, e := range m.enviados {
		if e.AccountID == conta {
			out = append(out, e)
		}
	}
	return out
}
