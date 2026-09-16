package secondfactor

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/logging"
	"github.com/barrosef/dop-core/internal/platform/totp"
)

// Users is the NARROW slice of identity this domain needs: who the person is
// (to address the code and to label the QR) and which scope their secret lives
// in. Declared here, in the language of the question being asked — the
// composition root wires identity into it (internal/app/glue.go).
type Users interface {
	UserProfile(ctx context.Context, userID string) (email, name string, emailVerified bool, err error)
	PersonalAccountOf(ctx context.Context, userID string) (string, error)
}

// vaultKind is the SecretRef's Kind for a TOTP seed. Fixed: the domain does not
// invent vault taxonomy (ADR-0001).
const vaultKind = "second_factor_totp"

// Issuer is what an authenticator app shows above the code.
const Issuer = "DOP"

// Service is the second factor's rules. It takes only PORTS.
type Service struct {
	repo   Repository
	users  Users
	vault  ports.SecretStore
	mailer ports.Mailer
	sms    ports.SMSer
	clock  ports.Clock
}

func NewService(repo Repository, users Users, vault ports.SecretStore,
	mailer ports.Mailer, sms ports.SMSer, clock ports.Clock) *Service {
	return &Service{repo: repo, users: users, vault: vault, mailer: mailer, sms: sms, clock: clock}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// actor is the caller's user id. Every operation here is about the CALLER's own
// factors — there is no "manage somebody else's second factor", by design: an
// admin who could enrol a factor for another person would be an admin who could
// take over their account.
func actor(ctx context.Context) (string, error) {
	c, ok := ctxutil.From(ctx)
	if !ok || c.ActorID == "" {
		return "", errs.Permission("this operation requires an authenticated session")
	}
	return c.ActorID, nil
}

func session(ctx context.Context) (string, error) {
	c, _ := ctxutil.From(ctx)
	if strings.TrimSpace(c.SessionID) == "" {
		return "", errs.Invalid("this operation requires a session identifier").
			WithCode(KeySessionMissing, nil)
	}
	return c.SessionID, nil
}

// ── reading ─────────────────────────────────────────────────────────────────

// List returns the caller's factors. The destination comes MASKED: the whole
// value is stored because it has to be reachable, and a screen needs enough to
// recognize it and not enough to use it.
func (s *Service) List(ctx context.Context) ([]Factor, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	factors, err := s.repo.ListFactors(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range factors {
		factors[i].Destination = factors[i].MaskedDestination()
		factors[i].SecretRef = "" // it never leaves the domain
	}
	return factors, nil
}

// ── enrolment ───────────────────────────────────────────────────────────────

// TOTPEnrollment is what enrolling a TOTP returns — and the ONLY time the seed
// exists outside the vault.
type TOTPEnrollment struct {
	Factor *Factor
	Secret string
	URI    string
	// ChallengeID is the row that will count the confirmation's attempts. Both
	// enrolments return one, so the confirmation has ONE shape — a TOTP with no
	// challenge would have no cool-off, and it is the kind that can be guessed
	// offline.
	ChallengeID string
}

// EnrollTOTP registers an authenticator app. The factor is born PENDING: what
// activates it is Confirm, with a code the app produced — the proof that the
// seed arrived where it should.
func (s *Service) EnrollTOTP(ctx context.Context, label string) (*TOTPEnrollment, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.assertAllowed(ctx, KindTOTP); err != nil {
		return nil, err
	}
	label, err = ValidateLabel(label)
	if err != nil {
		return nil, err
	}
	email, _, _, err := s.users.UserProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	scope, err := s.users.PersonalAccountOf(ctx, userID)
	if err != nil {
		return nil, err
	}

	secret, err := totp.NewSecret()
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "generating the seed")
	}

	// The row is created FIRST: the vault's reference is derived from the
	// factor's id, and a seed stored under an id that does not exist yet would
	// be a secret nobody can reach.
	f, err := s.repo.CreateFactor(ctx, &Factor{
		UserID: userID, Kind: KindTOTP, Status: StatusPending, Label: label,
		SecretRef: "pending", CreatedAt: s.now(), UpdatedAt: s.now(),
	})
	if err != nil {
		return nil, err
	}
	ref := ports.SecretRef{AccountID: scope, Kind: vaultKind, OwnerID: f.ID}
	if err := s.vault.Put(ctx, ref, ports.SecretValue(secret)); err != nil {
		// The row without a seed is a factor that can never be confirmed. It is
		// removed here, and the failure surfaces: a `pending` left behind would
		// occupy the (user, kind) slot and block the next attempt.
		_ = s.repo.RevokeFactor(ctx, f.ID, s.now())
		return nil, err
	}
	f.SecretRef = ref.Kind + ":" + scope + ":" + f.ID
	if _, err := s.repo.ActivateFactor(ctx, f.ID, time.Time{}); err != nil {
		// ActivateFactor with a zero instant only records the reference; see the
		// repository's contract.
		return nil, err
	}

	ch, err := s.issueCode(ctx, f, PurposeEnrollment)
	if err != nil {
		return nil, err
	}

	logging.From(ctx).Info("second factor enrolled", "kind", string(KindTOTP), "factor_id", f.ID)
	return &TOTPEnrollment{
		Factor:      f,
		Secret:      secret,
		URI:         totp.URI(Issuer, addressFor(email, userID), secret),
		ChallengeID: ch.ID,
	}, nil
}

// EnrollCode registers an e-mail or SMS factor and sends the first code. The
// factor is born PENDING for the same reason as TOTP.
func (s *Service) EnrollCode(ctx context.Context, kind Kind, label, destination string) (*Factor, string, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, "", err
	}
	if !kind.NeedsChannel() {
		return nil, "", errs.Invalid("this kind is not enrolled with a destination: %q", kind).
			WithCode(KeyKindUnknown, map[string]any{"kind": string(kind)})
	}
	if err := s.assertAllowed(ctx, kind); err != nil {
		return nil, "", err
	}
	label, err = ValidateLabel(label)
	if err != nil {
		return nil, "", err
	}
	destination, err = ValidateDestination(kind, destination)
	if err != nil {
		return nil, "", err
	}

	// An e-mail factor requires a VERIFIED address (ADR-0026's guarantee 5). An
	// unverified address as a second factor is not a second factor: it is the
	// same unproven address the first factor already trusted.
	if kind == KindEmail {
		email, _, verified, err := s.users.UserProfile(ctx, userID)
		if err != nil {
			return nil, "", err
		}
		if !verified || !strings.EqualFold(strings.TrimSpace(email), destination) {
			return nil, "", errs.Precondition(
				"the e-mail of a second factor has to be the session's verified address").
				WithCode(KeyEmailNotVerified, nil)
		}
	}

	f, err := s.repo.CreateFactor(ctx, &Factor{
		UserID: userID, Kind: kind, Status: StatusPending, Label: label,
		Destination: destination, CreatedAt: s.now(), UpdatedAt: s.now(),
	})
	if err != nil {
		return nil, "", err
	}
	ch, err := s.issueCode(ctx, f, PurposeEnrollment)
	if err != nil {
		return nil, "", err
	}
	logging.From(ctx).Info("second factor enrolled", "kind", string(kind), "factor_id", f.ID)
	f.Destination = f.MaskedDestination()
	return f, ch.ID, nil
}

// Confirm activates a factor with the proof of possession.
//
// It returns the recovery codes when this is the person's FIRST active factor:
// generating them at that moment is what stops the account from being one lost
// phone away from a support ticket.
func (s *Service) Confirm(ctx context.Context, factorID, challengeID, code string) ([]string, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	f, err := s.ownedFactor(ctx, userID, factorID)
	if err != nil {
		return nil, err
	}
	if f.Status == StatusActive {
		return nil, errs.Precondition("this factor is already active").WithCode(KeyFactorAlreadyLive, nil)
	}
	if err := s.checkProof(ctx, f, challengeID, code, PurposeEnrollment); err != nil {
		return nil, err
	}

	first, err := s.isFirstActive(ctx, userID)
	if err != nil {
		return nil, err
	}
	if _, err := s.repo.ActivateFactor(ctx, f.ID, s.now()); err != nil {
		return nil, err
	}
	logging.From(ctx).Info("second factor confirmed", "kind", string(f.Kind), "factor_id", f.ID)

	// Confirming steps the SESSION up: the person proved possession seconds ago,
	// in this session. Asking again would add nothing and would cost a second
	// message.
	//
	// It does not contradict "an enrolment challenge does not authenticate": what
	// steps up is the CONFIRMATION — an operation that names the factor and
	// activates it — and not answering the enrolment's challenge through Verify,
	// which stays refused.
	if sessionID, err := session(ctx); err == nil {
		if _, err := s.stepUp(ctx, userID, sessionID, f.Kind); err != nil {
			return nil, err
		}
	}

	if !first {
		return nil, nil
	}
	return s.generateRecoveryCodes(ctx, userID)
}

// Revoke removes a factor. It refuses to remove the LAST active one while the
// active account requires a second factor — otherwise the person would lock
// themselves out of the account with one click, and the way back would be
// support.
func (s *Service) Revoke(ctx context.Context, factorID string) error {
	userID, err := actor(ctx)
	if err != nil {
		return err
	}
	f, err := s.ownedFactor(ctx, userID, factorID)
	if err != nil {
		return err
	}
	if f.IsActive() {
		actives, err := s.activeFactors(ctx, userID)
		if err != nil {
			return err
		}
		if len(actives) == 1 {
			required, err := s.requiredHere(ctx)
			if err != nil {
				return err
			}
			if required {
				return errs.Precondition(
					"this account requires a second factor: register another before removing this one").
					WithCode(KeyLastFactorRequired, nil)
			}
		}
	}
	if err := s.repo.RevokeFactor(ctx, f.ID, s.now()); err != nil {
		return err
	}

	// Revoking a factor drops EVERY stepped-up session of the person, not only
	// the ones this factor opened.
	//
	// It is deliberately more than the minimum: the step-up records the METHOD,
	// not which factor answered, so "only this one's sessions" is not a question
	// the data can answer. And the reason somebody revokes a factor is that the
	// device is gone — leaving a session open would keep the door the removal
	// was meant to close. The price is that the person answers again on their
	// next sensitive operation, which is small next to the alternative.
	if err := s.repo.DeleteStepUpsOf(ctx, userID); err != nil {
		return err
	}
	logging.From(ctx).Info("second factor revoked", "kind", string(f.Kind), "factor_id", f.ID)
	return nil
}

// ── the challenge ───────────────────────────────────────────────────────────

// Challenge starts a step-up. For e-mail and SMS it SENDS the code; for TOTP it
// only opens the row that counts the attempts.
func (s *Service) Challenge(ctx context.Context, factorID string) (*Challenge, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	f, err := s.pickFactor(ctx, userID, factorID)
	if err != nil {
		return nil, err
	}
	return s.issueCode(ctx, f, PurposeStepUp)
}

// Verify answers a step-up challenge and, on success, steps the SESSION up.
func (s *Service) Verify(ctx context.Context, challengeID, code string) (*StepUp, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	sessionID, err := session(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.repo.ChallengeByID(ctx, challengeID)
	if err != nil {
		return nil, err
	}
	if ch == nil || ch.UserID != userID {
		return nil, errs.NotFound("challenge not found").WithCode(KeyChallengeNotFound, nil)
	}
	f, err := s.ownedFactor(ctx, userID, ch.FactorID)
	if err != nil {
		return nil, err
	}
	if err := s.checkProof(ctx, f, challengeID, code, PurposeStepUp); err != nil {
		return nil, err
	}
	return s.stepUp(ctx, userID, sessionID, f.Kind)
}

// VerifyRecoveryCode is the way back when the factor is lost. It consumes the
// code and steps the session up the same way — a recovery code is a factor,
// with the difference that it dies on use.
func (s *Service) VerifyRecoveryCode(ctx context.Context, code string) (*StepUp, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	sessionID, err := session(ctx)
	if err != nil {
		return nil, err
	}
	ok, err := s.repo.UseRecoveryCode(ctx, userID, HashRecoveryCode(code), s.now())
	if err != nil {
		return nil, err
	}
	if !ok {
		// The same refusal for "it does not exist" and for "it was already
		// used": telling them apart would turn the endpoint into an oracle for
		// which codes exist.
		return nil, errs.Permission("invalid recovery code").WithCode(KeyRecoveryInvalid, nil)
	}
	logging.From(ctx).Info("second factor: a recovery code was used")
	return s.stepUp(ctx, userID, sessionID, "recovery_code")
}

// RegenerateRecoveryCodes replaces every code. It requires a stepped-up session:
// whoever asks for new codes is asking for a new set of keys to the account.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context) ([]string, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.RequireStepUp(ctx); err != nil {
		return nil, err
	}
	return s.generateRecoveryCodes(ctx, userID)
}

// RemainingRecoveryCodes is what the cockpit shows so that "I have two left"
// is not a discovery made on the day of the loss.
func (s *Service) RemainingRecoveryCodes(ctx context.Context) (int, error) {
	userID, err := actor(ctx)
	if err != nil {
		return 0, err
	}
	return s.repo.CountUnusedRecoveryCodes(ctx, userID)
}

// ── the gate ────────────────────────────────────────────────────────────────

// State answers what the cockpit needs in order to decide the sign-in screen:
// whether the account requires a factor, whether the person has one, and
// whether this session has already answered.
type State struct {
	Required   bool
	Enrolled   bool
	SteppedUp  bool
	Allowed    []Kind
	Factors    []Factor
	Recovery   int
	ExpiresAt  *time.Time
	NeedsSetup bool
}

func (s *Service) State(ctx context.Context) (*State, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := s.policyHere(ctx)
	if err != nil {
		return nil, err
	}
	factors, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	active := 0
	for _, f := range factors {
		if f.IsActive() {
			active++
		}
	}
	st := &State{Required: policy.Required, Enrolled: active > 0, Allowed: policy.Allowed, Factors: factors}
	if len(st.Allowed) == 0 {
		st.Allowed = []Kind{KindTOTP, KindEmail, KindSMS}
	}
	st.NeedsSetup = policy.Required && active == 0
	if st.Recovery, err = s.repo.CountUnusedRecoveryCodes(ctx, userID); err != nil {
		return nil, err
	}
	if c, _ := ctxutil.From(ctx); c.SessionID != "" {
		if su, err := s.repo.StepUpFor(ctx, userID, c.SessionID); err == nil && su != nil {
			if s.now().Before(su.ExpiresAt) {
				st.SteppedUp = true
				exp := su.ExpiresAt
				st.ExpiresAt = &exp
			}
		}
	}
	return st, nil
}

// RequireStepUp is THE GATE. It is what the sensitive operations call, and the
// only reading of "has this session already answered".
//
// It is permissive in one case on purpose: a person with NO active factor
// passes, as long as the account does not require one. The alternative — hard
// refusal — would make the platform unusable the day the feature ships, for
// everybody who has not enrolled yet.
func (s *Service) RequireStepUp(ctx context.Context) error {
	userID, err := actor(ctx)
	if err != nil {
		return err
	}
	actives, err := s.activeFactors(ctx, userID)
	if err != nil {
		return err
	}
	required, err := s.requiredHere(ctx)
	if err != nil {
		return err
	}
	if len(actives) == 0 {
		if required {
			return errs.Precondition("this account requires a second factor; register one to continue").
				WithCode(KeyNoActiveFactor, nil)
		}
		return nil
	}

	sessionID, err := session(ctx)
	if err != nil {
		return err
	}
	su, err := s.repo.StepUpFor(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	if su == nil || !s.now().Before(su.ExpiresAt) {
		return errs.Permission("this operation requires the second factor").
			WithCode(KeyStepUpRequired, nil)
	}
	return nil
}

// ── internals ───────────────────────────────────────────────────────────────

func (s *Service) stepUp(ctx context.Context, userID, sessionID string, method Kind) (*StepUp, error) {
	su := StepUp{
		UserID: userID, SessionID: sessionID, Method: method,
		VerifiedAt: s.now(), ExpiresAt: s.now().Add(StepUpTTL),
	}
	if err := s.repo.SaveStepUp(ctx, su); err != nil {
		return nil, err
	}
	logging.From(ctx).Info("session stepped up", "method", string(method))
	return &su, nil
}

// issueCode opens the challenge and, when there is a channel, sends the code.
//
// The order matters: the row is written BEFORE the send. A code that went out
// and has no row is a code nobody can answer; a row whose send failed is a
// challenge the person asks for again.
func (s *Service) issueCode(ctx context.Context, f *Factor, purpose Purpose) (*Challenge, error) {
	if err := s.assertMaySend(ctx, f); err != nil {
		return nil, err
	}
	ch := &Challenge{
		FactorID: f.ID, UserID: f.UserID, Purpose: purpose,
		ExpiresAt: s.now().Add(CodeTTL), CreatedAt: s.now(),
	}
	var code string
	if f.Kind.NeedsChannel() {
		var err error
		if code, err = numericCode(); err != nil {
			return nil, err
		}
		ch.CodeHash = HashCode(f.ID, code)
	}
	saved, err := s.repo.CreateChallenge(ctx, ch)
	if err != nil {
		return nil, err
	}
	if code == "" {
		return saved, nil
	}
	if err := s.send(ctx, f, code); err != nil {
		return nil, err
	}
	return saved, nil
}

// assertMaySend is the ceiling on SENDING — the other half of the cool-off.
//
// The attempt counter stops guessing; this stops the abuse that costs the
// attacker nothing and costs us a message: asking for codes in a loop. It only
// applies where there is a channel — a TOTP challenge sends nothing, and
// throttling it would only make the screen refuse for no reason.
func (s *Service) assertMaySend(ctx context.Context, f *Factor) error {
	if !f.Kind.NeedsChannel() {
		return nil
	}
	count, last, err := s.repo.ChallengesSince(ctx, f.ID, s.now().Add(-ChallengeWindow))
	if err != nil {
		return err
	}
	if !last.IsZero() {
		if wait := ResendInterval - s.now().Sub(last); wait > 0 {
			return errs.Precondition("wait %d seconds before asking for another code",
				int(wait.Seconds()+0.999)).
				WithCode(KeyResendTooSoon, map[string]any{"seconds": int(wait.Seconds() + 0.999)})
		}
	}
	if count >= MaxChallengesPerHour {
		return errs.Precondition("too many codes requested for this factor; try again later").
			WithCode(KeyTooManyChallenges, map[string]any{"max": MaxChallengesPerHour})
	}
	return nil
}

// send is where the CHANNEL is used and the Notifier is not (ADR-0027 §3).
func (s *Service) send(ctx context.Context, f *Factor, code string) error {
	switch f.Kind {
	case KindEmail:
		if s.mailer == nil {
			return errs.New(errs.KindUnavailable, "there is no e-mail channel configured")
		}
		_, err := s.mailer.Send(ctx, ports.Mail{
			Kind: "second_factor_code",
			To:   f.Destination,
			Data: map[string]any{"code": code, "minutes": int(CodeTTL.Minutes())},
		})
		return err
	case KindSMS:
		if s.sms == nil {
			return errs.New(errs.KindUnavailable, "there is no SMS channel configured")
		}
		_, err := s.sms.Send(ctx, ports.SMS{
			To: f.Destination,
			// The text is ready here: SMS is one line, the same in every
			// provider, and inventing a template index for one line would be
			// ceremony with no editor to preserve.
			Text: fmt.Sprintf("%s: %s is your verification code. It expires in %d minutes.",
				Issuer, code, int(CodeTTL.Minutes())),
		})
		return err
	}
	return nil
}

// checkProof is the single verification path: it registers the attempt, checks
// the proof and consumes the challenge. Every refusal is the SAME message —
// telling "wrong code" from "expired challenge" apart out loud helps an attacker
// more than it helps a person, and the specific reason is in the challenge's
// state, which the caller may read.
func (s *Service) checkProof(ctx context.Context, f *Factor, challengeID, code string, want Purpose) error {
	ch, err := s.repo.ChallengeByID(ctx, challengeID)
	if err != nil {
		return err
	}
	if ch == nil || ch.FactorID != f.ID {
		return errs.NotFound("challenge not found").WithCode(KeyChallengeNotFound, nil)
	}
	if ch.Purpose != want {
		// A challenge issued to enrol does not step a session up, and one
		// issued to sign in does not confirm a factor.
		return errs.Precondition("this code was not issued for this operation").
			WithCode(KeyChallengeConsumed, nil)
	}
	if err := ch.Usable(s.now()); err != nil {
		return err
	}

	ok := false
	if f.Kind == KindTOTP {
		secret, err := s.seedOf(ctx, f)
		if err != nil {
			return err
		}
		ok = totp.Validate(secret, code, s.now())
	} else {
		ok = subtleEqual(ch.CodeHash, HashCode(f.ID, code))
	}

	if _, err := s.repo.RegisterAttempt(ctx, ch.ID, ok, s.now()); err != nil {
		return err
	}
	if !ok {
		return errs.Permission("invalid code").WithCode(KeyCodeInvalid, nil)
	}
	return s.repo.TouchFactor(ctx, f.ID, s.now())
}

func (s *Service) seedOf(ctx context.Context, f *Factor) (string, error) {
	scope, err := s.users.PersonalAccountOf(ctx, f.UserID)
	if err != nil {
		return "", err
	}
	v, err := s.vault.Get(ctx, ports.SecretRef{AccountID: scope, Kind: vaultKind, OwnerID: f.ID})
	if err != nil {
		return "", err
	}
	if len(v) == 0 {
		// The row exists and the seed does not: the factor cannot be proven, and
		// saying "invalid code" would send the person to reinstall the app for
		// a problem that is ours.
		return "", errs.New(errs.KindInternal, "the factor's seed is not in the vault")
	}
	return string(v), nil
}

func (s *Service) ownedFactor(ctx context.Context, userID, factorID string) (*Factor, error) {
	f, err := s.repo.FactorByID(ctx, factorID)
	if err != nil {
		return nil, err
	}
	// Another person's factor is NOT FOUND, not "forbidden": whoever is outside
	// should not even discover that the id exists.
	if f == nil || f.UserID != userID || f.Status == StatusRevoked {
		return nil, errs.NotFound("factor not found").WithCode(KeyFactorNotFound, nil)
	}
	return f, nil
}

// pickFactor resolves which factor answers a challenge: the one asked for, or
// the person's only active one. With more than one and no choice, it refuses —
// choosing for them would send an SMS (and a charge) for somebody who wanted
// TOTP.
func (s *Service) pickFactor(ctx context.Context, userID, factorID string) (*Factor, error) {
	if factorID != "" {
		f, err := s.ownedFactor(ctx, userID, factorID)
		if err != nil {
			return nil, err
		}
		if !f.IsActive() {
			return nil, errs.Precondition("this factor is not active").WithCode(KeyFactorNotActive, nil)
		}
		return f, nil
	}
	actives, err := s.activeFactors(ctx, userID)
	if err != nil {
		return nil, err
	}
	switch len(actives) {
	case 0:
		return nil, errs.Precondition("no active second factor").WithCode(KeyNoActiveFactor, nil)
	case 1:
		return &actives[0], nil
	default:
		return nil, errs.Invalid("choose which factor to use").WithCode(KeyFactorNotFound, nil)
	}
}

func (s *Service) activeFactors(ctx context.Context, userID string) ([]Factor, error) {
	all, err := s.repo.ListFactors(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Factor, 0, len(all))
	for _, f := range all {
		if f.IsActive() {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *Service) isFirstActive(ctx context.Context, userID string) (bool, error) {
	actives, err := s.activeFactors(ctx, userID)
	if err != nil {
		return false, err
	}
	return len(actives) == 0, nil
}

func (s *Service) policyHere(ctx context.Context) (Policy, error) {
	c, _ := ctxutil.From(ctx)
	if c.AccountID == "" {
		// With no active account there is no policy to apply: enrolling is the
		// person's own decision, and the platform's default accepts the three.
		return Policy{}, nil
	}
	return s.repo.PolicyOf(ctx, c.AccountID)
}

func (s *Service) requiredHere(ctx context.Context) (bool, error) {
	p, err := s.policyHere(ctx)
	if err != nil {
		return false, err
	}
	return p.Required, nil
}

func (s *Service) assertAllowed(ctx context.Context, k Kind) error {
	if !ValidKind(k) {
		return errs.Invalid("unknown second factor kind: %q", k).
			WithCode(KeyKindUnknown, map[string]any{"kind": string(k)})
	}
	p, err := s.policyHere(ctx)
	if err != nil {
		return err
	}
	if !p.Accepts(k) {
		return errs.Precondition("this account does not accept this kind of second factor").
			WithCode(KeyKindNotAllowed, map[string]any{"kind": string(k)})
	}
	return nil
}

func (s *Service) generateRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	codes := make([]string, 0, RecoveryCodeCount)
	hashes := make([][]byte, 0, RecoveryCodeCount)
	for i := 0; i < RecoveryCodeCount; i++ {
		c, err := recoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, c)
		hashes = append(hashes, HashRecoveryCode(c))
	}
	if err := s.repo.ReplaceRecoveryCodes(ctx, userID, hashes, s.now()); err != nil {
		return nil, err
	}
	logging.From(ctx).Info("recovery codes generated", "count", len(codes))
	return codes, nil
}

// addressFor is what the authenticator app shows under the issuer. The e-mail
// when there is one — phone or passkey sign-in has none — and the user's id
// otherwise, which is ugly and unambiguous.
func addressFor(email, userID string) string {
	if strings.TrimSpace(email) != "" {
		return email
	}
	return userID
}

// numericCode draws SIX digits with crypto/rand.
//
// math/rand here would be a code predictable from the clock, which is exactly
// what a second factor exists to prevent — and it is the kind of mistake no test
// catches, because the code works.
func numericCode() (string, error) {
	max := big.NewInt(1_000_000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "drawing the code")
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// recoveryCode draws 80 bits and formats them in two readable groups. Base32
// without padding, uppercase: it is what a person copies off a screen and types
// back months later without confusing a 0 with an O — the alphabet has neither
// 0, 1, 8 nor 9 for that reason.
func recoveryCode() (string, error) {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "drawing the recovery code")
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	return s[:8] + "-" + s[8:], nil
}

// subtleEqual compares hashes in constant time. They are hashes, not secrets,
// and the cost of not creating a timing oracle is one function call.
func subtleEqual(a, b []byte) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
