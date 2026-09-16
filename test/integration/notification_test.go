//go:build integration

// The test that crosses the event spine for real.
//
// The reason is the same as attention_test.go's, and the lesson comes from a bug
// that already happened in this house: the attention box read `status` on an
// event the domain emitted with `to`. The unit tests on both sides passed,
// because each used the shape it ASSUMED of the other.
//
// Communication has exactly the same exposure, and worse: the symptom is the
// ABSENCE of an e-mail. If the rule reads `email` on a payload the identity
// domain emits under another name, `Apply` returns zero commands, the consumer
// returns nil, JetStream acks, and nothing anywhere goes red.
//
// That is why this file does NOT fabricate the event: it creates the invite
// through the REAL repository and reads the envelope the outbox wrote.
package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/adapter/clock"
	"github.com/barrosef/dop-core/internal/adapter/notifier"
	"github.com/barrosef/dop-core/internal/adapter/postgres"
	"github.com/barrosef/dop-core/internal/adapter/postgres/projection"
	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
)

// spyMailer is this file's only double: everything else is real. The channel
// already has its own contract suite (test/contract/mailer.go); what is proven
// here is the TRIGGER, and for that it is enough to know what would have been
// sent.
type spyMailer struct {
	mu   sync.Mutex
	sent []ports.Mail
}

func (m *spyMailer) Resolve(context.Context, string) error { return nil }

func (m *spyMailer) Send(_ context.Context, mail ports.Mail) (*ports.MailReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, mail)
	return &ports.MailReceipt{State: ports.MailSent, Provider: "spy", Reference: "ref"}, nil
}

func (m *spyMailer) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// poolWithCleanup is outbox_test.go's openPool plus the automatic close: these
// tests create rows and need the pool alive until the very last t.Cleanup.
func poolWithCleanup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("TEST_DATABASE_URL",
		"postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable"))
	if err != nil || pool.Ping(ctx) != nil {
		t.Skipf("Postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// accountWithMember creates an account, a user with a VERIFIED e-mail and the
// membership.
func accountWithMember(t *testing.T, pool *pgxpool.Pool, prefix string, verified bool) (account, email string) {
	t.Helper()
	ctx := context.Background()
	id := time.Now().UnixNano()
	handle := fmt.Sprintf("%s-%d", prefix, id)
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
		handle).Scan(&account); err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	email = fmt.Sprintf("member-%d@example.test", id)
	var user string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (subject, email, email_verified, name)
		 VALUES ($1,$2,$3,'Member') RETURNING id`,
		fmt.Sprintf("sub-%d", id), email, verified).Scan(&user); err != nil {
		t.Fatalf("creating the user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (user_id, account_id, role) VALUES ($1,$2,'owner')`,
		user, account); err != nil {
		t.Fatalf("creating the membership: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM notification_deliveries WHERE account_id=$1`, account)
		_, _ = pool.Exec(c, `DELETE FROM attention_items WHERE account_id=$1`, account)
		_, _ = pool.Exec(c, `DELETE FROM invites WHERE account_id=$1`, account)
		_, _ = pool.Exec(c, `DELETE FROM accounts WHERE id=$1`, account)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id=$1`, user)
	})
	return account, email
}

// ── the transactional trigger ───────────────────────────────────────────────

func TestAnInviteCreatedBECOMESAnEmail(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	account, _ := accountWithMember(t, pool, "notif-invite", true)

	// The invite is created through the REAL repository — it is the one that
	// decides the payload's shape. If `CreateInvite` stops emitting `email`,
	// this test breaks, and that is exactly what we want: in production the
	// symptom would be an e-mail that simply does not arrive.
	invitee := fmt.Sprintf("invitee-%d@example.test", time.Now().UnixNano())
	inv := &identity.Invite{
		AccountID: account, Email: invitee, Role: identity.RoleDeveloper,
		ExpiresAt: time.Now().Add(72 * time.Hour).UTC(),
	}
	if _, err := postgres.NewIdentityRepo(pool).CreateInvite(ctx, inv); err != nil {
		t.Fatalf("creating the invite: %v", err)
	}

	envelope := envelopeFromOutbox(t, pool, account, "dop.identity.invite.created")

	spy := &spyMailer{}
	svc := notification.NewService(postgres.NewNotificationRepo(pool), spy,
		clock.NewSystem(), notification.Config{BaseURL: "https://cockpit.test"})
	consumer := notifier.NewConsumer(svc)

	if err := consumer.Handle(ctx, ports.Event{Payload: envelope}); err != nil {
		t.Fatalf("consumer: %v", err)
	}
	if spy.total() != 1 {
		t.Fatalf("the invite did NOT become an e-mail (%d sends) — the rule is reading a "+
			"field the identity event does not have", spy.total())
	}
	if to := spy.sent[0].To; to != invitee {
		t.Fatalf("the e-mail went to %q, expected %q", to, invitee)
	}
	if k := spy.sent[0].Kind; k != string(notification.KindInvite) {
		t.Fatalf("kind %q", k)
	}

	// The button has to address THIS invite, not the invite list: whoever
	// receives it is not a user yet and has no list to look at.
	//
	// This line is the only one binding the three boundaries nobody sees from
	// the inside: `CreateInvite` emitting `invite_id` in the payload, the rule
	// copying it into the template's data, and the LinkPath `/invites/{invite_id}`
	// resolving it. If any of them gives, the e-mail goes out with a button that
	// leads nowhere — and nothing else in the system complains.
	wantLink := "https://cockpit.test/invites/" + inv.ID
	if got := fmt.Sprint(spy.sent[0].Data["link"]); got != wantLink {
		t.Fatalf("the e-mail's link: %q, expected %q", got, wantLink)
	}

	// ── idempotency AGAINST THE UNIQUE INDEX, not against a double ──────────
	for i := 0; i < 3; i++ {
		if err := consumer.Handle(ctx, ports.Event{Payload: envelope}); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
	}
	if spy.total() != 1 {
		t.Fatalf("the redelivery sent %d e-mails: the unique index (event_id, rule_name, "+
			"action_name) is not holding", spy.total())
	}

	var state, kind, rule, action string
	var recipients []string
	if err := pool.QueryRow(ctx, `
		SELECT state, kind, rule_name, action_name, recipients
		  FROM notification_deliveries WHERE account_id=$1`, account).
		Scan(&state, &kind, &rule, &action, &recipients); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if state != string(notification.StateSent) {
		t.Fatalf("the record's state: %q", state)
	}
	if rule == "" || action == "" {
		t.Fatalf("the stored key is not composite: rule=%q action=%q", rule, action)
	}
	if len(recipients) != 1 || recipients[0] != invitee {
		t.Fatalf("stored recipients: %v", recipients)
	}

	// And the send has to have become an EVENT, in the same transaction as the
	// record: it is what P-29 will consume when the reaction becomes data.
	var events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE account_id=$1 AND type='dop.notification.sent'`,
		account).Scan(&events); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if events != 1 {
		t.Fatalf("%d dop.notification.sent event(s) — state and event go out in the SAME "+
			"transaction (ADR-0019)", events)
	}
}

// ── the delayed attention digest ────────────────────────────────────────────

func TestTheDigestOnlyReportsWhatSURVIVEDTheDelay(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	account, _ := accountWithMember(t, pool, "notif-digest", true)
	now := time.Now().UTC()

	// Three items, and each answers a different question:
	old := openItem(t, pool, account, "old-thread", now.Add(-40*time.Minute))
	_ = openItem(t, pool, account, "new-thread", now.Add(-2*time.Minute))
	resolved := openItem(t, pool, account, "resolved-thread", now.Add(-40*time.Minute))
	if _, err := pool.Exec(ctx,
		`UPDATE attention_items SET resolved_at=now() WHERE target_id=$1 AND account_id=$2`,
		"resolved-thread", account); err != nil {
		t.Fatalf("resolving the item: %v", err)
	}

	spy := &spyMailer{}
	svc := notification.NewService(postgres.NewNotificationRepo(pool), spy,
		clock.NewSystem(), notification.Config{DigestDelay: 15 * time.Minute})

	// The sweep is the one over ALL accounts — the same one the scheduler calls.
	if _, _, err := svc.SweepDigest(ctx); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}

	mine := forTheAccount(spy, account)
	if len(mine) != 1 {
		t.Fatalf("expected 1 e-mail for this account, got %d", len(mine))
	}
	items, _ := mine[0].Data["items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("the digest listed %d items, expected 1 (only what survived the delay): %+v",
			len(items), items)
	}

	// The record: one row per ITEM, and only for the mature item.
	var keys []string
	rows, err := pool.Query(ctx,
		`SELECT event_id::text FROM notification_deliveries WHERE account_id=$1`, account)
	if err != nil {
		t.Fatalf("reading the records: %v", err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		keys = append(keys, s)
	}
	rows.Close()
	if len(keys) != 1 || keys[0] != old {
		t.Fatalf("records: %v (expected only the mature item's event %q); the new item "+
			"must not be marked as reported, or it would never become an e-mail",
			keys, old)
	}
	_ = resolved

	// A second sweep in the same minute: nothing new.
	before := spy.total()
	if _, _, err := svc.SweepDigest(ctx); err != nil {
		t.Fatalf("2nd sweep: %v", err)
	}
	if spy.total() != before {
		t.Fatalf("the second sweep reported again: %d → %d", before, spy.total())
	}
}

// An account whose member did NOT verify their e-mail does not get the digest —
// and, more importantly, their item is NOT marked as reported.
//
// The choice is declared in postgres/notification.go: the digest carries the
// title of an item from the box (a demand's, a PR's, a project's name), and
// sending that to an address nobody proved belongs to the member is leaking the
// account's work.
func TestAnUnverifiedMemberNeitherReceivesNorConsumesTheItem(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	account, _ := accountWithMember(t, pool, "notif-unverified", false)
	openItem(t, pool, account, "thread-x", time.Now().UTC().Add(-40*time.Minute))

	spy := &spyMailer{}
	svc := notification.NewService(postgres.NewNotificationRepo(pool), spy,
		clock.NewSystem(), notification.Config{DigestDelay: 15 * time.Minute})
	if _, _, err := svc.SweepDigest(ctx); err != nil {
		t.Fatalf("SweepDigest: %v", err)
	}
	if n := len(forTheAccount(spy, account)); n != 0 {
		t.Fatalf("sent %d e-mail(s) to a member with an unverified address", n)
	}
	var records int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notification_deliveries WHERE account_id=$1`, account).
		Scan(&records); err != nil {
		t.Fatalf("counting the records: %v", err)
	}
	if records != 0 {
		t.Fatalf("%d record(s) written with nobody to send to: on the day the member "+
			"verifies their e-mail, they would start out in debt to their own box",
			records)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// openItem opens an item in the box through the real PROJECTION, with a real
// event, and then moves `opened_at` back — moving it back is the only way to
// test a 15-minute delay without waiting 15 minutes, and it is honest because
// that column is exactly what the delay's predicate compares.
func openItem(t *testing.T, pool *pgxpool.Pool, account, thread string, openedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	eventID := testUUID()
	envelope := mustJSON(map[string]any{
		"id": eventID, "account_id": account, "aggregate": "demand",
		"aggregate_id": testUUID(),
		"type":         "dop.demand.thread.blocked",
		"occurred_at":  openedAt,
		"payload": map[string]any{
			"thread_id": thread, "question": "An agent needs an answer",
		},
	})
	if err := projection.NewAttention(pool).Handle(ctx, ports.Event{Payload: envelope}); err != nil {
		t.Fatalf("opening the item: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE attention_items SET opened_at=$3 WHERE account_id=$1 AND target_id=$2`,
		account, thread, openedAt); err != nil {
		t.Fatalf("moving opened_at back: %v", err)
	}
	return eventID
}

// testUUID generates a valid, unique UUID. outbox_test.go's `uuidFrom` only
// works for short hexadecimal inputs, and a malformed id would make these tests
// fail at the INSERT — failing for the wrong reason hides the real failure.
func testUUID() string {
	n := testSeq.Add(1)
	h := fmt.Sprintf("%016x%016x", time.Now().UnixNano(), n)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

var testSeq atomic.Int64

func envelopeFromOutbox(t *testing.T, pool *pgxpool.Pool, account, kind string) []byte {
	t.Helper()
	var payload []byte
	err := pool.QueryRow(context.Background(), `
		SELECT o.payload FROM outbox o
		  JOIN events e ON e.id = o.event_id
		 WHERE e.account_id = $1 AND e.type = $2
		 ORDER BY o.occurred_at DESC LIMIT 1`, account, kind).Scan(&payload)
	if err != nil {
		t.Fatalf("the event %q did not reach the outbox: %v", kind, err)
	}
	return payload
}

// forTheAccount filters this account's sends: the sweep is global, and the local
// environment may carry residue from another run. Filtering here keeps this test
// from failing because of somebody else's data — or, worse, passing because of
// it.
func forTheAccount(m *spyMailer, account string) []ports.Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ports.Mail
	for _, e := range m.sent {
		if e.AccountID == account {
			out = append(out, e)
		}
	}
	return out
}
