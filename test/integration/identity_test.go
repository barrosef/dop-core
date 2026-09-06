//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

func TestUserByVerifiedEmailIgnoresAnUnverifiedOne(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(pool)

	unverified, err := repo.UpsertUser(ctx, &identity.User{
		Subject: uniqueSubject(t), Email: uniqueEmail(t), EmailVerified: false,
		// A nil slice binds as SQL NULL, not the column's DEFAULT '{}' — the
		// insert only ever runs through here in production once a provider
		// login has already appended itself.
		Providers: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UserByVerifiedEmail(ctx, unverified.Email); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("an unverified e-mail is an unproven claim and must not match: %v", err)
	}
}

func TestUserByVerifiedEmailMatchesRegardlessOfCase(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(pool)

	email := uniqueEmail(t)
	saved, err := repo.UpsertUser(ctx, &identity.User{
		Subject: uniqueSubject(t), Email: email, EmailVerified: true, Providers: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.UserByVerifiedEmail(ctx, strings.ToUpper(email))
	if err != nil {
		t.Fatalf("the unique index is on lower(email); the lookup must agree: %v", err)
	}
	if got.ID != saved.ID {
		t.Fatalf("found the wrong user: %s", got.ID)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

var identitySeq atomic.Int64

// uniqueSubject and uniqueEmail keep repeated runs against the same database
// from colliding with users_email_uniq and the subject unique constraint —
// same shape as reaction_test.go's uniqueEventType. The counter alone only
// tells two calls in the SAME process apart; UnixNano is what tells two
// separate `go test` invocations apart, exactly as in uniqueEventType.
func uniqueSubject(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("subject-%s-%d-%d", t.Name(), time.Now().UnixNano(), identitySeq.Add(1))
}

func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d-%d@example.test", t.Name(), time.Now().UnixNano(), identitySeq.Add(1))
}
