package secondfactor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/domain/secondfactor"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/totp"
)

// ── doubles ─────────────────────────────────────────────────────────────────

type memRepo struct {
	factors    map[string]*secondfactor.Factor
	challenges map[string]*secondfactor.Challenge
	recovery   map[string][][]byte // userID -> unused hashes
	stepUps    map[string]secondfactor.StepUp
	policy     secondfactor.Policy
	seq        int
}

func newRepo() *memRepo {
	return &memRepo{
		factors:    map[string]*secondfactor.Factor{},
		challenges: map[string]*secondfactor.Challenge{},
		recovery:   map[string][][]byte{},
		stepUps:    map[string]secondfactor.StepUp{},
	}
}

func (r *memRepo) id(prefix string) string {
	r.seq++
	return prefix + "-" + string(rune('a'+r.seq-1))
}

func (r *memRepo) ListFactors(_ context.Context, userID string) ([]secondfactor.Factor, error) {
	out := []secondfactor.Factor{}
	for _, f := range r.factors {
		if f.UserID == userID && f.Status != secondfactor.StatusRevoked {
			out = append(out, *f)
		}
	}
	return out, nil
}

func (r *memRepo) FactorByID(_ context.Context, id string) (*secondfactor.Factor, error) {
	f, ok := r.factors[id]
	if !ok {
		return nil, nil
	}
	c := *f
	return &c, nil
}

func (r *memRepo) CreateFactor(_ context.Context, f *secondfactor.Factor) (*secondfactor.Factor, error) {
	// The live-uniqueness rule is the database's; the double reproduces it so
	// the domain test sees the same refusal.
	for _, e := range r.factors {
		if e.UserID == f.UserID && e.Kind == f.Kind && e.Destination == f.Destination &&
			e.Status != secondfactor.StatusRevoked {
			return nil, errs.Conflict("factor already registered")
		}
	}
	c := *f
	c.ID = r.id("fac")
	r.factors[c.ID] = &c
	out := c
	return &out, nil
}

func (r *memRepo) ActivateFactor(_ context.Context, id string, at time.Time) (*secondfactor.Factor, error) {
	f, ok := r.factors[id]
	if !ok {
		return nil, errs.NotFound("factor")
	}
	if !at.IsZero() {
		f.Status = secondfactor.StatusActive
		f.ConfirmedAt = &at
	}
	c := *f
	return &c, nil
}

func (r *memRepo) RevokeFactor(_ context.Context, id string, _ time.Time) error {
	if f, ok := r.factors[id]; ok {
		f.Status = secondfactor.StatusRevoked
	}
	return nil
}

func (r *memRepo) TouchFactor(_ context.Context, id string, at time.Time) error {
	if f, ok := r.factors[id]; ok {
		f.LastUsedAt = &at
	}
	return nil
}

func (r *memRepo) CreateChallenge(_ context.Context, c *secondfactor.Challenge) (*secondfactor.Challenge, error) {
	cp := *c
	cp.ID = r.id("chl")
	r.challenges[cp.ID] = &cp
	out := cp
	return &out, nil
}

func (r *memRepo) ChallengeByID(_ context.Context, id string) (*secondfactor.Challenge, error) {
	c, ok := r.challenges[id]
	if !ok {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (r *memRepo) RegisterAttempt(_ context.Context, id string, consumed bool, at time.Time) (*secondfactor.Challenge, error) {
	c, ok := r.challenges[id]
	if !ok {
		return nil, errs.NotFound("challenge")
	}
	c.Attempts++
	if consumed {
		c.ConsumedAt = &at
	}
	cp := *c
	return &cp, nil
}

func (r *memRepo) ChallengesSince(_ context.Context, factorID string, since time.Time) (int, time.Time, error) {
	count := 0
	var lastPending time.Time
	for _, c := range r.challenges {
		if c.FactorID != factorID || c.CreatedAt.Before(since) {
			continue
		}
		count++
		// Only what has NOT been answered holds the floor — see the port.
		if c.ConsumedAt == nil && c.CreatedAt.After(lastPending) {
			lastPending = c.CreatedAt
		}
	}
	return count, lastPending, nil
}

func (r *memRepo) ReplaceRecoveryCodes(_ context.Context, userID string, hashes [][]byte, _ time.Time) error {
	r.recovery[userID] = hashes
	return nil
}

func (r *memRepo) UseRecoveryCode(_ context.Context, userID string, hash []byte, _ time.Time) (bool, error) {
	list := r.recovery[userID]
	for i, h := range list {
		if string(h) == string(hash) {
			r.recovery[userID] = append(append([][]byte{}, list[:i]...), list[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (r *memRepo) CountUnusedRecoveryCodes(_ context.Context, userID string) (int, error) {
	return len(r.recovery[userID]), nil
}

func (r *memRepo) SaveStepUp(_ context.Context, s secondfactor.StepUp) error {
	r.stepUps[s.UserID+"/"+s.SessionID] = s
	return nil
}

func (r *memRepo) StepUpFor(_ context.Context, userID, sessionID string) (*secondfactor.StepUp, error) {
	s, ok := r.stepUps[userID+"/"+sessionID]
	if !ok {
		return nil, nil
	}
	return &s, nil
}

func (r *memRepo) DeleteStepUpsOf(_ context.Context, userID string) error {
	for k, s := range r.stepUps {
		if s.UserID == userID {
			delete(r.stepUps, k)
		}
	}
	return nil
}

func (r *memRepo) PolicyOf(_ context.Context, _ string) (secondfactor.Policy, error) {
	return r.policy, nil
}

type fakeUsers struct {
	email    string
	verified bool
	// phones records what Confirm reported; a pointer so the fixture's copy
	// and the test's read the same slice.
	phones *[]string
}

func (u fakeUsers) PhoneVerified(_ context.Context, _ string, destination string) error {
	if u.phones != nil {
		*u.phones = append(*u.phones, destination)
	}
	return nil
}

func (u fakeUsers) UserProfile(context.Context, string) (string, string, bool, error) {
	return u.email, "Dev", u.verified, nil
}
func (u fakeUsers) PersonalAccountOf(context.Context, string) (string, error) {
	return "acct-personal", nil
}

type memVault struct{ data map[string][]byte }

func (v *memVault) key(r ports.SecretRef) string { return r.AccountID + "/" + r.Kind + "/" + r.OwnerID }
func (v *memVault) Put(_ context.Context, r ports.SecretRef, val ports.SecretValue) error {
	v.data[v.key(r)] = []byte(val)
	return nil
}
func (v *memVault) Get(_ context.Context, r ports.SecretRef) (ports.SecretValue, error) {
	return v.data[v.key(r)], nil
}
func (v *memVault) Delete(_ context.Context, r ports.SecretRef) error {
	delete(v.data, v.key(r))
	return nil
}
func (v *memVault) Exists(_ context.Context, r ports.SecretRef) (bool, error) {
	_, ok := v.data[v.key(r)]
	return ok, nil
}

type sentMail struct {
	to   string
	data map[string]any
}
type fakeMailer struct{ sent []sentMail }

func (m *fakeMailer) Send(_ context.Context, mail ports.Mail) (*ports.MailReceipt, error) {
	m.sent = append(m.sent, sentMail{to: mail.To, data: mail.Data})
	return &ports.MailReceipt{State: ports.MailSent, Provider: "fake"}, nil
}
func (m *fakeMailer) Resolve(context.Context, string) error { return nil }

type sentSMS struct{ to, text string }
type fakeSMS struct{ sent []sentSMS }

func (s *fakeSMS) Send(_ context.Context, m ports.SMS) (*ports.SMSReceipt, error) {
	s.sent = append(s.sent, sentSMS{to: m.To, text: m.Text})
	return &ports.SMSReceipt{State: ports.SMSSent, Provider: "fake"}, nil
}

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

type fixture struct {
	svc    *secondfactor.Service
	repo   *memRepo
	vault  *memVault
	mailer *fakeMailer
	sms    *fakeSMS
	clock  *fixedClock
}

func newFixture(t *testing.T, users fakeUsers) *fixture {
	t.Helper()
	repo := newRepo()
	vault := &memVault{data: map[string][]byte{}}
	mailer := &fakeMailer{}
	sms := &fakeSMS{}
	clk := &fixedClock{t: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	return &fixture{
		svc:  secondfactor.NewService(repo, users, vault, mailer, sms, clk),
		repo: repo, vault: vault, mailer: mailer, sms: sms, clock: clk,
	}
}

func ctxOf(session string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: "u-1", ActorKind: ctxutil.ActorUser, AccountID: "acct-1", SessionID: session,
	})
}

// ── enrolment ───────────────────────────────────────────────────────────────

func TestATOTPFactorIsBornPendingAndTheSeedGoesToTheVault(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")

	enr, err := f.svc.EnrollTOTP(ctx, "iPhone")
	if err != nil {
		t.Fatal(err)
	}
	if enr.Factor.Status != secondfactor.StatusPending {
		t.Errorf("it was born %s — a factor that is not proven grants nothing", enr.Factor.Status)
	}
	if enr.Secret == "" || !strings.Contains(enr.URI, "otpauth://totp/") {
		t.Fatal("the enrolment has to return the seed and the URI — it is the only moment they exist")
	}
	if len(f.vault.data) != 1 {
		t.Fatalf("the seed did not reach the vault: %d entries", len(f.vault.data))
	}
	for _, v := range f.vault.data {
		if string(v) != enr.Secret {
			t.Error("the vault has a different seed from the one shown")
		}
	}
}

func TestTheSeedNeverComesBackInAListing(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	enr, _ := f.svc.EnrollTOTP(ctx, "iPhone")

	list, err := f.svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("%d factors", len(list))
	}
	if list[0].SecretRef != "" {
		t.Error("the listing carries the vault's reference — it is not the screen's business")
	}
	if strings.Contains(list[0].Destination, enr.Secret) {
		t.Error("the seed leaked into the listing")
	}
}

func TestConfirmingWithTheAppsCodeActivatesAndReturnsTheRecoveryCodes(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	enr, _ := f.svc.EnrollTOTP(ctx, "iPhone")

	// A pending factor does not answer a STEP-UP challenge: enrolling must not
	// authenticate.
	if _, err := f.svc.Challenge(ctx, enr.Factor.ID); err == nil {
		t.Fatal("a pending factor answered a step-up challenge")
	}

	code, _ := totp.Code(enr.Secret, f.clock.t)
	codes, err := f.svc.Confirm(ctx, enr.Factor.ID, enr.ChallengeID, code)
	if err != nil {
		t.Fatalf("confirmation refused: %v", err)
	}
	if len(codes) != secondfactor.RecoveryCodeCount {
		t.Fatalf("%d recovery codes, want %d", len(codes), secondfactor.RecoveryCodeCount)
	}
	list, _ := f.svc.List(ctx)
	if !list[0].IsActive() {
		t.Error("the factor did not become active after the proof")
	}
}

func TestAnEmailFactorRequiresTheSessionsVerifiedAddress(t *testing.T) {
	// Unverified: an unproven address as a second factor is the same unproven
	// address the first factor already trusted (ADR-0019's guarantee 5).
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: false})
	if _, _, err := f.svc.EnrollCode(ctxOf("s"), secondfactor.KindEmail, "work", "dev@dop.local"); err == nil {
		t.Fatal("an unverified address was accepted as a second factor")
	}

	// Verified, but ANOTHER address: it would be a factor pointing somewhere the
	// platform never proved the person reads.
	g := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	if _, _, err := g.svc.EnrollCode(ctxOf("s"), secondfactor.KindEmail, "other", "other@dop.local"); err == nil {
		t.Fatal("an address other than the session's was accepted")
	}
}

func TestEnrollingByCodeSendsThroughTheChannelAndStoresOnlyTheHash(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")

	factor, challengeID, err := f.svc.EnrollCode(ctx, secondfactor.KindSMS, "iPhone", "+5511999999999")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sms.sent) != 1 {
		t.Fatalf("%d messages sent", len(f.sms.sent))
	}
	if f.sms.sent[0].to != "+5511999999999" {
		t.Errorf("it went to %q", f.sms.sent[0].to)
	}
	// The destination comes back MASKED: a screen needs enough to recognize it
	// and not enough to use it.
	if factor.Destination != "*********9999" { // 13 digits: 9 hidden, the last 4 kept
		t.Errorf("unmasked destination: %q", factor.Destination)
	}

	// The code is in the message and NOT in the row: what is stored is the hash.
	ch := f.repo.challenges[challengeID]
	if ch.CodeHash == nil {
		t.Fatal("the challenge did not store the hash")
	}
	code := codeFromText(t, f.sms.sent[0].text)
	if strings.Contains(string(ch.CodeHash), code) {
		t.Error("the code is readable in the stored row")
	}
	if !strings.Contains(f.sms.sent[0].text, code) {
		t.Error("the message does not carry the code")
	}
}

func TestAWrongCodeCountsAnAttemptAndFiveExhaustTheChallenge(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor, challengeID, _ := f.svc.EnrollCode(ctx, secondfactor.KindSMS, "iPhone", "+5511999999999")

	for i := 0; i < secondfactor.MaxAttempts; i++ {
		if _, err := f.svc.Confirm(ctx, factor.ID, challengeID, "000000"); err == nil {
			t.Fatalf("attempt %d: a wrong code was accepted", i+1)
		}
	}
	// The sixth does not even get compared: the challenge is spent, and saying
	// so is what turns 10^6 guesses into 5.
	_, err := f.svc.Confirm(ctx, factor.ID, challengeID, "000000")
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("after %d attempts the refusal was %v (%s)", secondfactor.MaxAttempts, err, errs.KindOf(err))
	}
	code, _ := errs.CodeOf(err)
	if code != secondfactor.KeyChallengeExhausted {
		t.Errorf("translation key = %q", code)
	}
}

func TestAnExpiredCodeIsRefused(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor, challengeID, _ := f.svc.EnrollCode(ctx, secondfactor.KindEmail, "work", "dev@dop.local")
	code := codeFromMail(t, f.mailer.sent[0].data)

	f.clock.t = f.clock.t.Add(secondfactor.CodeTTL + time.Second)
	_, err := f.svc.Confirm(ctx, factor.ID, challengeID, code)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("an expired code gave %v", err)
	}
	if k, _ := errs.CodeOf(err); k != secondfactor.KeyChallengeExpired {
		t.Errorf("key = %q", k)
	}
}

func TestACodeIsSingleUse(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor, challengeID, _ := f.svc.EnrollCode(ctx, secondfactor.KindEmail, "work", "dev@dop.local")
	code := codeFromMail(t, f.mailer.sent[0].data)

	if _, err := f.svc.Confirm(ctx, factor.ID, challengeID, code); err != nil {
		t.Fatal(err)
	}
	// Reusing it would turn an intercepted message into a permanent key.
	if _, err := f.svc.Confirm(ctx, factor.ID, challengeID, code); err == nil {
		t.Fatal("the code worked twice")
	}
}

// ── the step-up ─────────────────────────────────────────────────────────────

func TestTheStepUpIsPerSession(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	first := ctxOf("sess-1")
	factor := activeSMSFactor(t, f, first)

	ch, err := f.svc.Challenge(first, factor.ID)
	if err != nil {
		t.Fatal(err)
	}
	code := codeFromText(t, f.sms.sent[len(f.sms.sent)-1].text)
	if _, err := f.svc.Verify(first, ch.ID, code); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.RequireStepUp(first); err != nil {
		t.Fatalf("the session that answered was refused: %v", err)
	}
	// Two open sessions are two doors: one of them answering must not open the
	// other.
	if err := f.svc.RequireStepUp(ctxOf("sess-2")); err == nil {
		t.Fatal("another session inherited the step-up")
	}
}

func TestTheStepUpExpires(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor := activeSMSFactor(t, f, ctx)
	ch, _ := f.svc.Challenge(ctx, factor.ID)
	code := codeFromText(t, f.sms.sent[len(f.sms.sent)-1].text)
	if _, err := f.svc.Verify(ctx, ch.ID, code); err != nil {
		t.Fatal(err)
	}

	f.clock.t = f.clock.t.Add(secondfactor.StepUpTTL + time.Minute)
	if err := f.svc.RequireStepUp(ctx); err == nil {
		t.Fatal("an expired step-up still opened the gate")
	}
}

func TestWithNoActiveFactorTheGateOnlyRefusesWhereTheAccountRequiresIt(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")

	// Nobody has enrolled yet: refusing here would make the platform unusable
	// the day the feature ships.
	if err := f.svc.RequireStepUp(ctx); err != nil {
		t.Fatalf("with no factor and no requirement it refused: %v", err)
	}

	f.repo.policy = secondfactor.Policy{Required: true}
	err := f.svc.RequireStepUp(ctx)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("with a requirement and no factor it gave %v", err)
	}
	if k, _ := errs.CodeOf(err); k != secondfactor.KeyNoActiveFactor {
		t.Errorf("key = %q", k)
	}
}

func TestAnEnrolmentChallengeDoesNotStepTheSessionUp(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor, enrollID, _ := f.svc.EnrollCode(ctx, secondfactor.KindSMS, "iPhone", "+5511999999999")
	code := codeFromText(t, f.sms.sent[0].text)

	// Answering the ENROLMENT challenge through Verify would be enrolling in
	// order to authenticate.
	if _, err := f.svc.Verify(ctx, enrollID, code); err == nil {
		t.Fatal("an enrolment challenge stepped the session up")
	}
	_ = factor
}

// ── recovery codes ──────────────────────────────────────────────────────────

func TestARecoveryCodeStepsUpAndDiesOnUse(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	codes := activeSMSFactorWithRecovery(t, f, ctx)

	if _, err := f.svc.VerifyRecoveryCode(ctx, codes[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RequireStepUp(ctx); err != nil {
		t.Fatalf("the recovery code did not step the session up: %v", err)
	}
	// It dies on use: a code that worked twice would be a password.
	if _, err := f.svc.VerifyRecoveryCode(ctxOf("sess-9"), codes[0]); err == nil {
		t.Fatal("the recovery code worked twice")
	}

	left, _ := f.svc.RemainingRecoveryCodes(ctx)
	if left != secondfactor.RecoveryCodeCount-1 {
		t.Errorf("%d codes left, want %d", left, secondfactor.RecoveryCodeCount-1)
	}
}

func TestAnInvalidRecoveryCodeDoesNotSayWhy(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	activeSMSFactorWithRecovery(t, f, ctx)

	_, err := f.svc.VerifyRecoveryCode(ctx, "AAAAAAAA-AAAAAAAA")
	if errs.KindOf(err) != errs.KindPermission {
		t.Fatalf("an unknown code gave %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "used") {
		t.Error("the message tells 'does not exist' from 'already used' — it is an oracle")
	}
}

// ── policy ──────────────────────────────────────────────────────────────────

func TestAnAccountMayRefuseAKind(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	f.repo.policy = secondfactor.Policy{Allowed: []secondfactor.Kind{secondfactor.KindTOTP}}
	ctx := ctxOf("sess-1")

	if _, _, err := f.svc.EnrollCode(ctx, secondfactor.KindSMS, "iPhone", "+5511999999999"); err == nil {
		t.Fatal("SMS was accepted in an account that does not allow it")
	}
	if _, err := f.svc.EnrollTOTP(ctx, "iPhone"); err != nil {
		t.Fatalf("TOTP is allowed and was refused: %v", err)
	}
}

func TestRevokingTheLastFactorIsRefusedWhereTheAccountRequiresOne(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor := activeSMSFactor(t, f, ctx)

	f.repo.policy = secondfactor.Policy{Required: true}
	err := f.svc.Revoke(ctx, factor.ID)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("removing the last factor gave %v", err)
	}
	if k, _ := errs.CodeOf(err); k != secondfactor.KeyLastFactorRequired {
		t.Errorf("key = %q", k)
	}

	// With no requirement, the person rules over their own account.
	f.repo.policy = secondfactor.Policy{}
	if err := f.svc.Revoke(ctx, factor.ID); err != nil {
		t.Fatalf("with no requirement it refused: %v", err)
	}
}

func TestAnotherPersonsFactorIsNotFound(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	mine := activeSMSFactor(t, f, ctxOf("sess-1"))

	other := ctxutil.Into(context.Background(), ctxutil.Call{
		ActorID: "u-2", ActorKind: ctxutil.ActorUser, AccountID: "acct-1", SessionID: "s",
	})
	// NOT FOUND, and not "forbidden": whoever is outside should not even
	// discover that the id exists.
	err := f.svc.Revoke(other, mine.ID)
	if errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("another person's factor gave %v (%s)", err, errs.KindOf(err))
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func activeSMSFactor(t *testing.T, f *fixture, ctx context.Context) *secondfactor.Factor {
	t.Helper()
	factor, challengeID, err := f.svc.EnrollCode(ctx, secondfactor.KindSMS, "iPhone", "+5511999999999")
	if err != nil {
		t.Fatal(err)
	}
	code := codeFromText(t, f.sms.sent[len(f.sms.sent)-1].text)
	if _, err := f.svc.Confirm(ctx, factor.ID, challengeID, code); err != nil {
		t.Fatal(err)
	}
	return factor
}

func activeSMSFactorWithRecovery(t *testing.T, f *fixture, ctx context.Context) []string {
	t.Helper()
	factor, challengeID, err := f.svc.EnrollCode(ctx, secondfactor.KindSMS, "iPhone", "+5511999999999")
	if err != nil {
		t.Fatal(err)
	}
	code := codeFromText(t, f.sms.sent[len(f.sms.sent)-1].text)
	codes, err := f.svc.Confirm(ctx, factor.ID, challengeID, code)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) == 0 {
		t.Fatal("the first factor did not generate recovery codes")
	}
	return codes
}

// codeFromText pulls the six digits out of the message. The test reads what the
// person reads — asserting against the stored hash would prove nothing about
// what was sent.
func codeFromText(t *testing.T, text string) string {
	t.Helper()
	digits := []rune{}
	for _, r := range text {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
			if len(digits) == 6 {
				return string(digits)
			}
			continue
		}
		digits = digits[:0]
	}
	t.Fatalf("no code in the message: %q", text)
	return ""
}

func codeFromMail(t *testing.T, data map[string]any) string {
	t.Helper()
	c, ok := data["code"].(string)
	if !ok || len(c) != 6 {
		t.Fatalf("the e-mail did not carry the code: %v", data)
	}
	return c
}

// ── the send ceiling, and the revocation ────────────────────────────────────

func TestASecondCodeIsNotSentWhileTheFirstIsPending(t *testing.T) {
	// The floor is against the second click and against the loop: with e-mail
	// and SMS every challenge is a message, and with SMS it is money.
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor := activeSMSFactor(t, f, ctx)
	sentSoFar := len(f.sms.sent)

	if _, err := f.svc.Challenge(ctx, factor.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Challenge(ctx, factor.ID)
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("a second immediate code gave %v", err)
	}
	if k, _ := errs.CodeOf(err); k != secondfactor.KeyResendTooSoon {
		t.Errorf("key = %q", k)
	}
	if len(f.sms.sent) != sentSoFar+1 {
		t.Errorf("%d messages sent, want 1", len(f.sms.sent)-sentSoFar)
	}

	// After the floor, asking again is legitimate.
	f.clock.t = f.clock.t.Add(secondfactor.ResendInterval + time.Second)
	if _, err := f.svc.Challenge(ctx, factor.ID); err != nil {
		t.Fatalf("after the interval it still refused: %v", err)
	}
}

func TestAnsweringCorrectlyDoesNotCostTheWait(t *testing.T) {
	// The floor hangs off the PENDING challenge. Whoever verified a code must
	// not be made to wait a minute for having succeeded.
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor := activeSMSFactor(t, f, ctx)

	ch, err := f.svc.Challenge(ctx, factor.ID)
	if err != nil {
		t.Fatal(err)
	}
	code := codeFromText(t, f.sms.sent[len(f.sms.sent)-1].text)
	if _, err := f.svc.Verify(ctx, ch.ID, code); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Challenge(ctx, factor.ID); err != nil {
		t.Fatalf("after answering correctly it refused: %v", err)
	}
}

func TestTheHourlyCeilingStopsTheLoop(t *testing.T) {
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	factor := activeSMSFactor(t, f, ctx)

	// Walking the clock forward between requests defeats the floor — which is
	// exactly what a script does. The ceiling is what is left.
	var err error
	for i := 0; i < secondfactor.MaxChallengesPerHour+2; i++ {
		f.clock.t = f.clock.t.Add(secondfactor.ResendInterval + time.Second)
		_, err = f.svc.Challenge(ctx, factor.ID)
		if err != nil {
			break
		}
	}
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("the loop was not stopped: %v", err)
	}
	if k, _ := errs.CodeOf(err); k != secondfactor.KeyTooManyChallenges {
		t.Errorf("key = %q", k)
	}

	// An hour later the window has moved and it is possible again.
	f.clock.t = f.clock.t.Add(secondfactor.ChallengeWindow + time.Minute)
	if _, err := f.svc.Challenge(ctx, factor.ID); err != nil {
		t.Fatalf("after the window it still refused: %v", err)
	}
}

func TestATOTPChallengeIsNotThrottled(t *testing.T) {
	// It sends nothing: throttling would make the screen refuse for no reason.
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	enr, _ := f.svc.EnrollTOTP(ctx, "iPhone")
	code, _ := totp.Code(enr.Secret, f.clock.t)
	if _, err := f.svc.Confirm(ctx, enr.Factor.ID, enr.ChallengeID, code); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < secondfactor.MaxChallengesPerHour+3; i++ {
		if _, err := f.svc.Challenge(ctx, enr.Factor.ID); err != nil {
			t.Fatalf("attempt %d on a TOTP was throttled: %v", i+1, err)
		}
	}
}

func TestRevokingAFactorDropsEveryStepUp(t *testing.T) {
	// Whoever revokes a factor is saying "the device I had is no longer mine".
	// Leaving a session open would keep the door the removal was meant to close.
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	other := ctxOf("sess-2")
	factor := activeSMSFactor(t, f, ctx)

	// Two sessions stepped up.
	for _, c := range []context.Context{ctx, other} {
		f.clock.t = f.clock.t.Add(secondfactor.ResendInterval + time.Second)
		ch, err := f.svc.Challenge(c, factor.ID)
		if err != nil {
			t.Fatal(err)
		}
		code := codeFromText(t, f.sms.sent[len(f.sms.sent)-1].text)
		if _, err := f.svc.Verify(c, ch.ID, code); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.RequireStepUp(other); err != nil {
		t.Fatalf("the second session was not stepped up: %v", err)
	}

	if err := f.svc.Revoke(ctx, factor.ID); err != nil {
		t.Fatal(err)
	}
	// With no active factor left and no requirement, the gate lets through —
	// what has to be gone is the RECORD, not the permission.
	if su, _ := f.repo.StepUpFor(ctx, "u-1", "sess-2"); su != nil {
		t.Error("a stepped-up session survived the revocation")
	}
	if su, _ := f.repo.StepUpFor(ctx, "u-1", "sess-1"); su != nil {
		t.Error("the revoking session kept its step-up")
	}
}

func TestConfirmingStepsTheSessionUp(t *testing.T) {
	// The person proved possession seconds ago, in this session: asking again
	// would add nothing and would cost a second message.
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true})
	ctx := ctxOf("sess-1")
	activeSMSFactor(t, f, ctx)

	if err := f.svc.RequireStepUp(ctx); err != nil {
		t.Fatalf("after confirming, the gate still refused: %v", err)
	}
	// And only THIS session: confirming is not a master key.
	if err := f.svc.RequireStepUp(ctxOf("sess-2")); err == nil {
		t.Error("confirming stepped up another session too")
	}
}

func TestConfirmingAnSMSFactorReportsThePhone(t *testing.T) {
	var phones []string
	f := newFixture(t, fakeUsers{email: "dev@dop.local", verified: true, phones: &phones})
	ctx := ctxOf("sess-1")

	factor, challengeID, err := f.svc.EnrollCode(ctx, secondfactor.KindSMS, "iPhone", "+5511999999999")
	if err != nil {
		t.Fatal(err)
	}
	code := codeFromText(t, f.sms.sent[0].text)
	if _, err := f.svc.Confirm(ctx, factor.ID, challengeID, code); err != nil {
		t.Fatal(err)
	}
	if len(phones) != 1 || phones[0] != "+5511999999999" {
		t.Errorf("identity should be told the UNMASKED number once, got %v", phones)
	}

	// A TOTP factor says nothing about a phone.
	enr, _ := f.svc.EnrollTOTP(ctx, "app")
	totpCode, _ := totp.Code(enr.Secret, f.clock.t)
	if _, err := f.svc.Confirm(ctx, enr.Factor.ID, enr.ChallengeID, totpCode); err != nil {
		t.Fatal(err)
	}
	if len(phones) != 1 {
		t.Errorf("a TOTP confirmation must not report a phone: %v", phones)
	}
}
