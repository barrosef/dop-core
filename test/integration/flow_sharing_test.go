//go:build integration

// Integration tests of the FLOW-SHARING adapter against a real Postgres
// (flow-sharing plan, Task 9).
//
//	go test ./test/integration/ -tags=integration -run TestResolvePublication -v
//	go test ./test/integration/ -tags=integration -run TestRecordDerivation -v
//	go test ./test/integration/ -tags=integration -run TestRevokeShare -v
//
// What is proven here and cannot be proven with the domain's fake: the SQL —
// ResolvePublication's join is the whole authorisation (R2's ONE deliberate
// crossing), RecordDerivation writes the copy and the adoption record in ONE
// transaction (R17), and RevokeShare writes the share, every reached copy and
// adoption, and both events, in another (R1). postgres.InTx and postgres.Emit
// have no other caller in the tree yet — this is the first.
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ── seed helpers ─────────────────────────────────────────────────────────────

// seedThreeAccounts creates the cast every sharing test plays against: a
// publisher, an account it grants to, and a third account that holds no grant
// at all — the one that proves ResolvePublication answers NotFound and not
// Permission.
func seedThreeAccounts(t *testing.T, pool *pgxpool.Pool) (pubAccount, otherAccount, thirdAccount string) {
	t.Helper()
	mk := func(label string) string {
		handle := fmt.Sprintf("shr-%s-%d", label, time.Now().UnixNano())
		var id string
		if err := pool.QueryRow(context.Background(),
			`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
			handle).Scan(&id); err != nil {
			t.Fatalf("seeding account %s: %v", label, err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, id) })
		return id
	}
	return mk("pub"), mk("other"), mk("third")
}

// seedFlow writes a flow and its first version DIRECTLY, bypassing
// WorkflowRepo the same way agentmetrics_test.go and secondfactor_test.go seed
// their own fixtures — what this file exercises is the SHARING adapter, not
// flow creation. Both rows go in one statement (a CTE) because
// flows_versao_corrente_existe is a DEFERRED FK: writing them as two
// statements would let the first commit before the version it points at
// exists, and the deferred check would fail at THAT commit.
func seedFlow(t *testing.T, pool *pgxpool.Pool, accountID string) (flowID string, version int32) {
	t.Helper()
	key := fmt.Sprintf("shr-flow-%d", time.Now().UnixNano())
	const stages = `[{"key":"context","name":"Context","type":"context","artifacts":[],"gate":"none","subtypes":[]}]`
	err := pool.QueryRow(context.Background(), `
		WITH new_flow AS (
		  INSERT INTO flows (account_id, owner_scope, owner_id, current_version,
		                     idempotency_key, created_at, updated_at)
		  VALUES ($1, 'account', $1, 1, $2, now(), now())
		  RETURNING id
		)
		INSERT INTO flow_versions (flow_id, version, name, stages, idempotency_key, created_at)
		SELECT new_flow.id, 1, 'seed flow', $3::jsonb, $2 || ':v1', now() FROM new_flow
		RETURNING flow_id`,
		accountID, key, stages).Scan(&flowID)
	if err != nil {
		t.Fatalf("seeding the flow: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM flows WHERE id = $1`, flowID) })
	return flowID, 1
}

func handleOf(t *testing.T, pool *pgxpool.Pool, accountID string) string {
	t.Helper()
	var handle string
	if err := pool.QueryRow(context.Background(),
		`SELECT handle FROM accounts WHERE id = $1`, accountID).Scan(&handle); err != nil {
		t.Fatalf("looking up the account's handle: %v", err)
	}
	return handle
}

func stageSpec(key string) workflow.StageSpec {
	return workflow.StageSpec{Key: key, Name: key, Type: workflow.TypeContext, Gate: workflow.GateNone}
}

// ── ResolvePublication: the one deliberate crossing ─────────────────────────

func TestResolvePublicationOnlyAnswersToWhoWasGranted(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	pubAccount, otherAccount, third := seedThreeAccounts(t, pool)
	flowID, version := seedFlow(t, pool, pubAccount)

	pub, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version,
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateShare(ctx, &workflow.Share{
		PublicationID: pub.ID, ToAccountID: otherAccount,
		RevocationPolicy: workflow.PolicyProspective, GrantedAt: time.Now(),
	}, "g1"); err != nil {
		t.Fatal(err)
	}

	ref := workflow.PublicationRef{Handle: handleOf(t, pool, pubAccount), Slug: "backend-go"}
	if _, err := repo.ResolvePublication(ctx, otherAccount, ref); err != nil {
		t.Fatalf("the granted account had to resolve it: %v", err)
	}
	// No grant, no existence. NotFound and never Permission: whether a flow
	// exists in another account is not an outsider's to learn.
	if _, err := repo.ResolvePublication(ctx, third, ref); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("an account with no grant had to get not-found: %v", err)
	}
}

// A withdrawn publication resolves to nothing even for an account that still
// holds a live grant: withdrawal and revocation are different refusals, and
// this is what keeps them from being conflated in the query.
func TestResolvePublicationRefusesAWithdrawnPublicationEvenWithAGrant(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	pubAccount, otherAccount, _ := seedThreeAccounts(t, pool)
	flowID, version := seedFlow(t, pool, pubAccount)

	pub, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version,
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateShare(ctx, &workflow.Share{
		PublicationID: pub.ID, ToAccountID: otherAccount,
		RevocationPolicy: workflow.PolicyProspective, GrantedAt: time.Now(),
	}, "g1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Withdraw(ctx, pubAccount, pub.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	ref := workflow.PublicationRef{Handle: handleOf(t, pool, pubAccount), Slug: "backend-go"}
	if _, err := repo.ResolvePublication(ctx, otherAccount, ref); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("a withdrawn publication has to be not-found even for a granted account: %v", err)
	}
}

// ── idempotency: the natural key absorbs the retry ──────────────────────────

func TestCreatePublicationRetryReturnsTheSameRowNotASecondOne(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	pubAccount, _, _ := seedThreeAccounts(t, pool)
	flowID, version := seedFlow(t, pool, pubAccount)

	first, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version, Notes: "first cut",
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version, Notes: "first cut",
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("a repeated publish came back as a DIFFERENT row: %s vs %s", again.ID, first.ID)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM flow_publications WHERE account_id = $1 AND slug = $2 AND version = $3`,
		pubAccount, "backend-go", version).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows for one (account, slug, version) — the natural key did not absorb the retry", n)
	}
}

func TestWithdrawIsIdempotentAndDoesNotMoveTheTimestamp(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	pubAccount, _, _ := seedThreeAccounts(t, pool)
	flowID, version := seedFlow(t, pool, pubAccount)

	pub, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version,
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if err := repo.Withdraw(ctx, pubAccount, pub.ID, first); err != nil {
		t.Fatal(err)
	}
	// Withdrawing again with a LATER time must not move the timestamp: the
	// first call is the one that matters.
	if err := repo.Withdraw(ctx, pubAccount, pub.ID, time.Now()); err != nil {
		t.Fatalf("withdrawing what is already withdrawn has to be success: %v", err)
	}
	got, err := repo.PublicationByID(ctx, pubAccount, pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WithdrawnAt.Equal(first) {
		t.Fatalf("a second withdrawal moved the timestamp: got %v, want %v", got.WithdrawnAt, first)
	}
	// An id under the wrong account is not found, never silently withdrawn.
	if err := repo.Withdraw(ctx, "00000000-0000-0000-0000-000000000000", pub.ID, time.Now()); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("withdrawing under the wrong account has to be not-found: %v", err)
	}
}

// ── RecordDerivation: one transaction, or neither write (R17) ──────────────

// TestRecordDerivationWritesTheCopyAndTheAdoptionTogether is the positive
// control: the copy exists, carries its provenance, and the publisher's
// adoption record points back at it.
func TestRecordDerivationWritesTheCopyAndTheAdoptionTogether(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	pubAccount, otherAccount, _ := seedThreeAccounts(t, pool)
	flowID, version := seedFlow(t, pool, pubAccount)

	pub, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version,
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateShare(ctx, &workflow.Share{
		PublicationID: pub.ID, ToAccountID: otherAccount,
		RevocationPolicy: workflow.PolicyProspective, GrantedAt: time.Now(),
	}, "g1"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	ref := "@" + handleOf(t, pool, pubAccount) + "/backend-go"
	copied := &workflow.Flow{
		AccountID: otherAccount, OwnerScope: workflow.ScopeAccount, OwnerID: otherAccount,
		Name: "copied flow", Version: 1, Stages: []workflow.StageSpec{stageSpec("context")},
		Origin:    &workflow.Origin{Ref: ref, Version: pub.Version, AdoptedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	adoption := &workflow.Adoption{
		PublicationID: pub.ID, Version: pub.Version, ByAccountID: otherAccount, DerivedAt: now,
	}
	key := fmt.Sprintf("derive-%d", time.Now().UnixNano())
	saved, err := repo.RecordDerivation(ctx, otherAccount, copied, adoption, key)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccountID != otherAccount {
		t.Fatalf("the copy belongs to %q, want %q", saved.AccountID, otherAccount)
	}
	if saved.Origin == nil || saved.Origin.Ref != ref || saved.Origin.Version != pub.Version {
		t.Fatalf("the copy does not carry where it came from: %+v", saved.Origin)
	}
	if adoption.ID == "" || adoption.FlowID != saved.ID {
		t.Fatalf("the adoption record was not filled in: %+v", adoption)
	}

	ads, err := repo.AdoptionsOfPublication(ctx, pubAccount, pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ads) != 1 || ads[0].FlowID != saved.ID || ads[0].ByAccountID != otherAccount {
		t.Fatalf("the derivation was not recorded on the publisher's side: %+v", ads)
	}

	// A retry with the SAME key must not create a second flow or a second
	// adoption record.
	again, err := repo.RecordDerivation(ctx, otherAccount, copied, &workflow.Adoption{
		PublicationID: pub.ID, Version: pub.Version, ByAccountID: otherAccount, DerivedAt: now,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != saved.ID {
		t.Fatalf("a repeated derivation came back as a DIFFERENT flow: %s vs %s", again.ID, saved.ID)
	}
	ads, err = repo.AdoptionsOfPublication(ctx, pubAccount, pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ads) != 1 {
		t.Fatalf("a repeated derivation created a second adoption record: %+v", ads)
	}
}

// TestRecordDerivationLeavesNoOrphanedCopyOnFailure is proof #1 asked for by
// the task: a failure INSIDE RecordDerivation — here, the adoption's FK on
// by_account_id, which fires only AFTER the flow and its version are already
// staged in the same transaction — must leave NO copy behind. A copy that
// survived with nobody having recorded the adoption is one nobody could ever
// revoke, which is the entire reason this method exists as one call and not
// two.
func TestRecordDerivationLeavesNoOrphanedCopyOnFailure(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	pubAccount, otherAccount, _ := seedThreeAccounts(t, pool)
	flowID, version := seedFlow(t, pool, pubAccount)

	pub, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version,
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	ref := "@" + handleOf(t, pool, pubAccount) + "/backend-go"
	copied := &workflow.Flow{
		AccountID: otherAccount, OwnerScope: workflow.ScopeAccount, OwnerID: otherAccount,
		Name: "copied flow", Version: 1, Stages: []workflow.StageSpec{stageSpec("context")},
		Origin:    &workflow.Origin{Ref: ref, Version: pub.Version, AdoptedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	// An account id that does not exist: flow_adoptions.by_account_id's FK
	// fails, but only once the INSERT actually runs — well after the flow and
	// its version were written in the SAME transaction.
	adoption := &workflow.Adoption{
		PublicationID: pub.ID, Version: pub.Version,
		ByAccountID: "00000000-0000-0000-0000-000000000000", DerivedAt: now,
	}
	key := fmt.Sprintf("derive-fail-%d", time.Now().UnixNano())
	if _, err := repo.RecordDerivation(ctx, otherAccount, copied, adoption, key); err == nil {
		t.Fatal("expected the adoption's foreign-key violation to surface")
	}

	var flows, versions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM flows WHERE idempotency_key = $1`, key).Scan(&flows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM flow_versions WHERE idempotency_key = $1`, key+":1").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if flows != 0 {
		t.Errorf("the copy survived a failed derivation: %d flows row(s) — an orphaned copy nobody could ever revoke", flows)
	}
	if versions != 0 {
		t.Errorf("the copy's version survived a failed derivation: %d row(s)", versions)
	}
}

// ── RevokeShare: one transaction, share + copies + both events (R1) ────────

// TestRevokeShareUnderProspectiveEmitsBothEventsAndTouchesNoCopy is proof #2:
// under `prospective`, RevokeShare must still write BOTH events even though
// rev.Adoptions is empty and nothing about the derived copy or its adoption
// record may change.
func TestRevokeShareUnderProspectiveEmitsBothEventsAndTouchesNoCopy(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	pubAccount, otherAccount, _ := seedThreeAccounts(t, pool)
	flowID, version := seedFlow(t, pool, pubAccount)

	pub, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version,
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	share, err := repo.CreateShare(ctx, &workflow.Share{
		PublicationID: pub.ID, ToAccountID: otherAccount,
		RevocationPolicy: workflow.PolicyProspective, GrantedAt: time.Now(),
	}, "g1")
	if err != nil {
		t.Fatal(err)
	}

	// A real copy has to exist so the assertion "prospective touches no copy"
	// is checking something, not the absence of anything to touch.
	now := time.Now().UTC()
	ref := "@" + handleOf(t, pool, pubAccount) + "/backend-go"
	copied := &workflow.Flow{
		AccountID: otherAccount, OwnerScope: workflow.ScopeAccount, OwnerID: otherAccount,
		Name: "copied flow", Version: 1, Stages: []workflow.StageSpec{stageSpec("context")},
		Origin:    &workflow.Origin{Ref: ref, Version: pub.Version, AdoptedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	adoption := &workflow.Adoption{
		PublicationID: pub.ID, Version: pub.Version, ByAccountID: otherAccount, DerivedAt: now,
	}
	saved, err := repo.RecordDerivation(ctx, otherAccount, copied,
		adoption, fmt.Sprintf("derive-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.RevokeShare(ctx, pubAccount, workflow.Revocation{
		ShareID: share.ID, PublicationID: pub.ID, ToAccountID: otherAccount,
		Policy: workflow.PolicyProspective, At: time.Now(),
		// Adoptions is EMPTY: prospective reaches the grant and nothing else.
	}); err != nil {
		t.Fatal(err)
	}

	var shareRevoked *time.Time
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM flow_shares WHERE id = $1`, share.ID).Scan(&shareRevoked); err != nil {
		t.Fatal(err)
	}
	if shareRevoked == nil {
		t.Fatal("the share was not marked revoked")
	}

	var copyRevoked *time.Time
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM flows WHERE id = $1`, saved.ID).Scan(&copyRevoked); err != nil {
		t.Fatal(err)
	}
	if copyRevoked != nil {
		t.Fatal("a prospective revocation touched a copy it must not reach")
	}
	var adoptionRevoked *time.Time
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM flow_adoptions WHERE id = $1`, adoption.ID).Scan(&adoptionRevoked); err != nil {
		t.Fatal(err)
	}
	if adoptionRevoked != nil {
		t.Fatal("a prospective revocation touched an adoption record it must not reach")
	}

	// Both events, still fired, one per side of the grant.
	rows, err := pool.Query(ctx,
		`SELECT type, account_id::text FROM events WHERE aggregate = 'flow' AND aggregate_id = $1 ORDER BY type`,
		share.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var typ, acct string
		if err := rows.Scan(&typ, &acct); err != nil {
			t.Fatal(err)
		}
		got = append(got, typ+":"+acct)
	}
	want := []string{"flow.grant.revoked:" + otherAccount, "flow.share.revoked:" + pubAccount}
	if len(got) != len(want) {
		t.Fatalf("got %v events, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v events, want %v", got, want)
		}
	}

	// The events reached the outbox in the SAME transaction — the whole point
	// of Emit (ADR-0019).
	var outboxed int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox o JOIN events e ON e.id = o.event_id
		 WHERE e.aggregate = 'flow' AND e.aggregate_id = $1`, share.ID).Scan(&outboxed); err != nil {
		t.Fatal(err)
	}
	if outboxed != 2 {
		t.Fatalf("expected 2 outbox rows for the 2 events, got %d", outboxed)
	}
}

// TestRevokeShareUnderDrainRevokesTheReachedCopyAndItsAdoption exercises the
// OTHER branch of RevokeShare: a non-prospective policy has to mark the
// adoption record AND the copy it points at, in the same transaction as the
// share and the events.
func TestRevokeShareUnderDrainRevokesTheReachedCopyAndItsAdoption(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	pubAccount, otherAccount, _ := seedThreeAccounts(t, pool)
	flowID, version := seedFlow(t, pool, pubAccount)

	pub, err := repo.CreatePublication(ctx, &workflow.Publication{
		FlowID: flowID, AccountID: pubAccount, Slug: "backend-go", Version: version,
	}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	share, err := repo.CreateShare(ctx, &workflow.Share{
		PublicationID: pub.ID, ToAccountID: otherAccount,
		RevocationPolicy: workflow.PolicyDrain, GrantedAt: time.Now(),
	}, "g1")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	ref := "@" + handleOf(t, pool, pubAccount) + "/backend-go"
	copied := &workflow.Flow{
		AccountID: otherAccount, OwnerScope: workflow.ScopeAccount, OwnerID: otherAccount,
		Name: "copied flow", Version: 1, Stages: []workflow.StageSpec{stageSpec("context")},
		Origin:    &workflow.Origin{Ref: ref, Version: pub.Version, AdoptedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	adoption := &workflow.Adoption{
		PublicationID: pub.ID, Version: pub.Version, ByAccountID: otherAccount, DerivedAt: now,
	}
	saved, err := repo.RecordDerivation(ctx, otherAccount, copied,
		adoption, fmt.Sprintf("derive-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.RevokeShare(ctx, pubAccount, workflow.Revocation{
		ShareID: share.ID, PublicationID: pub.ID, ToAccountID: otherAccount,
		Policy: workflow.PolicyDrain, At: time.Now(),
		Adoptions: []workflow.AdoptionRef{{ID: adoption.ID, FlowID: saved.ID}},
	}); err != nil {
		t.Fatal(err)
	}

	var copyRevoked, adoptionRevoked *time.Time
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM flows WHERE id = $1`, saved.ID).Scan(&copyRevoked); err != nil {
		t.Fatal(err)
	}
	if copyRevoked == nil {
		t.Fatal("drain has to revoke the copy it reaches")
	}
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM flow_adoptions WHERE id = $1`, adoption.ID).Scan(&adoptionRevoked); err != nil {
		t.Fatal(err)
	}
	if adoptionRevoked == nil {
		t.Fatal("drain has to revoke the adoption record it reaches")
	}

	// Still marked, never deleted: the copy stays retrievable with its
	// content whole.
	var stages []byte
	if err := pool.QueryRow(ctx, `SELECT stages FROM flow_versions WHERE flow_id = $1 AND version = 1`, saved.ID).Scan(&stages); err != nil {
		t.Fatal(err)
	}
	if len(stages) == 0 {
		t.Fatal("the copy's content did not survive the revocation")
	}
}

// ── the pin ──────────────────────────────────────────────────────────────────

func TestPinRoundTripsThroughPinOf(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowSharing(pool)
	_, otherAccount, _ := seedThreeAccounts(t, pool)
	flowID, _ := seedFlow(t, pool, otherAccount)

	if _, pinned, err := repo.PinOf(ctx, otherAccount, flowID); err != nil || pinned {
		t.Fatalf("an unpinned flow has to answer unpinned: pinned=%v err=%v", pinned, err)
	}
	if err := repo.Pin(ctx, otherAccount, flowID, 1, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	version, pinned, err := repo.PinOf(ctx, otherAccount, flowID)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned || version != 1 {
		t.Fatalf("pinned=%v version=%d, want pinned=true version=1", pinned, version)
	}
	// A second version has to actually EXIST before it can be pinned to — the
	// compound FK (flow_id, version) → flow_versions checks exactly that.
	if _, err := pool.Exec(ctx, `
		INSERT INTO flow_versions (flow_id, version, name, stages, idempotency_key, created_at)
		VALUES ($1, 2, 'seed flow v2', '[{"key":"context","name":"Context","type":"context","artifacts":[],"gate":"none","subtypes":[]}]'::jsonb, $2, now())`,
		flowID, fmt.Sprintf("shr-flow-v2-%d", time.Now().UnixNano())); err != nil {
		t.Fatal(err)
	}
	// Bumping the SAME pin moves it, it does not create a second row.
	if err := repo.Pin(ctx, otherAccount, flowID, 2, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	version, pinned, err = repo.PinOf(ctx, otherAccount, flowID)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned || version != 2 {
		t.Fatalf("after bumping: pinned=%v version=%d, want pinned=true version=2", pinned, version)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM account_flow_pins WHERE account_id = $1 AND flow_id = $2`,
		otherAccount, flowID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d pin rows for one (account, flow) — bumping created a second one", n)
	}
}

// ── AccountDefaults ──────────────────────────────────────────────────────────

func TestAccountDefaultsReadsTheColumn(t *testing.T) {
	pool := poolWithCleanup(t)
	ctx := context.Background()
	defaults := postgres.NewAccountDefaultsRepo(pool)

	handle := fmt.Sprintf("shr-defaults-%d", time.Now().UnixNano())
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (kind, handle, display_name, default_revocation_policy)
		VALUES ('personal', $1, $1, 'terminate') RETURNING id`, handle).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, id) })

	got, err := defaults.DefaultRevocationPolicy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != "terminate" {
		t.Fatalf("got %q, want %q", got, "terminate")
	}
}
