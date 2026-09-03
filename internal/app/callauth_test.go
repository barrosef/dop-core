package app

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/callauth"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

var authKey = []byte("a-key-long-enough-for-a-test-0123")

type fakeTokens struct {
	subject string
	err     error
}

func (f fakeTokens) VerifyToken(context.Context, string) (*ports.Principal, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &ports.Principal{Subject: f.subject}, nil
}

type fakeUsers struct {
	bySubject map[string]string
	calls     int
}

func (f *fakeUsers) UserBySubject(_ context.Context, s string) (*identity.User, error) {
	f.calls++
	id, ok := f.bySubject[s]
	if !ok {
		return nil, nil
	}
	return &identity.User{ID: id, Subject: s}, nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func newAuth(t *testing.T, mode string, tokens ports.IdentityProvider, users subjectResolver) (*callAuth, time.Time) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	return newCallAuth(mode, map[string][]byte{"bff": authKey}, tokens, users, fixedClock{now}), now
}

func mdWith(pairs ...string) metadata.MD { return metadata.Pairs(pairs...) }

func TestStrictRefusesWhatIsOnlyClaimed(t *testing.T) {
	// The whole point of ADR-0029: the metadata is text. In strict mode a call
	// carrying only headers arrives with NO actor, and the use case fails at the
	// authorization — where the message means something — instead of here.
	a, _ := newAuth(t, "strict", fakeTokens{}, &fakeUsers{})
	got := a.authenticate(context.Background(),
		mdWith("x-actor-id", "usr-invented", "x-account-id", "acct-9"),
		ctxutil.Call{ActorID: "usr-invented", AccountID: "acct-9"})
	if got.ActorID != "" || got.AccountID != "" {
		t.Fatalf("an unsigned call got through: %+v", got)
	}
}

func TestPermissiveLetsItThroughSoTheMigrationCanHappen(t *testing.T) {
	a, _ := newAuth(t, "permissive", fakeTokens{}, &fakeUsers{})
	got := a.authenticate(context.Background(),
		mdWith("x-actor-id", "usr-1"), ctxutil.Call{ActorID: "usr-1"})
	if got.ActorID != "usr-1" {
		t.Fatal("permissive dropped the actor and would break every caller at once")
	}
}

func TestASignedAssertionIsBelievedAndTheHeadersAreNot(t *testing.T) {
	a, now := newAuth(t, "strict", fakeTokens{}, &fakeUsers{})
	raw, err := callauth.Sign(authKey, callauth.Assertion{
		Caller: "bff", ActorID: "usr-real", ActorKind: "user",
		AccountID: "acct-real", SessionID: "sess-1", ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	got := a.authenticate(context.Background(),
		mdWith("x-dop-assertion", raw, "x-actor-id", "usr-forged", "x-account-id", "acct-forged"),
		ctxutil.Call{ActorID: "usr-forged", AccountID: "acct-forged"})
	if got.ActorID != "usr-real" || got.AccountID != "acct-real" || got.SessionID != "sess-1" {
		t.Fatalf("the headers won over the signature: %+v", got)
	}
}

func TestTheTokenProvesWhoAndTheAssertionCarriesTheAccount(t *testing.T) {
	// The account is NOT in the token — that was the objection to forwarding the
	// JWT alone. The assertion carries it, and it is signed too.
	users := &fakeUsers{bySubject: map[string]string{"sub-1": "usr-7"}}
	a, now := newAuth(t, "strict", fakeTokens{subject: "sub-1"}, users)
	raw, _ := callauth.Sign(authKey, callauth.Assertion{
		Caller: "bff", ActorKind: "system", AccountID: "acct-3",
		ExpiresAt: now.Add(time.Minute)})
	got := a.authenticate(context.Background(),
		mdWith("x-dop-assertion", raw, "authorization", "Bearer the-token"), ctxutil.Call{})
	if got.ActorID != "usr-7" {
		t.Errorf("the token did not resolve the actor: %+v", got)
	}
	if got.AccountID != "acct-3" {
		t.Errorf("the account did not come from the assertion: %+v", got)
	}
	if got.ActorKind != ctxutil.ActorUser {
		t.Errorf("a call with a person is not a user call: %+v", got)
	}
}

func TestATokenAloneIsEnoughToProveWho(t *testing.T) {
	users := &fakeUsers{bySubject: map[string]string{"sub-1": "usr-7"}}
	a, _ := newAuth(t, "strict", fakeTokens{subject: "sub-1"}, users)
	got := a.authenticate(context.Background(),
		mdWith("authorization", "Bearer t", "x-account-id", "acct-5"),
		ctxutil.Call{AccountID: "acct-5"})
	if got.ActorID != "usr-7" || got.AccountID != "acct-5" {
		t.Fatalf("%+v", got)
	}
}

func TestAnAssertionAndATokenThatDisagreeAreRefused(t *testing.T) {
	// It is not a preference between two sources — it is a bug or an attack.
	users := &fakeUsers{bySubject: map[string]string{"sub-1": "usr-7"}}
	a, now := newAuth(t, "strict", fakeTokens{subject: "sub-1"}, users)
	raw, _ := callauth.Sign(authKey, callauth.Assertion{
		Caller: "bff", ActorID: "usr-OTHER", ActorKind: "user", AccountID: "acct-3",
		ExpiresAt: now.Add(time.Minute)})
	got := a.authenticate(context.Background(),
		mdWith("x-dop-assertion", raw, "authorization", "Bearer t"), ctxutil.Call{})
	if got.ActorID != "" {
		t.Fatalf("it picked one of the two instead of refusing: %+v", got)
	}
}

func TestAForgedTokenProvesNothing(t *testing.T) {
	a, _ := newAuth(t, "strict", fakeTokens{err: errs.New(errs.KindUnauthorized, "forged")},
		&fakeUsers{bySubject: map[string]string{"sub-1": "usr-7"}})
	got := a.authenticate(context.Background(),
		mdWith("authorization", "Bearer forged", "x-actor-id", "usr-7"),
		ctxutil.Call{ActorID: "usr-7"})
	if got.ActorID != "" {
		t.Fatalf("a forged token filled in the actor: %+v", got)
	}
}

func TestTheSubjectLookupIsCached(t *testing.T) {
	// Without the cache this is a database hit on EVERY call.
	users := &fakeUsers{bySubject: map[string]string{"sub-1": "usr-7"}}
	a, _ := newAuth(t, "strict", fakeTokens{subject: "sub-1"}, users)
	for i := 0; i < 5; i++ {
		a.authenticate(context.Background(), mdWith("authorization", "Bearer t"), ctxutil.Call{})
	}
	if users.calls != 1 {
		t.Errorf("%d lookups for the same subject", users.calls)
	}
}

func TestOffIsTheOldBehaviour(t *testing.T) {
	a, _ := newAuth(t, "off", fakeTokens{}, &fakeUsers{})
	claimed := ctxutil.Call{ActorID: "usr-1", AccountID: "acct-1"}
	if got := a.authenticate(context.Background(), mdWith(), claimed); got != claimed {
		t.Fatalf("off changed the call: %+v", got)
	}
}
