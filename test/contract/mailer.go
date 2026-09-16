package contract

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/notification"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ════════════════════════════════════════════════════════════════════════════
// The ports.Mailer port's contract suite.
//
// ADR-0001's discipline: a port with a single adapter is guesswork. The same set
// runs against SendGrid and against SMTP.
//
// ── The subtest that justifies the whole suite ──────────────────────────────
//
// `1_resolves_every_domain_kind`. ADR-0025 wrote down the consequence that
// demands a test: a notification kind may exist in the policy and have no
// template at the provider, and that would fail in SILENCE — the event happens,
// the consumer runs, nobody receives anything. Nothing else in the system turns
// red because of it: there is no error, no log, no metric. Only somebody's inbox
// staying empty.
//
// The list of kinds comes from `notification.Kinds()`, which in turn is DERIVED
// from the rules table. It is the whole chain: adding a line to the policy adds
// a kind, which adds a requirement to EVERY adapter, which fails here until
// somebody gives it a template in both providers.
//
// ── The provider's API is never really called ───────────────────────────────
//
// On the other side of the wire there is a DOUBLE (an httptest.Server for
// SendGrid, a local SMTP server for the other); what is under test is the REAL
// adapter. It is the only combination that proves anything: a double on both
// sides proves the double is consistent with itself, and the adapter against the
// real provider turns the suite into something nobody runs.
// ════════════════════════════════════════════════════════════════════════════

// Inbox is what the double on the other side of the wire collected. The type
// belongs to the SUITE, and not to each runner, so the assertions are the same
// in both adapters.
type Inbox struct {
	mu       sync.Mutex
	msgs     []SentMail
	chamadas int
}

// SentMail is a message that reached the double.
type SentMail struct {
	To      string
	Kind    string
	Subject string
	Body    string
	// ReplyTo is recorded by the doubles that can see it. Not a port guarantee:
	// a channel with no notion of reply-to (SMS) leaves it empty.
	ReplyTo string
}

// Received records a message. Called by the runners' doubles.
func (c *Inbox) Received(m SentMail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	c.chamadas++
}

// Touched records that there was CONTACT, even with no valid message. It is what
// allows proving guarantee 5: a refusal with no I/O.
func (c *Inbox) Touched() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chamadas++
}

func (c *Inbox) Messages() []SentMail {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SentMail, len(c.msgs))
	copy(out, c.msgs)
	return out
}

func (c *Inbox) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chamadas
}

// Failure is the refusal mode the double simulates. There are three because the
// port's guarantee 7 makes three distinctions, and confusing them sends the team
// hunting for a defect in the wrong place.
type Failure string

const (
	// FailureCredential: the provider refuses the credential (401 / 535).
	FailureCredential Failure = "credential"
	// FailureUnavailable: the provider does not answer (503 / connection refused).
	FailureUnavailable Failure = "unavailable"
	// FailureContent: the provider refuses the message (400 / 550).
	FailureContent Failure = "content"
)

// MailerHarness is what each runner supplies: three assemblies of the SAME real
// adapter, against different doubles.
type MailerHarness struct {
	// New assembles the adapter against a healthy double.
	New func(t *testing.T) (ports.Mailer, *Inbox)
	// NewFailing assembles against a double that refuses in the requested way.
	NewFailing func(t *testing.T, f Failure) (ports.Mailer, *Inbox)
	// NewDryRun assembles WITHOUT a credential — the mode in which the adapter
	// prints instead of sending.
	NewDryRun func(t *testing.T) (ports.Mailer, *Inbox)
	// Secret is the credential the doubles ECHO back in the error message. It is
	// how guarantee 4 becomes a test instead of a promise: the provider returns
	// the secret, and the suite requires it not to reach the error.
	Secret string
}

// MailerSuite verifies the ten guarantees documented on the port.
func MailerSuite(t *testing.T, name string, h MailerHarness) {
	t.Run(name, func(t *testing.T) {
		ctx := context.Background()
		kinds := notification.KindNames()
		if len(kinds) == 0 {
			t.Fatal("notification.Kinds() is empty: with no kinds, this suite proves nothing")
		}

		// ── 1. THE GUARANTEE THAT JUSTIFIES THE SUITE ───────────────────────
		t.Run("1_resolves_every_domain_kind", func(t *testing.T) {
			m, _ := h.New(t)
			for _, kind := range kinds {
				if err := m.Resolve(ctx, kind); err != nil {
					t.Errorf("the adapter does NOT resolve the %q notice: %v\n\n"+
						"This would fail in SILENCE in production: the event happens, the "+
						"consumer runs and nobody receives anything. Give this kind a template in this "+
						"fornecedor (ADR-0025).", kind, err)
				}
			}
		})

		t.Run("1b_sends_every_domain_kind", func(t *testing.T) {
			// Resolve and Send have to AGREE (guarantee 2). An adapter whose
			// Resolve says "yes" and whose Send does not find the template would
			// pass the subtest above and keep failing quietly.
			for _, kind := range kinds {
				m, inbox := h.New(t)
				rec, err := m.Send(ctx, ports.Mail{
					AccountID: "acct-1", Kind: kind, To: "someone@example.test",
					ToName: "Somebody", Data: sampleData(),
				})
				if err != nil {
					t.Errorf("Send of the %q notice failed against the healthy double: %v", kind, err)
					continue
				}
				if rec == nil || rec.State != ports.MailSent {
					t.Errorf("notice %q: expected State=%q, got %+v", kind, ports.MailSent, rec)
					continue
				}
				if rec.Provider == "" {
					t.Errorf("notice %q: empty Provider — the record would not say who sent it", kind)
				}
				msgs := inbox.Messages()
				if len(msgs) != 1 {
					t.Errorf("notice %q: the double received %d message(s), expected 1", kind, len(msgs))
					continue
				}
				if msgs[0].Kind != kind {
					t.Errorf("the %q notice reached the provider as %q — a swapped template is "+
						"worse than a missing one: somebody receives the wrong message",
						kind, msgs[0].Kind)
				}
				if strings.TrimSpace(msgs[0].Subject) == "" {
					t.Errorf("notice %q arrived WITH NO subject", kind)
				}
			}
		})

		// ── 2. a kind outside the index is KindNotFound, not a silent success ─
		t.Run("2_an_unknown_kind_is_notfound", func(t *testing.T) {
			m, inbox := h.New(t)
			const invented = "a-kind-that-never-existed"

			if err := m.Resolve(ctx, invented); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("Resolve of an unknown kind: expected KindNotFound, got %v (%v)",
					errs.KindOf(err), err)
			}
			_, err := m.Send(ctx, ports.Mail{
				AccountID: "acct-1", Kind: invented, To: "someone@example.test",
			})
			if errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("Send of an unknown kind: expected KindNotFound, got %v (%v)",
					errs.KindOf(err), err)
			}
			if inbox.Calls() != 0 {
				t.Fatalf("an unknown kind actually touched the provider (%d call(s)): "+
					"the resolution happens BEFORE the I/O", inbox.Calls())
			}
		})

		// ── 3. ensaio local ─────────────────────────────────────────────────
		t.Run("3_the_local_dry_run_prints_and_does_not_send", func(t *testing.T) {
			m, inbox := h.NewDryRun(t)
			rec, err := m.Send(ctx, ports.Mail{
				AccountID: "acct-1", Kind: kinds[0], To: "someone@example.test",
				Data: sampleData(),
			})
			if err != nil {
				t.Fatalf("the dry run should work with no credential: %v", err)
			}
			if rec.State != ports.MailSentLocal {
				t.Fatalf("dry run: expected State=%q, got %q — the difference between "+
					"'we notified' and 'we pretended to notify' must not depend on whoever "+
					"reads the log remembering which environment it ran in", ports.MailSentLocal, rec.State)
			}
			if inbox.Calls() != 0 {
				t.Fatalf("the DRY RUN talked to the provider (%d call(s))", inbox.Calls())
			}
		})

		t.Run("3b_the_dry_run_still_resolves_the_template", func(t *testing.T) {
			// The easiest subtest to forget, and the one that decides whether
			// guarantee 1 holds where it is exercised. Every developer machine
			// and every CI runs with no key; if the dry run skipped the
			// resolution, a kind with no template would pass EVERYWHERE and only
			// break in production.
			m, _ := h.NewDryRun(t)
			if err := m.Resolve(ctx, "a-kind-that-never-existed"); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("dry run: Resolve of an unknown kind should be KindNotFound, got %v", err)
			}
			_, err := m.Send(ctx, ports.Mail{
				AccountID: "acct-1", Kind: "a-kind-that-never-existed", To: "someone@example.test",
			})
			if errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("dry run: Send of an unknown kind should be KindNotFound, got %v", err)
			}
			for _, kind := range kinds {
				if err := m.Resolve(ctx, kind); err != nil {
					t.Errorf("dry run: the %q notice does not resolve: %v", kind, err)
				}
			}
		})

		// ── 4. the secret does not get out ──────────────────────────────────
		t.Run("4_the_secret_does_not_leak", func(t *testing.T) {
			if h.Secret == "" {
				t.Fatal("the runner has to provide the Secret for this guarantee to hold")
			}
			m, _ := h.New(t)

			// 4a. the adapter's formatting. `%#v` is on the list, and it is what
			// this suite did NOT check until this session: a String() with a
			// value receiver swallows `%v` and `%+v`, so keeping the key in a
			// field went unnoticed — and `%#v` (which ignores String()) printed
			// it whole. Verified with a deliberately sabotaged adapter.
			for _, s := range []string{
				fmt.Sprintf("%v", m), fmt.Sprintf("%+v", m), fmt.Sprintf("%#v", m),
				fmt.Sprintf("%v", safeDeref(m)), fmt.Sprintf("%+v", safeDeref(m)),
				fmt.Sprintf("%#v", safeDeref(m)),
			} {
				if strings.Contains(s, h.Secret) {
					t.Fatalf("THE CREDENTIAL LEAKED in the adapter's formatting: %s", s)
				}
			}

			// 4d. STRUCTURAL, and this is the real guarantee.
			//
			// Checking the formatting checks a SYMPTOM: as long as a String()
			// with a value receiver exists, the field with the key stays hidden
			// from `%v` — and the test approves an adapter that keeps the
			// secret. The day somebody renames the String(), embeds the adapter
			// inside another struct, or a panic prints `%#v`, the key comes out.
			//
			// Here the question is different: IS the secret in some field? The
			// port says it must not be — it is captured in a closure, and
			// reflection does not open a closure. "There is no key to log" is a
			// property of the structure, and that is how it is verified.
			if path := fieldWithSecret(reflect.ValueOf(m), h.Secret, "adaptador", 0); path != "" {
				t.Fatalf("THE CREDENTIAL is kept in %s.\n\n"+
					"Today it does not show up in `%%v` only because a String() with a "+
					"value receiver exists — that is protection that depends on discipline. "+
					"Capture the secret in a CLOSURE (as `authorize`/`authenticate` do): a "+
					"closure prints as an address, and there is nothing to leak.", path)
			}

			// 4b. the error message, with the provider ECHOING the secret back.
			falho, _ := h.NewFailing(t, FailureUnavailable)
			_, err := falho.Send(ctx, ports.Mail{
				AccountID: "acct-1", Kind: kinds[0], To: "someone@example.test",
				Data: sampleData(),
			})
			if err == nil {
				t.Fatal("the unavailable double should have made the send fail")
			}
			if strings.Contains(err.Error(), h.Secret) {
				t.Fatalf("THE CREDENTIAL LEAKED in the error message: %v", err)
			}

			// 4c. the same through the refused-authentication path, which is the
			// other place where the provider has the secret in hand.
			semCred, _ := h.NewFailing(t, FailureCredential)
			_, err = semCred.Send(ctx, ports.Mail{
				AccountID: "acct-1", Kind: kinds[0], To: "someone@example.test",
				Data: sampleData(),
			})
			if err == nil {
				t.Fatal("the double that refuses the credential should have made the send fail")
			}
			if strings.Contains(err.Error(), h.Secret) {
				t.Fatalf("THE CREDENTIAL LEAKED in the authentication refusal: %v", err)
			}
		})

		// ── 5. a refusal with no I/O ────────────────────────────────────────
		t.Run("5_an_invalid_request_costs_no_io", func(t *testing.T) {
			casos := []struct {
				name string
				mail ports.Mail
			}{
				{"empty recipient", ports.Mail{Kind: kinds[0], To: ""}},
				{"recipient with no at sign", ports.Mail{Kind: kinds[0], To: "someone"}},
				{"recipient with a space", ports.Mail{Kind: kinds[0], To: "a b@c.test"}},
				{"arroba no fim", ports.Mail{Kind: kinds[0], To: "fulano@"}},
				{"empty kind", ports.Mail{Kind: "", To: "someone@example.test"}},
			}
			for _, c := range casos {
				m, inbox := h.New(t)
				_, err := m.Send(ctx, c.mail)
				if errs.KindOf(err) != errs.KindInvalid {
					t.Errorf("%s: expected KindInvalid, got %v (%v)", c.name, errs.KindOf(err), err)
				}
				if inbox.Calls() != 0 {
					t.Errorf("%s: it cost %d round trip(s) to the provider — calling with no "+
						"address must not cost a call", c.name, inbox.Calls())
				}
			}
		})

		// ── 6/7. error translation ──────────────────────────────────────────
		t.Run("7_errors_translated_by_nature", func(t *testing.T) {
			casos := []struct {
				failure Failure
				want    errs.Kind
				why     string
			}{
				{FailureCredential, errs.KindUnauthorized,
					"a refused credential is not an unavailability: it sends the team hunting the network when the problem is a password"},
				{FailureUnavailable, errs.KindUnavailable,
					"a provider that is down is not the caller's error, and resending later makes sense"},
				{FailureContent, errs.KindInvalid,
					"refused content or address is permanent: resending the same gives the same result"},
			}
			for _, c := range casos {
				m, _ := h.NewFailing(t, c.failure)
				_, err := m.Send(ctx, ports.Mail{
					AccountID: "acct-1", Kind: kinds[0], To: "someone@example.test",
					Data: sampleData(),
				})
				if got := errs.KindOf(err); got != c.want {
					t.Errorf("failure %q: expected %v, got %v (%v) — %s",
						c.failure, c.want, got, err, c.why)
				}
			}
		})

		// ── 10. concurrency ─────────────────────────────────────────────────
		t.Run("10_safe_for_concurrent_use", func(t *testing.T) {
			m, inbox := h.New(t)
			const n = 8
			var wg sync.WaitGroup
			errCh := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, err := m.Send(ctx, ports.Mail{
						AccountID: "acct-1", Kind: kinds[i%len(kinds)],
						To:   fmt.Sprintf("dest-%d@example.test", i),
						Data: sampleData(),
					})
					errCh <- err
				}(i)
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				if err != nil {
					t.Fatalf("a concurrent send failed: %v", err)
				}
			}
			if got := len(inbox.Messages()); got != n {
				t.Fatalf("the double received %d messages, expected %d", got, n)
			}
		})

		// ── 9. Send does not mutate the caller's Data ───────────────────────
		t.Run("9_send_does_not_mutate_the_callers_data", func(t *testing.T) {
			// The digest reuses the SAME map for every recipient. An adapter that
			// wrote into it (SendGrid injects the subject) would contaminate the
			// second send — and the defect would only show up in an account with
			// more than one member.
			m, _ := h.New(t)
			data := sampleData()
			before := len(data)
			for i := 0; i < 2; i++ {
				if _, err := m.Send(ctx, ports.Mail{
					AccountID: "acct-1", Kind: kinds[0],
					To: fmt.Sprintf("dest-%d@example.test", i), Data: data,
				}); err != nil {
					t.Fatalf("envio %d: %v", i, err)
				}
			}
			if len(data) != before {
				t.Fatalf("the adapter MUTATED the caller's Data: %d keys became %d",
					before, len(data))
			}
		})
	})
}

// sampleData is a payload that serves EVERY kind: the digest needs `items` and
// `total`, the invite needs `email` and `role`. A spare key is not an error
// (guarantee 8), and that is what allows a single payload.
func sampleData() map[string]any {
	return map[string]any{
		"account_id": "acct-1",
		"email":      "invitee@example.test",
		"role":       "member",
		"link":       "https://cockpit.example.test/attention",
		"total":      2,
		"items": []map[string]any{
			{"kind": "thread_blocked", "title": "An agent needs an answer", "summary": "thread 7"},
			{"kind": "pr_review", "title": "A PR awaiting review", "summary": ""},
		},
	}
}

// fieldWithSecret looks for the secret in any reachable field, and returns the
// path to it.
//
// `reflect.Value.String()` READS an unexported field (only `Interface()` panics),
// and that is what makes this check possible without `unsafe`. Closures are not
// walkable — which is exactly the point: what is captured in a closure is
// reachable neither from here, nor by fmt, nor by a debugger's dump.
func fieldWithSecret(v reflect.Value, secret, path string, depth int) string {
	// A depth cap: the adapter carries an http.Client, which carries a
	// Transport, which carries the world. The secret, if it is kept, is near the
	// surface.
	if depth > 6 || !v.IsValid() || secret == "" {
		return ""
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return fieldWithSecret(v.Elem(), secret, path, depth+1)
	case reflect.String:
		if strings.Contains(v.String(), secret) {
			return path
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			name := v.Type().Field(i).Name
			if achado := fieldWithSecret(v.Field(i), secret, path+"."+name, depth+1); achado != "" {
				return achado
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return ""
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			// []byte is the other obvious way of keeping a credential.
			if b, ok := asBytes(v); ok && strings.Contains(string(b), secret) {
				return path
			}
			return ""
		}
		for i := 0; i < v.Len(); i++ {
			if achado := fieldWithSecret(v.Index(i), secret, fmt.Sprintf("%s[%d]", path, i), depth+1); achado != "" {
				return achado
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return ""
		}
		for _, k := range v.MapKeys() {
			if achado := fieldWithSecret(v.MapIndex(k), secret, fmt.Sprintf("%s[%v]", path, k), depth+1); achado != "" {
				return achado
			}
		}
	}
	return ""
}

// asBytes reads a []byte even when unexported, byte by byte — `Bytes()` refuses
// a value obtained from an unexported field, `Index(i).Uint()` does not.
func asBytes(v reflect.Value) ([]byte, bool) {
	out := make([]byte, v.Len())
	for i := range out {
		out[i] = byte(v.Index(i).Uint())
	}
	return out, true
}

// safeDeref returns the VALUE pointed at, when the adapter is a pointer.
//
// It exists because `%+v` of a pointer with a value-receiver String() prints the
// String(); what exposes the fields is `%+v` of the VALUE. Without this,
// guarantee 4 would be verified on the path that is easiest to pass.
func safeDeref(m ports.Mailer) any {
	v := reflect.ValueOf(m)
	if v.Kind() == reflect.Ptr && !v.IsNil() {
		return v.Elem().Interface()
	}
	return m
}
