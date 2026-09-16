package totp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/platform/totp"
)

// The RFC 6238 vector: the ASCII seed "12345678901234567890", which in base32
// is GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ. Without a published vector the test only
// proves the code agrees with itself — and an implementation that is wrong in
// the same way at both ends passes.
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestItAgreesWithTheRFC6238Vector(t *testing.T) {
	for _, c := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	} {
		got, err := totp.Code(rfcSecret, time.Unix(c.unix, 0).UTC())
		if err != nil {
			t.Fatalf("unix %d: %v", c.unix, err)
		}
		if got != c.want {
			t.Errorf("unix %d = %s, want %s", c.unix, got, c.want)
		}
	}
}

func TestTheWindowAcceptsDriftAndRefusesBeyondIt(t *testing.T) {
	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	code, _ := totp.Code(secret, now)

	for _, drift := range []time.Duration{0, -totp.Step, totp.Step} {
		if !totp.Validate(secret, code, now.Add(drift)) {
			t.Errorf("a drift of %s should be accepted (±%d window)", drift, totp.Skew)
		}
	}
	// Two windows away is another code: accepting it would double the guessing
	// surface for a comfort nobody asked for.
	if totp.Validate(secret, code, now.Add(2*totp.Step+time.Second)) {
		t.Error("a drift beyond the window was accepted")
	}
}

func TestAWrongCodeAndAMalformedOneAreRefused(t *testing.T) {
	secret, _ := totp.NewSecret()
	now := time.Now().UTC()
	for _, code := range []string{"", "12345", "1234567", "abcdef", "000000 "} {
		if totp.Validate(secret, code, now) {
			t.Errorf("%q was accepted", code)
		}
	}
	if totp.Validate("not base32!!", "123456", now) {
		t.Error("an unreadable seed was accepted — it has to refuse, not panic")
	}
}

func TestEachSeedIsDifferentAndHasTheExpectedSize(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := totp.NewSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 32 { // 20 bytes in base32 with no padding
			t.Fatalf("seed with %d characters: %s", len(s), s)
		}
		if seen[s] {
			t.Fatal("a repeated seed — the source of randomness is not one")
		}
		seen[s] = true
	}
}

func TestTheURICarriesWhatAnAuthenticatorReads(t *testing.T) {
	uri := totp.URI("DOP", "dev@dop.local", rfcSecret)
	for _, want := range []string{
		"otpauth://totp/", "secret=" + rfcSecret, "issuer=DOP",
		"algorithm=SHA1", "digits=6", "period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("the URI does not carry %q: %s", want, uri)
		}
	}
}
