package callauth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/callauth"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

var key = []byte("a-key-long-enough-for-a-test-0123")

func TestWhatWasSignedComesBackWhole(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	want := callauth.Assertion{
		Caller: "bff", ActorID: "usr-1", ActorKind: "user",
		AccountID: "acct-9", SessionID: "sess-3", ExpiresAt: now.Add(time.Minute),
	}
	raw, err := callauth.Sign(key, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := callauth.Verify(key, raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Caller != want.Caller || got.ActorID != want.ActorID ||
		got.ActorKind != want.ActorKind || got.AccountID != want.AccountID ||
		got.SessionID != want.SessionID {
		t.Errorf("came back changed: %+v", got)
	}
}

func TestAnActorWithNoIdIsLegitimate(t *testing.T) {
	// The edge resolving a subject into a user does not yet know the actor. An
	// assertion has to be able to say "I am the BFF, acting for nobody yet" —
	// refusing that would push whoever wrote it into inventing an id.
	now := time.Unix(1_800_000_000, 0)
	raw, err := callauth.Sign(key, callauth.Assertion{
		Caller: "bff", ActorKind: "system", ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := callauth.Verify(key, raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActorID != "" || got.ActorKind != "system" {
		t.Errorf("%+v", got)
	}
}

func TestATamperedClaimIsRefused(t *testing.T) {
	// This is the whole point over a shared secret: the ACTOR is inside what was
	// signed, so promoting yourself breaks the signature.
	now := time.Unix(1_800_000_000, 0)
	raw, _ := callauth.Sign(key, callauth.Assertion{
		Caller: "bff", ActorID: "usr-1", ActorKind: "user",
		AccountID: "acct-9", ExpiresAt: now.Add(time.Minute)})
	tampered := strings.Replace(raw, "dXNyLTE", "dXNyLTI", 1) // usr-1 -> usr-2, if it were that simple
	if tampered == raw {
		// The payload is one base64 blob; flip a byte in it instead.
		b := []byte(raw)
		b[3] = b[3] ^ 0x01
		tampered = string(b)
	}
	if _, err := callauth.Verify(key, tampered, now); errs.KindOf(err) != errs.KindUnauthorized {
		t.Fatalf("a tampered assertion was accepted: %v", err)
	}
}

func TestAnotherKeyIsRefused(t *testing.T) {
	// One key per caller is what keeps a compromised component forging only its
	// own calls.
	now := time.Unix(1_800_000_000, 0)
	raw, _ := callauth.Sign(key, callauth.Assertion{
		Caller: "bff", ActorID: "usr-1", ExpiresAt: now.Add(time.Minute)})
	if _, err := callauth.Verify([]byte("another-key-entirely-0123456789ab"), raw, now); err == nil {
		t.Fatal("another key verified the assertion")
	}
}

func TestItExpires(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	raw, _ := callauth.Sign(key, callauth.Assertion{
		Caller: "bff", ActorID: "usr-1", ExpiresAt: now.Add(30 * time.Second)})
	if _, err := callauth.Verify(key, raw, now.Add(31*time.Second)); err == nil {
		t.Fatal("an expired assertion was accepted")
	}
	// And the refusal reads the same as a bad signature: telling them apart
	// tells whoever is probing which half to work on.
	bad := errs.New(errs.KindUnauthorized, "assertion not accepted")
	_, err := callauth.Verify(key, raw, now.Add(31*time.Second))
	if err.Error() != bad.Error() {
		t.Errorf("the expiry refusal reveals itself: %q", err)
	}
}

func TestAFieldWithASeparatorIsRefusedAtSigning(t *testing.T) {
	// An id carrying a `|` is a bug at the source. Escaping it would hide the
	// bug until the two ends disagreed about the escaping rule.
	_, err := callauth.Sign(key, callauth.Assertion{
		Caller: "bff", ActorID: "usr|1", ExpiresAt: time.Now().Add(time.Minute)})
	if errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("it signed an id with a separator: %v", err)
	}
}

func TestGarbageIsRefusedAndDoesNotPanic(t *testing.T) {
	now := time.Now()
	for _, raw := range []string{"", ".", "a.b", "not-base64!.deadbeef", strings.Repeat("x", 500)} {
		if _, err := callauth.Verify(key, raw, now); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestTheWireFormatIsPinnedToTheSameBytesAsTheEdge(t *testing.T) {
	// The edge is Python and this is Go: the two can only agree by accident
	// unless something pins them. The BFF has this SAME vector in
	// tests/test_app.py, and a drift does not fail loudly — it makes every call
	// arrive unauthenticated, which is an outage in strict and silence in
	// permissive.
	const vector = "YmZmfHVzci0xfHVzZXJ8YWNjdC05fHNlc3MtM3wxODAwMDAwMDYw" +
		".54240927df959a745d2ba1a1fc1947a9e9466f32be02b6b96876ebe930aed530"

	as, err := callauth.Verify(key, vector, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("the edge's vector was refused here: %v", err)
	}
	if as.Caller != "bff" || as.ActorID != "usr-1" || as.ActorKind != "user" ||
		as.AccountID != "acct-9" || as.SessionID != "sess-3" {
		t.Errorf("the fields came out different: %+v", as)
	}

	// And the other direction: signing the same thing here produces the same
	// bytes. Verifying alone would let the two drift as long as both were
	// self-consistent.
	mine, err := callauth.Sign(key, callauth.Assertion{
		Caller: "bff", ActorID: "usr-1", ActorKind: "user",
		AccountID: "acct-9", SessionID: "sess-3",
		ExpiresAt: time.Unix(1_800_000_060, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if mine != vector {
		t.Errorf("Sign produced other bytes:\n got %s\nwant %s", mine, vector)
	}
}
