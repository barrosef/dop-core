//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/clock"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
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

// The two collisions the guard in EnsureUser cannot see.
//
// `users_email_uniq` is `UNIQUE (lower(email)) WHERE email IS NOT NULL` — it has
// NO email_verified predicate. The guard, on the other hand, requires the
// incoming principal to have proved the address and looks the other row up with
// `AND email_verified`, and both halves of that are deliberate: an unverified
// claim must not be enough to decide two subjects are the same person.
//
// So the guard is narrower than the index it protects, and these two cases get
// past it and reach the constraint. Without the adapter naming that constraint,
// the person reads a generic "user already exists" and nobody can act on it —
// which is exactly the opaque failure US-7 exists to replace. A domain double
// cannot reproduce this: the fact being tested IS the unique index.
func TestACollisionTheGuardCannotSeeStillExplainsItself(t *testing.T) {
	cases := map[string]struct {
		existingVerified  bool
		incomingVerified  bool
		incomingProviders []string
	}{
		// The lookup asks for a VERIFIED row, the row on file is not one, so it
		// finds nothing and the insert walks straight into the index.
		"the_row_on_file_is_unverified": {
			existingVerified: false, incomingVerified: true,
			incomingProviders: []string{"email", "password"},
		},
		// The guard is skipped altogether — it only runs for a principal that
		// proved the address. This is the GitHub case D-5 deliberately admits.
		"the_arriving_principal_is_unverified": {
			existingVerified: true, incomingVerified: false,
			incomingProviders: []string{"github.com"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pool := openPool(t)
			ctx := ctxutil.Into(context.Background(), ctxutil.System("test-"+name))
			svc := identity.NewService(postgres.NewIdentityRepo(pool), clock.NewSystem())

			email := uniqueEmail(t)
			if _, _, err := svc.EnsureUser(ctx, ports.Principal{
				Subject: uniqueSubject(t), Email: email, EmailVerified: tc.existingVerified,
				Providers: []string{"google.com"},
			}); err != nil {
				t.Fatalf("the first user could not be created: %v", err)
			}

			_, _, err := svc.EnsureUser(ctx, ports.Principal{
				Subject: uniqueSubject(t), Email: email, EmailVerified: tc.incomingVerified,
				Providers: tc.incomingProviders,
			})
			if errs.KindOf(err) != errs.KindConflict {
				t.Fatalf("two subjects on one address must be a named conflict, got %v (%v)",
					errs.KindOf(err), err)
			}
			if !strings.Contains(err.Error(), identity.MsgEmailBelongsToAnotherUser) {
				t.Fatalf("the collision must say WHAT to change — the provider's account "+
					"linking — instead of 'something already exists': %v", err)
			}
		})
	}
}
