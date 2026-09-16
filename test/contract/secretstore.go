// Package contract carries the ports' CONTRACT tests.
//
// ADR-0001's discipline: a port with a single adapter is guesswork. The same set
// runs against EVERY adapter — in-memory, k8s, GCP Secret Manager — and it is
// what guarantees substitutability in fact, not in intention.
package contract

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
)

// SecretStoreSuite verifies the six guarantees documented on the port.
// refSeq guarantees uniqueness even within the same nanosecond.
var refSeq atomic.Int64

func SecretStoreSuite(t *testing.T, name string, newStore func(t *testing.T) ports.SecretStore) {
	t.Run(name, func(t *testing.T) {
		// UNIQUE accounts per run, and not fixed literals.
		//
		// The first version used fixed "acct-a"/"acct-b", and the suite passed —
		// against the in-memory double, where `newStore` returns a fresh vault
		// on every subtest. Against a REAL backend, `newStore` returns a new
		// client for the SAME vault, and the secret written in subtest 1 made
		// the `Exists` subtest fail by finding what it had left behind itself.
		//
		// It was the suite written on top of the double: it verified the port,
		// but carried along an assumption only the double satisfied. No real
		// adapter would pass — and none was being run, which closed the circle.
		//
		// Cleaning up at the end would not be enough: a subtest that fails
		// midway leaves residue, and the next run would fail because of the
		// previous one.
		id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), refSeq.Add(1))
		refA := ports.SecretRef{AccountID: "acct-a-" + id, Kind: "integration_credential", OwnerID: "res-1"}
		refB := ports.SecretRef{AccountID: "acct-b-" + id, Kind: "integration_credential", OwnerID: "res-1"}
		val := ports.SecretValue("super-secret-token")

		// The cleanup grabs the vault NOW, not at the end: `newStore` may skip
		// the suite when the infrastructure does not answer, and skipping inside
		// a Cleanup would mark the test as SKIP after every subtest had PASSED —
		// an output that lies about what happened.
		cleanup := newStore(t)
		t.Cleanup(func() {
			_ = cleanup.Delete(context.Background(), refA)
			_ = cleanup.Delete(context.Background(), refB)
		})

		t.Run("1_immediate_read_after_write", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			if err := s.Put(ctx, refA, val); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := s.Get(ctx, refA)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(got, val) {
				t.Fatalf("valor divergente: %q != %q", got, val)
			}
		})

		t.Run("2_absent_returns_nil_with_no_error", func(t *testing.T) {
			s := newStore(t)
			got, err := s.Get(context.Background(),
				ports.SecretRef{AccountID: "acct-x", Kind: "integration_credential", OwnerID: "does-not-exist"})
			if err != nil {
				t.Fatalf("expected nil with no error, got error: %v", err)
			}
			if got != nil {
				t.Fatalf("expected nil, got %q", got)
			}
		})

		t.Run("3_idempotent_delete", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			_ = s.Put(ctx, refA, val)
			if err := s.Delete(ctx, refA); err != nil {
				t.Fatalf("1º Delete: %v", err)
			}
			if err := s.Delete(ctx, refA); err != nil {
				t.Fatalf("the 2nd Delete should be harmless: %v", err)
			}
		})

		t.Run("4_put_replaces", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			_ = s.Put(ctx, refA, ports.SecretValue("antigo"))
			_ = s.Put(ctx, refA, ports.SecretValue("new"))
			got, _ := s.Get(ctx, refA)
			if string(got) != "new" {
				t.Fatalf("expected 'new', got %q", got)
			}
		})

		t.Run("5_isolation_between_accounts", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			if err := s.Put(ctx, refA, ports.SecretValue("from-account-a")); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := s.Get(ctx, refB)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got != nil {
				t.Fatalf("LEAK BETWEEN ACCOUNTS: account B read %q", got)
			}
		})

		t.Run("6_value_does_not_leak_in_text", func(t *testing.T) {
			// SecretValue must not reveal the content when formatted.
			v := ports.SecretValue("never-show-me")
			if got := v.String(); got != "***" {
				t.Fatalf("String() should redact, it returned %q", got)
			}
			if formatted := fmtValue(v); formatted != "***" {
				t.Fatalf("formatting with %%v should redact, it returned %q", formatted)
			}
		})

		t.Run("exists_reflects_state", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			// A reference of its OWN: this is the only subtest that asserts
			// anything about ABSENCE, and `refA` has already been written by the
			// previous ones. On the double that did not show up because every
			// subtest got a fresh vault; in a real vault, absence requires a key
			// nobody has touched.
			refNova := refA
			refNova.OwnerID = "res-exists-" + strconv.FormatInt(refSeq.Add(1), 10)
			t.Cleanup(func() { _ = s.Delete(context.Background(), refNova) })

			if ok, _ := s.Exists(ctx, refNova); ok {
				t.Fatal("Exists should be false before the Put")
			}
			_ = s.Put(ctx, refNova, val)
			if ok, _ := s.Exists(ctx, refNova); !ok {
				t.Fatal("Exists should be true after the Put")
			}
		})
	})
}
