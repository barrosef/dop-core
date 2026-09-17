package contract

// The ports.SMSer contract suite (ADR-0020 §4).
//
// It is what makes the port real: two adapters, and everything the domain may
// assume proven in BOTH. What is not here — a delivery status, the sender, the
// message parts — is out of the port on purpose, and this file is where that
// stays true.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Outbox is what the double received. It is the suite's only way of seeing what
// left — asserting against the adapter's return would prove the adapter agrees
// with itself.
type Outbox struct {
	mu   sync.Mutex
	sent []SentSMS
	// calls counts EVERY arrival, including refused ones: it is what proves the
	// dry run did not talk to the provider.
	calls int
}

type SentSMS struct {
	To, Text, Auth string
}

func (o *Outbox) Received(s SentSMS) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sent = append(o.sent, s)
	o.calls++
}

func (o *Outbox) Touched() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
}

func (o *Outbox) Messages() []SentSMS {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]SentSMS{}, o.sent...)
}

func (o *Outbox) Calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

// SMSerHarness is what each adapter's test provides.
type SMSerHarness struct {
	// Secret is the credential the double echoes back in the refusal. The suite
	// hunts for it in every error and in the adapter's own reflection.
	Secret     string
	New        func(t *testing.T) (ports.SMSer, *Outbox)
	NewFailing func(t *testing.T, f Failure) (ports.SMSer, *Outbox)
	NewDryRun  func(t *testing.T) (ports.SMSer, *Outbox)
}

const smsText = "DOP: 123456 is your verification code. It expires in 10 minutes."

func SMSerSuite(t *testing.T, name string, h SMSerHarness) {
	t.Helper()
	ctx := context.Background()

	t.Run(name+"/1_it_sends_and_the_state_says_it_really_went", func(t *testing.T) {
		s, out := h.New(t)
		r, err := s.Send(ctx, ports.SMS{To: "+5511999999999", Text: smsText})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		// Guarantee 4: never empty. An empty state becomes a record that does
		// not say whether anybody received anything.
		if r.State != ports.SMSSent {
			t.Errorf("State = %q, want %q", r.State, ports.SMSSent)
		}
		if r.Provider == "" {
			t.Error("Provider is empty: 'it never arrived' is a different investigation per carrier")
		}
		msgs := out.Messages()
		if len(msgs) != 1 {
			t.Fatalf("%d messages reached the provider", len(msgs))
		}
		if msgs[0].Text != smsText {
			t.Errorf("the text arrived changed: %q", msgs[0].Text)
		}
		// The destination arrives in the form the provider expects — with or
		// without the "+" — and what matters to the port is that the DIGITS
		// survive.
		if !strings.Contains(digitsOf(msgs[0].To), "5511999999999") {
			t.Errorf("the destination arrived as %q", msgs[0].To)
		}
	})

	t.Run(name+"/2_a_broken_destination_costs_no_round_trip", func(t *testing.T) {
		// Guarantee 1. With SMS this is not only about latency: every send is
		// money, and a caller with a broken number must not spend it.
		for _, to := range []string{"", "5511999999999", "+55 11 99999-9999", "+551199", "+abcdefghij"} {
			s, out := h.New(t)
			_, err := s.Send(ctx, ports.SMS{To: to, Text: smsText})
			if errs.KindOf(err) != errs.KindInvalid {
				t.Errorf("To=%q gave %v (%s), want invalid", to, err, errs.KindOf(err))
			}
			if out.Calls() != 0 {
				t.Errorf("To=%q reached the provider", to)
			}
		}
	})

	t.Run(name+"/3_an_empty_text_is_refused_with_no_io", func(t *testing.T) {
		s, out := h.New(t)
		_, err := s.Send(ctx, ports.SMS{To: "+5511999999999", Text: ""})
		if errs.KindOf(err) != errs.KindInvalid {
			t.Errorf("empty text gave %v", err)
		}
		if out.Calls() != 0 {
			t.Error("an empty text reached the provider")
		}
	})

	t.Run(name+"/4_with_no_credential_it_rehearses_and_does_not_talk_to_the_provider", func(t *testing.T) {
		// Guarantee 2: the local environment's only mode. The difference
		// between "we sent" and "we pretended to" cannot depend on whoever
		// reads the log remembering the environment.
		s, out := h.NewDryRun(t)
		r, err := s.Send(ctx, ports.SMS{To: "+5511999999999", Text: smsText})
		if err != nil {
			t.Fatalf("the rehearsal failed: %v", err)
		}
		if r.State != ports.SMSSentLocal {
			t.Errorf("State = %q, want %q", r.State, ports.SMSSentLocal)
		}
		if out.Calls() != 0 {
			t.Error("the rehearsal talked to the provider")
		}
	})

	t.Run(name+"/5_the_error_translation_is_the_houses", func(t *testing.T) {
		// Guarantee 5. The distinction decides whether the caller retries the
		// code or tells the person to fix the number.
		for _, c := range []struct {
			failure Failure
			want    errs.Kind
		}{
			{FailureCredential, errs.KindUnauthorized},
			{FailureUnavailable, errs.KindUnavailable},
			{FailureContent, errs.KindInvalid},
		} {
			s, _ := h.NewFailing(t, c.failure)
			_, err := s.Send(ctx, ports.SMS{To: "+5511999999999", Text: smsText})
			if got := errs.KindOf(err); got != c.want {
				t.Errorf("%s gave %s, want %s (%v)", c.failure, got, c.want, err)
			}
		}
	})

	t.Run(name+"/6_the_secret_does_not_get_out", func(t *testing.T) {
		// Guarantee 3. Not a promise of discipline: the credential is captured
		// in a CLOSURE, and this test is what proves it — fmt reads unexported
		// fields by reflection and cannot call their String().
		if h.Secret == "" {
			t.Skip("the harness declares no secret")
		}
		s, _ := h.NewFailing(t, FailureCredential)
		_, err := s.Send(ctx, ports.SMS{To: "+5511999999999", Text: smsText})
		if err != nil && strings.Contains(err.Error(), h.Secret) {
			t.Errorf("the credential is in the error: %v", err)
		}
		if dump := fmt.Sprintf("%+v", s); strings.Contains(dump, h.Secret) {
			t.Errorf("the credential is in a %%+v of the adapter")
		}
		if path := fieldWithSecret(reflect.ValueOf(s), h.Secret, "", 0); path != "" {
			t.Errorf("the credential is in the field %s — it has to live in a closure", path)
		}
	})

	t.Run(name+"/7_the_text_does_not_reach_the_error", func(t *testing.T) {
		// Guarantee 7. The text carries the one-time code, and a code in a log
		// is a live credential at rest for as long as it is valid.
		s, _ := h.NewFailing(t, FailureContent)
		_, err := s.Send(ctx, ports.SMS{To: "+5511999999999", Text: smsText})
		if err == nil {
			t.Fatal("the failure did not produce an error")
		}
		if strings.Contains(err.Error(), "123456") {
			t.Errorf("the code is in the error message: %v", err)
		}
	})

	t.Run(name+"/8_concurrent_sends_do_not_race", func(t *testing.T) {
		// Guarantee 8. Run it with -race; without it, it only proves nothing
		// panics.
		s, out := h.New(t)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, _ = s.Send(ctx, ports.SMS{
					To:   fmt.Sprintf("+55119999999%02d", i),
					Text: smsText,
				})
			}(i)
		}
		wg.Wait()
		if got := len(out.Messages()); got != 8 {
			t.Errorf("%d messages arrived out of 8", got)
		}
	})
}

func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
