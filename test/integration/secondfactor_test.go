//go:build integration

// Integration tests of the SECOND FACTOR against the real Postgres.
//
//	go test ./test/integration/ -tags=integration -run SecondFactor -v
//
// What is proven here and cannot be proven with a fake: the SQL. Two operations
// in this repository are atomic by CONSTRUCTION — RegisterAttempt counts and
// consumes in one statement, UseRecoveryCode reads and consumes in another — and
// an in-memory double would pass either way, including the version with the race
// (ADR-0020 §5; the cool-off is what turns 10^6 guesses into 5).
package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/barrosef/dop-core/internal/adapter/postgres"
	"github.com/barrosef/dop-core/internal/domain/secondfactor"
)

// aUser creates a user and returns its id, cleaning up afterwards. The factor
// hangs off the user by FK, so the cascade takes the rest.
func aUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	var id string
	subject := fmt.Sprintf("sub-2fa-%d", time.Now().UnixNano())
	err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, email, email_verified, name)
		VALUES ($1, $2, true, 'Dev')
		RETURNING id`, subject, subject+"@dop.test").Scan(&id)
	if err != nil {
		t.Fatalf("creating the user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func TestSecondFactorTheAttemptCounterDoesNotRace(t *testing.T) {
	pool := poolWithCleanup(t)
	repo := postgres.NewSecondFactorRepo(pool)
	ctx := context.Background()
	userID := aUser(t, pool)

	f, err := repo.CreateFactor(ctx, &secondfactor.Factor{
		UserID: userID, Kind: secondfactor.KindSMS, Status: secondfactor.StatusPending,
		Label: "iPhone", Destination: "+5511999999999",
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := repo.CreateChallenge(ctx, &secondfactor.Challenge{
		FactorID: f.ID, UserID: userID, Purpose: secondfactor.PurposeStepUp,
		CodeHash:  secondfactor.HashCode(f.ID, "123456"),
		ExpiresAt: time.Now().Add(secondfactor.CodeTTL),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Twenty wrong answers at the same time — the shape of a script. Every one
	// of them has to be COUNTED: with two statements (read, then write), they
	// would all read `attempts = 0` and the ceiling would never be reached.
	const tries = 20
	var wg sync.WaitGroup
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = repo.RegisterAttempt(ctx, ch.ID, false, time.Now())
		}()
	}
	wg.Wait()

	got, err := repo.ChallengeByID(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != tries {
		t.Fatalf("%d attempts counted out of %d — the counter races, and the cool-off is decorative",
			got.Attempts, tries)
	}
}

func TestSecondFactorARecoveryCodeIsConsumedOnlyOnce(t *testing.T) {
	pool := poolWithCleanup(t)
	repo := postgres.NewSecondFactorRepo(pool)
	ctx := context.Background()
	userID := aUser(t, pool)

	hash := secondfactor.HashRecoveryCode("ABCDEFGH-IJKLMNOP")
	if err := repo.ReplaceRecoveryCodes(ctx, userID, [][]byte{hash}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Ten simultaneous uses of the SAME code. Exactly one may win — a code that
	// worked twice would be a password.
	const tries = 10
	var wg sync.WaitGroup
	wins := make(chan bool, tries)
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repo.UseRecoveryCode(ctx, userID, hash, time.Now())
			if err == nil && ok {
				wins <- true
			}
		}()
	}
	wg.Wait()
	close(wins)

	n := 0
	for range wins {
		n++
	}
	if n != 1 {
		t.Fatalf("%d simultaneous uses succeeded — it has to be exactly 1", n)
	}
	left, err := repo.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil || left != 0 {
		t.Fatalf("%d codes left (err=%v)", left, err)
	}
}

func TestSecondFactorTheSameDestinationIsNotRegisteredTwice(t *testing.T) {
	pool := poolWithCleanup(t)
	repo := postgres.NewSecondFactorRepo(pool)
	ctx := context.Background()
	userID := aUser(t, pool)

	newFactor := func() error {
		_, err := repo.CreateFactor(ctx, &secondfactor.Factor{
			UserID: userID, Kind: secondfactor.KindSMS, Status: secondfactor.StatusPending,
			Label: "iPhone", Destination: "+5511999999999",
		})
		return err
	}
	if err := newFactor(); err != nil {
		t.Fatal(err)
	}
	// The same number twice is not a second factor: it is the same factor
	// registered twice, and it would double the SMS bill.
	if err := newFactor(); err == nil {
		t.Fatal("the same destination was registered twice")
	}

	// Revoked ones stay out of the rule — re-enrolling a number that was
	// removed is legitimate.
	list, _ := repo.ListFactors(ctx, userID)
	if err := repo.RevokeFactor(ctx, list[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := newFactor(); err != nil {
		t.Fatalf("re-enrolling a revoked number was refused: %v", err)
	}
}

func TestSecondFactorAnActiveFactorAlwaysHasBeenConfirmed(t *testing.T) {
	// The check constraint is what stops an UPDATE from elsewhere from marking a
	// factor active without a confirmation — the state that grants access
	// without a proof of possession.
	pool := poolWithCleanup(t)
	ctx := context.Background()
	userID := aUser(t, pool)

	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO second_factors (user_id, kind, status, label, destination)
		VALUES ($1, 'sms', 'pending', 'iPhone', '+5511999999999') RETURNING id`, userID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE second_factors SET status = 'active' WHERE id = $1`, id)
	if err == nil {
		t.Fatal("a factor became active with no confirmed_at — the constraint is not doing its job")
	}
}

func TestSecondFactorTheStepUpIsPerSessionAndSurvivesARewrite(t *testing.T) {
	pool := poolWithCleanup(t)
	repo := postgres.NewSecondFactorRepo(pool)
	ctx := context.Background()
	userID := aUser(t, pool)

	now := time.Now().UTC().Truncate(time.Second)
	if err := repo.SaveStepUp(ctx, secondfactor.StepUp{
		UserID: userID, SessionID: "sess-1", Method: secondfactor.KindTOTP,
		VerifiedAt: now, ExpiresAt: now.Add(secondfactor.StepUpTTL),
	}); err != nil {
		t.Fatal(err)
	}
	// Answering again in the same session EXTENDS it; it does not duplicate.
	if err := repo.SaveStepUp(ctx, secondfactor.StepUp{
		UserID: userID, SessionID: "sess-1", Method: secondfactor.KindSMS,
		VerifiedAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Minute).Add(secondfactor.StepUpTTL),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.StepUpFor(ctx, userID, "sess-1")
	if err != nil || got == nil {
		t.Fatalf("the step-up did not come back: %v", err)
	}
	if got.Method != secondfactor.KindSMS {
		t.Errorf("method = %q: the second answer did not overwrite the first", got.Method)
	}
	// Another session did NOT inherit it: two open sessions are two doors.
	other, err := repo.StepUpFor(ctx, userID, "sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if other != nil {
		t.Error("another session inherited the step-up")
	}
}

func TestSecondFactorThePolicyComesFromTheAccount(t *testing.T) {
	pool := poolWithCleanup(t)
	repo := postgres.NewSecondFactorRepo(pool)
	ctx := context.Background()

	var acctID string
	handle := fmt.Sprintf("acct-2fa-%d", time.Now().UnixNano())
	err := pool.QueryRow(ctx, `
		INSERT INTO accounts (kind, handle, display_name, legal_id,
		                      require_second_factor, allowed_second_factors)
		VALUES ('organization', $1, 'Acme', '00000000000191', true,
		        ARRAY['totp','email']::second_factor_kind[])
		RETURNING id`, handle).Scan(&acctID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acctID) })

	p, err := repo.PolicyOf(ctx, acctID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Required {
		t.Error("the requirement did not come back")
	}
	if p.Accepts(secondfactor.KindSMS) {
		t.Error("SMS was accepted in an account that does not allow it")
	}
	if !p.Accepts(secondfactor.KindTOTP) {
		t.Error("TOTP is allowed and was refused")
	}
}
