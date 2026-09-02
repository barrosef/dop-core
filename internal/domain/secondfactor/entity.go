// Package secondfactor is the domain of the second step of a sign-in
// (ADR-0027).
//
// It is the PLATFORM's, and not the identity provider's, for three reasons the
// ADR develops: the provider does not do e-mail as a second factor, its MFA
// semantics do not survive the adapter swap ADR-0001 exists for, and the local
// emulator cannot exercise it.
//
// House rule: this package knows nothing of Postgres, gRPC or any SDK. It
// declares what it needs as a PORT (repository.go, and ports.Mailer/ports.SMSer
// for the channels) and the composition root wires it.
package secondfactor

import (
	"crypto/sha256"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Kind is the verifier. Three, because they are the three the product offers —
// and a fourth (a passkey) fits without the entity changing shape.
type Kind string

const (
	KindTOTP  Kind = "totp"
	KindEmail Kind = "email"
	KindSMS   Kind = "sms"
)

func ValidKind(k Kind) bool {
	switch k {
	case KindTOTP, KindEmail, KindSMS:
		return true
	}
	return false
}

// NeedsChannel: TOTP proves possession from the seed, with nothing sent. The
// other two send a code, and that is the predicate that decides whether there is
// a channel to call, a destination to validate and a cost per attempt.
func (k Kind) NeedsChannel() bool { return k == KindEmail || k == KindSMS }

type Status string

const (
	// StatusPending: registered and NOT proven. It grants nothing.
	StatusPending Status = "pending"
	StatusActive  Status = "active"
	StatusRevoked Status = "revoked"
)

// Purpose separates the two things a challenge can be for. A challenge issued
// to enrol does not step a session up, and one issued to sign in does not
// confirm a factor — without the distinction, enrolling would authenticate.
type Purpose string

const (
	PurposeEnrollment Purpose = "enrollment"
	PurposeStepUp     Purpose = "step_up"
)

func ValidPurpose(p Purpose) bool { return p == PurposeEnrollment || p == PurposeStepUp }

// Factor is the person's registered factor. The SEED is not here — only the
// opaque reference to it in the vault.
type Factor struct {
	ID          string
	UserID      string
	Kind        Kind
	Status      Status
	Label       string
	SecretRef   string // TOTP only
	Destination string // email/sms only
	ConfirmedAt *time.Time
	LastUsedAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (f Factor) IsActive() bool { return f.Status == StatusActive }

// MaskedDestination is what the edge shows. The whole address is stored because
// it has to be reachable; what a screen — and a log, and a support ticket —
// needs is enough to recognize it and not enough to use it.
func (f Factor) MaskedDestination() string { return Mask(f.Kind, f.Destination) }

func Mask(k Kind, dest string) string {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return ""
	}
	switch k {
	case KindEmail:
		at := strings.LastIndex(dest, "@")
		if at <= 0 {
			return "***"
		}
		local, domain := dest[:at], dest[at+1:]
		if len(local) <= 1 {
			return "*@" + domain
		}
		return local[:1] + strings.Repeat("*", len(local)-1) + "@" + domain
	case KindSMS:
		digits := []rune{}
		for _, r := range dest {
			if r >= '0' && r <= '9' {
				digits = append(digits, r)
			}
		}
		if len(digits) <= 4 {
			return strings.Repeat("*", len(digits))
		}
		tail := string(digits[len(digits)-4:])
		return strings.Repeat("*", len(digits)-4) + tail
	}
	return "***"
}

// Challenge is ONE attempt to prove possession, with its own counter.
type Challenge struct {
	ID         string
	FactorID   string
	UserID     string
	Purpose    Purpose
	CodeHash   []byte // nil for TOTP: the proof is computed, not stored
	Attempts   int
	ConsumedAt *time.Time
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// MaxAttempts is the cool-off. Five, because it is far above a person's typo
// rate and far below what makes 10^6 guesses viable.
const MaxAttempts = 5

// CodeTTL is how long a sent code lives. Ten minutes: long enough for an SMS
// that takes its time, short enough that an old message in an inbox is not a
// key.
const CodeTTL = 10 * time.Minute

// ResendInterval is the floor between two sends to the SAME factor.
//
// It exists for the person (a second button press must not send two messages)
// and against a script: with e-mail and SMS every challenge is a message, and
// with SMS it is money. Sixty seconds is longer than an impatient click and
// shorter than the wait for a message that got lost.
const ResendInterval = 60 * time.Second

// MaxChallengesPerHour is the ceiling per factor.
//
// The attempt counter already stops GUESSING; this stops the other abuse, which
// costs nothing to whoever does it and costs a message to us: asking for codes
// in a loop. Five in an hour covers a person who genuinely lost two messages and
// refuses the loop.
const MaxChallengesPerHour = 5

// ChallengeWindow is the window MaxChallengesPerHour is counted in.
const ChallengeWindow = time.Hour

// StepUpTTL is how long a session stays stepped up. Twelve hours is a working
// day: asking again in the middle of it teaches people to answer without
// reading, and never asking again turns the factor into a formality at sign-up.
const StepUpTTL = 12 * time.Hour

// RecoveryCodeCount and recoveryCodeBytes: ten codes of 80 bits each. Ten
// because it is what fits on a piece of paper and covers a few losses; 80 bits
// because a recovery code is not typed against a rate limit only — it is the
// LAST door.
const RecoveryCodeCount = 10

func (c Challenge) Expired(now time.Time) bool { return !now.Before(c.ExpiresAt) }
func (c Challenge) Consumed() bool             { return c.ConsumedAt != nil }
func (c Challenge) Exhausted() bool            { return c.Attempts >= MaxAttempts }

// Usable is the single reading of "may this challenge still be answered". One
// function, because three callers asking the same question in three `if`s is
// how the answers start diverging.
func (c Challenge) Usable(now time.Time) error {
	switch {
	case c.Consumed():
		return errs.Precondition("this code has already been used").WithCode(KeyChallengeConsumed, nil)
	case c.Expired(now):
		return errs.Precondition("this code has expired").WithCode(KeyChallengeExpired, nil)
	case c.Exhausted():
		return errs.Precondition("too many attempts on this code; ask for another").
			WithCode(KeyChallengeExhausted, map[string]any{"max": MaxAttempts})
	}
	return nil
}

// StepUp is the session that has already answered.
type StepUp struct {
	UserID     string
	SessionID  string
	Method     Kind
	VerifiedAt time.Time
	ExpiresAt  time.Time
}

// RecoveryCode is the stored form: the hash and whether it has been used. The
// value only exists once, in the response to the generation.
type RecoveryCode struct {
	ID        string
	UserID    string
	CodeHash  []byte
	UsedAt    *time.Time
	CreatedAt time.Time
}

// ── hashing ─────────────────────────────────────────────────────────────────

// HashCode hashes a sent code, salted with the factor's id.
//
// A plain SHA-256 is deliberate and it is documented in the migration: this is
// not a password. It is six digits that live ten minutes, are single use and die
// after five attempts. The hash exists so that reading the table does not hand
// over a LIVE code; the salt stops one rainbow table from serving every row.
func HashCode(factorID, code string) []byte {
	sum := sha256.Sum256([]byte(factorID + ":" + strings.TrimSpace(code)))
	return sum[:]
}

// HashRecoveryCode hashes over the whole value — these ARE high-entropy
// secrets, and they have no factor to salt them with.
func HashRecoveryCode(code string) []byte {
	sum := sha256.Sum256([]byte(normalizeRecovery(code)))
	return sum[:]
}

// normalizeRecovery accepts the code the way a person types it: in any case,
// with or without the group separator.
func normalizeRecovery(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

// ── validation ──────────────────────────────────────────────────────────────

const labelMaxLen = 60

// Translation keys for the refusals a person reads.
const (
	KeyKindUnknown        = "second_factor.kind.unknown"
	KeyKindNotAllowed     = "second_factor.kind.not_allowed"
	KeyLabelRequired      = "second_factor.label.required"
	KeyLabelTooLong       = "second_factor.label.too_long"
	KeyDestinationInvalid = "second_factor.destination.invalid"
	KeyEmailNotVerified   = "second_factor.email.not_verified"
	KeyFactorNotFound     = "second_factor.not_found"
	KeyFactorNotActive    = "second_factor.not_active"
	KeyFactorAlreadyLive  = "second_factor.already_registered"
	KeyNoActiveFactor     = "second_factor.none_active"
	KeyLastFactorRequired = "second_factor.last_one_required"
	KeyCodeInvalid        = "second_factor.code.invalid"
	KeyChallengeConsumed  = "second_factor.challenge.consumed"
	KeyChallengeExpired   = "second_factor.challenge.expired"
	KeyChallengeExhausted = "second_factor.challenge.exhausted"
	KeyChallengeNotFound  = "second_factor.challenge.not_found"
	KeyResendTooSoon      = "second_factor.challenge.too_soon"
	KeyTooManyChallenges  = "second_factor.challenge.too_many"
	KeyStepUpRequired     = "second_factor.step_up.required"
	KeyRecoveryInvalid    = "second_factor.recovery.invalid"
	KeySessionMissing     = "second_factor.session.missing"
)

func ValidateLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", errs.Invalid("the factor needs a name").WithCode(KeyLabelRequired, nil)
	}
	if len(label) > labelMaxLen {
		return "", errs.Invalid("the factor's name may have at most %d characters", labelMaxLen).
			WithCode(KeyLabelTooLong, map[string]any{"max": labelMaxLen})
	}
	return label, nil
}

// ValidateDestination checks the address BEFORE anything is sent. With SMS the
// check is also about money: a broken number costs a message.
func ValidateDestination(k Kind, dest string) (string, error) {
	dest = strings.TrimSpace(dest)
	switch k {
	case KindEmail:
		at := strings.LastIndex(dest, "@")
		if at <= 0 || at == len(dest)-1 || strings.Contains(dest, " ") {
			return "", errs.Invalid("invalid e-mail address").WithCode(KeyDestinationInvalid, nil)
		}
		return strings.ToLower(dest), nil
	case KindSMS:
		if !ValidE164(dest) {
			return "", errs.Invalid("the number has to be in international format, like +5511999999999").
				WithCode(KeyDestinationInvalid, nil)
		}
		return dest, nil
	}
	return "", nil
}

// ValidE164 is the same reading the SMSer's guarantee 1 demands, in the domain:
// refusing here is what stops a broken number from becoming a round trip and a
// charge.
func ValidE164(s string) bool {
	if !strings.HasPrefix(s, "+") {
		return false
	}
	digits := s[1:]
	if len(digits) < 8 || len(digits) > 15 {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
