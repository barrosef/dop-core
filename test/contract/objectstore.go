package contract

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ObjectStoreSuite verifies the nine guarantees documented on the port.
//
// newStore returns the adapter and the buckets usable in that environment. The
// first serves every case; the second, when it exists, exercises the isolation
// between buckets — on emulated GCS there are not always two buckets
// provisioned, and inventing one would make the test fail for the wrong
// reason.
func ObjectStoreSuite(t *testing.T, name string, newStore func(t *testing.T) (ports.ObjectStore, []string)) {
	t.Run(name, func(t *testing.T) {
		ctx := context.Background()

		// A unique prefix per run: the bucket may be shared between runs (and
		// between adapters) and test keys must not cross.
		keyPrefix := func(t *testing.T) string {
			return fmt.Sprintf("contract/%d-%d/", time.Now().UnixNano(), keySeq.Add(1))
		}

		t.Run("1_immediate_read_after_write", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: keyPrefix(t) + "diagram.svg"}
			content := []byte("<svg>drawing</svg>")

			if err := s.Put(ctx, ref, content, "image/svg+xml"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })

			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("divergent content: %q != %q", got, content)
			}
		})

		t.Run("2_put_replaces_the_whole_object", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: keyPrefix(t) + "note.txt"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })

			if err := s.Put(ctx, ref, []byte("an old version, much longer"), "text/plain"); err != nil {
				t.Fatalf("the 1st Put: %v", err)
			}
			if err := s.Put(ctx, ref, []byte("new"), "text/plain"); err != nil {
				t.Fatalf("the 2nd Put: %v", err)
			}
			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			// The replacement is total: leftover old bytes mean a write over the
			// top without truncating, which is silent corruption.
			if string(got) != "new" {
				t.Fatalf("expected 'new', got %q", got)
			}
			meta, err := s.Stat(ctx, ref)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if meta.Size != 3 {
				t.Fatalf("Stat.Size = %d, expected 3 — the size stayed on the old version", meta.Size)
			}
		})

		t.Run("3_absence_is_a_not_found_error", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: keyPrefix(t) + "never-written"}

			_, err := s.Get(ctx, ref)
			if err == nil {
				t.Fatal("a Get of an absent object should fail (here absence is an error, unlike in SecretStore)")
			}
			if k := errs.KindOf(err); k != errs.KindNotFound {
				t.Fatalf("a Get of an absent object returned kind %q, expected %q", k, errs.KindNotFound)
			}
			if _, err := s.Stat(ctx, ref); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("a Stat of an absent object returned %v, expected kind %q", err, errs.KindNotFound)
			}
		})

		t.Run("4_idempotent_delete", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: keyPrefix(t) + "temporary"}

			if err := s.Put(ctx, ref, []byte("x"), ""); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if err := s.Delete(ctx, ref); err != nil {
				t.Fatalf("1º Delete: %v", err)
			}
			if err := s.Delete(ctx, ref); err != nil {
				t.Fatalf("the 2nd Delete should be harmless: %v", err)
			}
			if _, err := s.Get(ctx, ref); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("o objeto sobreviveu ao Delete: %v", err)
			}
		})

		t.Run("5_the_key_is_opaque_and_flat", func(t *testing.T) {
			s, buckets := newStore(t)
			p := keyPrefix(t)
			parent := ports.ObjectRef{Bucket: buckets[0], Key: p + "a/b"}
			child := ports.ObjectRef{Bucket: buckets[0], Key: p + "a/b/c"}
			t.Cleanup(func() {
				_ = s.Delete(context.Background(), parent)
				_ = s.Delete(context.Background(), child)
			})

			// A deliberate order: first the "child", then the "parent". An
			// adapter that treated the slash as a directory would fail here —
			// "a/b" would already be a folder. The slash is a character of the
			// NAME, not hierarchy.
			if err := s.Put(ctx, child, []byte("do child"), "text/plain"); err != nil {
				t.Fatalf("Put do child: %v", err)
			}
			if err := s.Put(ctx, parent, []byte("do parent"), "text/plain"); err != nil {
				t.Fatalf("Put of 'a/b' with 'a/b/c' existing: %v", err)
			}
			for ref, want := range map[ports.ObjectRef]string{parent: "do parent", child: "do child"} {
				got, err := s.Get(ctx, ref)
				if err != nil {
					t.Fatalf("Get %q: %v", ref.Key, err)
				}
				if string(got) != want {
					t.Fatalf("Get %q returned %q, expected %q", ref.Key, got, want)
				}
			}
			// A prefix is not an object.
			if _, err := s.Get(ctx, ports.ObjectRef{Bucket: buckets[0], Key: p + "a"}); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("the prefix 'a' is not an object and should give not_found, got %v", err)
			}
		})

		t.Run("6_the_key_does_not_escape_the_bucket", func(t *testing.T) {
			s, buckets := newStore(t)
			p := keyPrefix(t)
			escaping := ports.ObjectRef{Bucket: buckets[0], Key: p + "../" + p + "escaping"}
			target := ports.ObjectRef{Bucket: buckets[0], Key: p + "escaping"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), escaping) })

			// Two answers are acceptable — refusing the key, or treating it as a
			// literal NAME. What is not acceptable is writing somewhere else.
			if err := s.Put(ctx, escaping, []byte("escaping content"), ""); err != nil {
				return
			}
			if _, err := s.Get(ctx, target); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("the key with '..' escaped: %q came to exist (%v)", target.Key, err)
			}
		})

		t.Run("7_isolation_between_buckets", func(t *testing.T) {
			s, buckets := newStore(t)
			if len(buckets) < 2 {
				t.Skip("an environment with a single bucket — isolation between buckets is not exercisable here")
			}
			key := keyPrefix(t) + "same-name"
			a := ports.ObjectRef{Bucket: buckets[0], Key: key}
			b := ports.ObjectRef{Bucket: buckets[1], Key: key}
			t.Cleanup(func() {
				_ = s.Delete(context.Background(), a)
				_ = s.Delete(context.Background(), b)
			})

			if err := s.Put(ctx, a, []byte("do bucket A"), ""); err != nil {
				t.Fatalf("Put em A: %v", err)
			}
			if _, err := s.Get(ctx, b); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("LEAK BETWEEN BUCKETS: the same key resolved in B (%v)", err)
			}
		})

		t.Run("8_metadata", func(t *testing.T) {
			s, buckets := newStore(t)
			p := keyPrefix(t)
			withType := ports.ObjectRef{Bucket: buckets[0], Key: p + "with-type.json"}
			withoutType := ports.ObjectRef{Bucket: buckets[0], Key: p + "without-type.bin"}
			t.Cleanup(func() {
				_ = s.Delete(context.Background(), withType)
				_ = s.Delete(context.Background(), withoutType)
			})

			content := []byte(`{"a":1}`)
			if err := s.Put(ctx, withType, content, "application/json"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			meta, err := s.Stat(ctx, withType)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if meta.Size != int64(len(content)) {
				t.Errorf("Stat.Size = %d, expected %d", meta.Size, len(content))
			}
			if meta.ContentType != "application/json" {
				t.Errorf("Stat.ContentType = %q, expected application/json", meta.ContentType)
			}
			if meta.UpdatedAt.IsZero() {
				t.Error("Stat.UpdatedAt is zeroed: without it there is no telling which version is there")
			}

			// A Put with no type falls back to the binary default — the same choice in both adapters.
			if err := s.Put(ctx, withoutType, []byte("bytes"), ""); err != nil {
				t.Fatalf("Put with no type: %v", err)
			}
			m2, err := s.Stat(ctx, withoutType)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if m2.ContentType != "application/octet-stream" {
				t.Errorf("with no Content-Type the default should be application/octet-stream, got %q", m2.ContentType)
			}

			// UpdatedAt does not go backwards on a rewrite.
			before := meta.UpdatedAt
			time.Sleep(5 * time.Millisecond)
			if err := s.Put(ctx, withType, []byte(`{"a":2}`), "application/json"); err != nil {
				t.Fatalf("reescrita: %v", err)
			}
			after, err := s.Stat(ctx, withType)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if after.UpdatedAt.Before(before) {
				t.Errorf("UpdatedAt went backwards: %v after %v", after.UpdatedAt, before)
			}
		})

		t.Run("9_an_empty_object_is_valid", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: keyPrefix(t) + "empty"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })

			// Uploading an empty file exists and must not become "it does not exist".
			if err := s.Put(ctx, ref, nil, "text/plain"); err != nil {
				t.Fatalf("empty Put: %v", err)
			}
			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get of the empty object: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("expected zero bytes, got %d", len(got))
			}
			meta, err := s.Stat(ctx, ref)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if meta.Size != 0 {
				t.Fatalf("Stat.Size = %d for an empty object", meta.Size)
			}
		})

		t.Run("10_long_key", func(t *testing.T) {
			s, buckets := newStore(t)
			// 300 characters: above the filename limit (255) and below GCS's key
			// limit (1024). A disk adapter that maps the key to a raw filename
			// breaks exactly here.
			long := keyPrefix(t) + strings.Repeat("k", 300)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: long}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })

			if err := s.Put(ctx, ref, []byte("key comprida"), ""); err != nil {
				t.Fatalf("Put with a %d-byte key: %v", len(long), err)
			}
			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if string(got) != "key comprida" {
				t.Fatalf("divergent content: %q", got)
			}
			// Two different long keys must not collide at the destination.
			other := ports.ObjectRef{Bucket: buckets[0], Key: long + "-other"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), other) })
			if err := s.Put(ctx, other, []byte("another object"), ""); err != nil {
				t.Fatalf("Put of the second long key: %v", err)
			}
			if got, _ := s.Get(ctx, ref); string(got) != "key comprida" {
				t.Fatalf("COLLISION: the second long key overwrote the first (%q)", got)
			}
		})

		t.Run("11_an_empty_key_is_refused", func(t *testing.T) {
			s, buckets := newStore(t)
			if err := s.Put(ctx, ports.ObjectRef{Bucket: buckets[0], Key: ""}, []byte("x"), ""); err == nil {
				t.Fatal("an object with no key should not be accepted")
			}
		})

		t.Run("12_signed_url_or_explicit_unavailable", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: keyPrefix(t) + "direct-upload"}

			for name, fn := range map[string]func(context.Context, ports.ObjectRef, time.Duration) (string, error){
				"SignedPutURL": s.SignedPutURL,
				"SignedGetURL": s.SignedGetURL,
			} {
				// The URL applies to an object that does not exist yet — it is
				// how a direct upload works: sign first, the client writes
				// after.
				u, err := fn(ctx, ref, 10*time.Minute)
				switch {
				case err == nil && u == "":
					t.Errorf("%s returned an empty string WITH NO error — the caller has no way "+
						"to know it has to fall back to uploading through the BFF", name)
				case err != nil && errs.KindOf(err) != errs.KindUnavailable:
					t.Errorf("%s failed with kind %q; the port admits only %q for an absent capability",
						name, errs.KindOf(err), errs.KindUnavailable)
				}
			}
		})

		t.Run("13_concurrent_use", func(t *testing.T) {
			s, buckets := newStore(t)
			p := keyPrefix(t)

			// Writes to distinct keys must not get in each other's way.
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					ref := ports.ObjectRef{Bucket: buckets[0], Key: fmt.Sprintf("%sconc-%d", p, i)}
					if err := s.Put(ctx, ref, []byte(fmt.Sprintf("objeto %d", i)), ""); err != nil {
						t.Errorf("concurrent Put %d: %v", i, err)
					}
				}(i)
			}
			wg.Wait()
			for i := 0; i < 16; i++ {
				ref := ports.ObjectRef{Bucket: buckets[0], Key: fmt.Sprintf("%sconc-%d", p, i)}
				got, err := s.Get(ctx, ref)
				if err != nil {
					t.Fatalf("Get concorrente %d: %v", i, err)
				}
				if string(got) != fmt.Sprintf("objeto %d", i) {
					t.Fatalf("objeto %d embaralhado: %q", i, got)
				}
				_ = s.Delete(ctx, ref)
			}

			// The replacement is atomic for whoever reads: either the short
			// version or the long one — never a piece of both.
			ref := ports.ObjectRef{Bucket: buckets[0], Key: p + "disputado"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })
			short := []byte("short")
			long := bytes.Repeat([]byte("L"), 4096)
			if err := s.Put(ctx, ref, short, ""); err != nil {
				t.Fatalf("Put inicial: %v", err)
			}
			stop := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					v := short
					if i%2 == 0 {
						v = long
					}
					if err := s.Put(ctx, ref, v, ""); err != nil {
						t.Errorf("Put during a read: %v", err)
						return
					}
				}
			}()
			for i := 0; i < 50; i++ {
				got, err := s.Get(ctx, ref)
				if err != nil {
					close(stop)
					wg.Wait()
					t.Fatalf("Get during a write: %v", err)
				}
				if !bytes.Equal(got, short) && !bytes.Equal(got, long) {
					close(stop)
					wg.Wait()
					t.Fatalf("PARTIAL READ: %d bytes that are neither of the two versions", len(got))
				}
			}
			close(stop)
			wg.Wait()
		})
	})
}

// keySeq guarantees a distinct keyPrefix even within the same nanosecond.
var keySeq atomic.Int64
