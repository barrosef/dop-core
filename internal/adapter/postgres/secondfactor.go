package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/domain/secondfactor"
)

// SecondFactorRepo implements secondfactor.Repository. It is the ONLY place
// with the second factor's SQL — the domain never sees a query.
type SecondFactorRepo struct{ pool *pgxpool.Pool }

func NewSecondFactorRepo(pool *pgxpool.Pool) *SecondFactorRepo { return &SecondFactorRepo{pool: pool} }

const factorCols = `id, user_id, kind, status, label,
	coalesce(secret_ref, ''), coalesce(destination, ''),
	confirmed_at, last_used_at, created_at, updated_at`

func scanFactor(row pgx.Row) (*secondfactor.Factor, error) {
	var f secondfactor.Factor
	err := row.Scan(&f.ID, &f.UserID, &f.Kind, &f.Status, &f.Label,
		&f.SecretRef, &f.Destination, &f.ConfirmedAt, &f.LastUsedAt, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ListFactors leaves the REVOKED ones out: they are kept in the table for the
// trail (who had what, and until when), and they are not part of the person's
// answer to "which factors do I have".
func (r *SecondFactorRepo) ListFactors(ctx context.Context, userID string) ([]secondfactor.Factor, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+factorCols+`
		  FROM second_factors
		 WHERE user_id = $1 AND status <> 'revoked'
		 ORDER BY created_at`, userID)
	if err != nil {
		return nil, Translate(err, "second factor")
	}
	defer rows.Close()

	out := []secondfactor.Factor{}
	for rows.Next() {
		f, err := scanFactor(rows)
		if err != nil {
			return nil, Translate(err, "second factor")
		}
		out = append(out, *f)
	}
	return out, Translate(rows.Err(), "second factor")
}

// FactorByID returns (nil, nil) when there is none: absence is a normal answer
// here — the domain is the one that decides whether it is NOT_FOUND or another
// person's factor.
func (r *SecondFactorRepo) FactorByID(ctx context.Context, id string) (*secondfactor.Factor, error) {
	f, err := scanFactor(r.pool.QueryRow(ctx, `SELECT `+factorCols+` FROM second_factors WHERE id = $1`, id))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "second factor")
	}
	return f, nil
}

func (r *SecondFactorRepo) CreateFactor(ctx context.Context, f *secondfactor.Factor) (*secondfactor.Factor, error) {
	saved, err := scanFactor(r.pool.QueryRow(ctx, `
		INSERT INTO second_factors (user_id, kind, status, label, secret_ref, destination)
		VALUES ($1, $2, $3, $4, nullif($5,''), nullif($6,''))
		RETURNING `+factorCols,
		f.UserID, string(f.Kind), string(f.Status), f.Label, f.SecretRef, f.Destination))
	if err != nil {
		return nil, Translate(err, "second factor")
	}
	return saved, nil
}

// ActivateFactor does two things with one signature, and the zero instant is
// what tells them apart: with a zero `at` it only records the vault reference
// (the TOTP enrolment, which writes the row before knowing the reference); with
// a real instant it CONFIRMS — status active and confirmed_at stamped.
//
// The check constraint `active_was_confirmed` is what stops the second form from
// ever being faked by an UPDATE from elsewhere.
func (r *SecondFactorRepo) ActivateFactor(ctx context.Context, id string, at time.Time) (*secondfactor.Factor, error) {
	if at.IsZero() {
		f, err := scanFactor(r.pool.QueryRow(ctx, `
			UPDATE second_factors SET updated_at = now() WHERE id = $1 RETURNING `+factorCols, id))
		return f, Translate(err, "second factor")
	}
	f, err := scanFactor(r.pool.QueryRow(ctx, `
		UPDATE second_factors
		   SET status = 'active', confirmed_at = $2, updated_at = now()
		 WHERE id = $1
		RETURNING `+factorCols, id, at))
	return f, Translate(err, "second factor")
}

func (r *SecondFactorRepo) RevokeFactor(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE second_factors SET status = 'revoked', updated_at = $2 WHERE id = $1`, id, at)
	return Translate(err, "second factor")
}

func (r *SecondFactorRepo) TouchFactor(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE second_factors SET last_used_at = $2 WHERE id = $1`, id, at)
	return Translate(err, "second factor")
}

// ── challenges ──────────────────────────────────────────────────────────────

const challengeCols = `id, factor_id, user_id, purpose, code_hash, attempts, consumed_at, expires_at, created_at`

func scanChallenge(row pgx.Row) (*secondfactor.Challenge, error) {
	var c secondfactor.Challenge
	err := row.Scan(&c.ID, &c.FactorID, &c.UserID, &c.Purpose, &c.CodeHash,
		&c.Attempts, &c.ConsumedAt, &c.ExpiresAt, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *SecondFactorRepo) CreateChallenge(ctx context.Context, c *secondfactor.Challenge) (*secondfactor.Challenge, error) {
	saved, err := scanChallenge(r.pool.QueryRow(ctx, `
		INSERT INTO second_factor_challenges (factor_id, user_id, purpose, code_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+challengeCols,
		c.FactorID, c.UserID, string(c.Purpose), c.CodeHash, c.ExpiresAt))
	if err != nil {
		return nil, Translate(err, "challenge")
	}
	return saved, nil
}

func (r *SecondFactorRepo) ChallengeByID(ctx context.Context, id string) (*secondfactor.Challenge, error) {
	c, err := scanChallenge(r.pool.QueryRow(ctx, `SELECT `+challengeCols+`
		FROM second_factor_challenges WHERE id = $1`, id))
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "challenge")
	}
	return c, nil
}

// RegisterAttempt counts and, when the answer was right, consumes — in ONE
// statement.
//
// Two statements would leave the window where the attempt is not yet counted,
// and that window is exactly what a script uses: N parallel requests, all of
// them reading `attempts = 0`.
func (r *SecondFactorRepo) RegisterAttempt(ctx context.Context, id string, consumed bool, at time.Time) (*secondfactor.Challenge, error) {
	c, err := scanChallenge(r.pool.QueryRow(ctx, `
		UPDATE second_factor_challenges
		   SET attempts = attempts + 1,
		       consumed_at = CASE WHEN $2 THEN $3 ELSE consumed_at END
		 WHERE id = $1
		RETURNING `+challengeCols, id, consumed, at))
	return c, Translate(err, "challenge")
}

// ChallengesSince counts what was opened in the window and reports the last
// PENDING one's instant — in ONE query, because they answer the same question
// ("may I send another?") and two queries would open the window between them.
//
// The `FILTER` is the whole point: the ceiling counts everything (a consumed
// message cost the same), the floor looks only at what has not been answered
// yet. Without it, whoever verified a code correctly would be made to wait a
// minute for having succeeded.
//
// The index `second_factor_challenges (factor_id, created_at DESC)` is what
// keeps this cheap: it runs on every send.
func (r *SecondFactorRepo) ChallengesSince(ctx context.Context, factorID string, since time.Time) (int, time.Time, error) {
	var count int
	var last *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT count(*),
		       max(created_at) FILTER (WHERE consumed_at IS NULL)
		  FROM second_factor_challenges
		 WHERE factor_id = $1 AND created_at >= $2`, factorID, since).Scan(&count, &last)
	if err != nil {
		return 0, time.Time{}, Translate(err, "challenges")
	}
	if last == nil {
		return count, time.Time{}, nil
	}
	return count, *last, nil
}

// ── recovery codes ──────────────────────────────────────────────────────────

// ReplaceRecoveryCodes swaps the WHOLE set in one transaction: generating new
// ones invalidates the old, and a half-done swap would leave the person with
// codes from two generations, none of which they can tell apart.
func (r *SecondFactorRepo) ReplaceRecoveryCodes(ctx context.Context, userID string, hashes [][]byte, at time.Time) error {
	return InTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1`, userID); err != nil {
			return Translate(err, "recovery codes")
		}
		for _, h := range hashes {
			if _, err := tx.Exec(ctx, `
				INSERT INTO recovery_codes (user_id, code_hash, created_at) VALUES ($1, $2, $3)`,
				userID, h, at); err != nil {
				return Translate(err, "recovery codes")
			}
		}
		return nil
	})
}

// UseRecoveryCode consumes by hash and answers whether it was still unused —
// atomically, for RegisterAttempt's same reason.
func (r *SecondFactorRepo) UseRecoveryCode(ctx context.Context, userID string, hash []byte, at time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE recovery_codes SET used_at = $3
		 WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, userID, hash, at)
	if err != nil {
		return false, Translate(err, "recovery code")
	}
	return tag.RowsAffected() == 1, nil
}

func (r *SecondFactorRepo) CountUnusedRecoveryCodes(ctx context.Context, userID string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID).Scan(&n)
	return n, Translate(err, "recovery codes")
}

// ── step-up ─────────────────────────────────────────────────────────────────

func (r *SecondFactorRepo) SaveStepUp(ctx context.Context, s secondfactor.StepUp) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO step_ups (user_id, session_id, method, verified_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, session_id) DO UPDATE
		   SET method = EXCLUDED.method,
		       verified_at = EXCLUDED.verified_at,
		       expires_at = EXCLUDED.expires_at`,
		s.UserID, s.SessionID, string(s.Method), s.VerifiedAt, s.ExpiresAt)
	return Translate(err, "step-up")
}

func (r *SecondFactorRepo) StepUpFor(ctx context.Context, userID, sessionID string) (*secondfactor.StepUp, error) {
	var s secondfactor.StepUp
	err := r.pool.QueryRow(ctx, `
		SELECT user_id, session_id, method, verified_at, expires_at
		  FROM step_ups WHERE user_id = $1 AND session_id = $2`, userID, sessionID).
		Scan(&s.UserID, &s.SessionID, &s.Method, &s.VerifiedAt, &s.ExpiresAt)
	if NoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Translate(err, "step-up")
	}
	return &s, nil
}

// DeleteStepUpsOf drops every stepped-up session of the person. It is what a
// factor revocation calls: whoever removes a factor is saying "the device I had
// is no longer mine", and leaving the sessions open would keep the door the
// removal was meant to close.
func (r *SecondFactorRepo) DeleteStepUpsOf(ctx context.Context, userID string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM step_ups WHERE user_id = $1`, userID)
	return Translate(err, "step-up")
}

// ── policy ──────────────────────────────────────────────────────────────────

func (r *SecondFactorRepo) PolicyOf(ctx context.Context, accountID string) (secondfactor.Policy, error) {
	var p secondfactor.Policy
	var allowed []string
	err := r.pool.QueryRow(ctx, `
		SELECT require_second_factor, allowed_second_factors FROM accounts WHERE id = $1`, accountID).
		Scan(&p.Required, &allowed)
	if NoRows(err) {
		// An account that does not exist has no policy: the caller's account was
		// already validated upstream, and inventing "required" here would lock
		// out whoever hit a stale id.
		return secondfactor.Policy{}, nil
	}
	if err != nil {
		return secondfactor.Policy{}, Translate(err, "account")
	}
	for _, a := range allowed {
		p.Allowed = append(p.Allowed, secondfactor.Kind(a))
	}
	return p, nil
}

// Compile-time proof that the adapter satisfies the port. Without it, a
// signature change in the domain only breaks at the composition root — far from
// where the fix is.
var _ secondfactor.Repository = (*SecondFactorRepo)(nil)
