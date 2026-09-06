//go:build integration

// The reaction rules against a real Postgres (P-29).
//
// What is proven here is only provable against the database: the ORDER the
// chain comes back in. `Decide` makes `disables` directional by trusting that
// the slice runs from the most generic level to the most specific, so the
// ordering is a correctness contract of the query — not a nicety — and an
// in-memory fake would never notice the planner returning another order.
package integration

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/domain/reaction"
)

func TestRulesForReturnsTheChainFromGenericToSpecific(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewReaction(pool)
	account, workspace, project := seedChain(t, pool)
	event := uniqueEventType()

	platform := mustCreateRule(t, repo, reaction.Rule{
		OwnerScope: "platform", Trigger: reaction.TriggerEvent,
		EventType: event,
		Actions:   []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled:   true, Why: "the invitee has no cockpit yet",
	})
	projectRule := mustCreateRule(t, repo, reaction.Rule{
		AccountID: account, OwnerScope: "project", OwnerID: project,
		Trigger: reaction.TriggerEvent, EventType: event,
		Disables: []string{platform},
		Enabled:  true, Why: "this project onboards by hand",
	})

	got, err := repo.RulesFor(ctx, account, event, []reaction.ScopeRef{
		{Scope: "platform"}, {Scope: "account", ID: account},
		{Scope: "workspace", ID: workspace}, {Scope: "project", ID: project},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("both rules of the chain, got %d", len(got))
	}
	// The order is the contract: `disables` is directional, and a project rule
	// arriving BEFORE the platform's would silently fail to switch it off.
	if got[0].ID != platform || got[1].ID != projectRule {
		t.Fatalf("wrong order: %s then %s", got[0].OwnerScope, got[1].OwnerScope)
	}
	// The platform rule round-trips its NULLs as empty strings, and an action
	// with no parameters comes back with no parameters — not with a nil map the
	// caller has to guard.
	if got[0].AccountID != "" || got[0].OwnerID != "" {
		t.Errorf("the platform rule carries an owner: account=%q owner=%q", got[0].AccountID, got[0].OwnerID)
	}
	if len(got[0].Disables) != 0 {
		t.Errorf("a rule that disables nothing came back with %v", got[0].Disables)
	}
	if len(got[0].Actions) != 1 || got[0].Actions[0].Name != reaction.ActionSendEmail {
		t.Errorf("the platform rule's actions did not survive: %+v", got[0].Actions)
	}
	if len(got[1].Disables) != 1 || got[1].Disables[0] != platform {
		t.Errorf("the project rule's disables did not survive: %v", got[1].Disables)
	}
}

func TestRulesForDoesNotLeakAnotherAccountsRules(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewReaction(pool)
	mine, _, _ := seedChain(t, pool)
	theirs, _, theirProject := seedChain(t, pool)
	event := uniqueEventType()

	mustCreateRule(t, repo, reaction.Rule{
		AccountID: theirs, OwnerScope: "project", OwnerID: theirProject,
		Trigger: reaction.TriggerEvent, EventType: event,
		Actions: []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled: true, Why: "theirs",
	})
	got, err := repo.RulesFor(ctx, mine, event, []reaction.ScopeRef{
		{Scope: "platform"}, {Scope: "account", ID: mine},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("another account's rule reached this chain: %+v", got)
	}
}

// Two rules at the SAME level share a position in the chain, so ordering by the
// level alone leaves their relative order to the planner. That is not cosmetic:
// `Decide` only lets a rule disable what came BEFORE it, so a plan that swapped
// these two would flip which one switches the other off — the same input
// deciding differently between two runs.
func TestTwoRulesAtTheSameLevelKeepAStableOrder(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewReaction(pool)
	account, workspace, project := seedChain(t, pool)
	event := uniqueEventType()

	first := mustCreateRule(t, repo, reaction.Rule{
		AccountID: account, OwnerScope: "project", OwnerID: project,
		Trigger: reaction.TriggerEvent, EventType: event,
		Actions: []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled: true, Why: "the first one written at this level",
	})
	second := mustCreateRule(t, repo, reaction.Rule{
		AccountID: account, OwnerScope: "project", OwnerID: project,
		Trigger: reaction.TriggerEvent, EventType: event,
		Disables: []string{first},
		Enabled:  true, Why: "and the one that later refused it",
	})

	chain := []reaction.ScopeRef{
		{Scope: "platform"}, {Scope: "account", ID: account},
		{Scope: "workspace", ID: workspace}, {Scope: "project", ID: project},
	}
	for i := 0; i < 5; i++ {
		got, err := repo.RulesFor(ctx, account, event, chain)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("run %d: two rules at one level, got %d", i, len(got))
		}
		// Written first, so returned first: the tiebreaker is created_at then
		// id, which is a total order over the rows rather than the planner's
		// mood.
		if got[0].ID != first || got[1].ID != second {
			t.Fatalf("run %d: unstable order within a level: %s then %s", i, got[0].ID, got[1].ID)
		}
	}
}

// A platform rule is the case an in-memory fake never catches: AccountID and
// OwnerID are the empty string, and an empty string is not a uuid. This
// repository already shipped that bug once on the flows path.
func TestCreatingAPlatformRuleWritesNullsAndNotEmptyStrings(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewReaction(pool)
	account, _, _ := seedChain(t, pool)
	event := uniqueEventType()

	id := mustCreateRule(t, repo, reaction.Rule{
		OwnerScope: "platform", Trigger: reaction.TriggerEvent, EventType: event,
		Actions: []reaction.Action{{Name: reaction.ActionOpenAttention, Params: map[string]string{"title": "hello"}}},
		Enabled: true, Why: "every account starts from the platform's policy",
	})

	var accountNull, ownerNull bool
	if err := pool.QueryRow(ctx,
		`SELECT account_id IS NULL, owner_id IS NULL FROM reaction_rules WHERE id = $1`, id).
		Scan(&accountNull, &ownerNull); err != nil {
		t.Fatal(err)
	}
	if !accountNull || !ownerNull {
		t.Errorf("the platform rule stored an owner: account NULL=%v owner NULL=%v", accountNull, ownerNull)
	}

	// It reaches a chain that asks for the platform level...
	got, err := repo.RulesFor(ctx, account, event, []reaction.ScopeRef{
		{Scope: "platform"}, {Scope: "account", ID: account},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("the platform rule did not reach the chain: %+v", got)
	}
	if got[0].Actions[0].Params["title"] != "hello" {
		t.Errorf("the action's parameters did not survive: %+v", got[0].Actions[0])
	}

	// ...and NOT one that does not. The chain is the caller's, and a caller
	// resolving an account that opted out of the platform level must not be
	// handed it anyway.
	got, err = repo.RulesFor(ctx, account, event, []reaction.ScopeRef{{Scope: "account", ID: account}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the platform rule arrived without being asked for: %+v", got)
	}
}

// The write path's tenancy guard. idempotency_key is globally unique, so the
// conflict lookup that resolves a repeat has to filter by account — without it,
// the second account gets the FIRST account's rule back and believes it created
// it. That leak already shipped once through flowByKey.
func TestTheSameIdempotencyKeyInTwoAccountsMakesTwoRules(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewReaction(pool)
	mine, _, myProject := seedChain(t, pool)
	theirs, _, theirProject := seedChain(t, pool)
	event := uniqueEventType()
	key := fmt.Sprintf("shared-key-%d", time.Now().UnixNano())

	first, err := repo.Create(ctx, &reaction.Rule{
		AccountID: mine, OwnerScope: "project", OwnerID: myProject,
		Trigger: reaction.TriggerEvent, EventType: event,
		Actions: []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled: true, Why: "mine",
	}, key)
	if err != nil {
		t.Fatal(err)
	}

	second, err := repo.Create(ctx, &reaction.Rule{
		AccountID: theirs, OwnerScope: "project", OwnerID: theirProject,
		Trigger: reaction.TriggerEvent, EventType: event,
		Actions: []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled: true, Why: "theirs",
	}, key)
	if err != nil {
		t.Fatalf("the second account could not use a key the first had used: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("account %s received account %s's rule %s", theirs, mine, first.ID)
	}
	if second.AccountID != theirs {
		t.Fatalf("the rule came back owned by %s, not %s", second.AccountID, theirs)
	}

	// And the repeat is still a repeat WITHIN one account: the same key twice
	// returns the same rule instead of a twin (ADR-0017).
	again, err := repo.Create(ctx, &reaction.Rule{
		AccountID: mine, OwnerScope: "project", OwnerID: myProject,
		Trigger: reaction.TriggerEvent, EventType: event,
		Actions: []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled: true, Why: "mine, resent",
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("the resend created a twin: %s and %s", first.ID, again.ID)
	}
}

// SetEnabled is an account's switch over its OWN rules. The platform's are not
// an account's to switch off — that is what `disables` is for, and it is an act
// recorded in the account's own row.
func TestSetEnabledIsScopedByAccount(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewReaction(pool)
	mine, _, myProject := seedChain(t, pool)
	theirs, _, _ := seedChain(t, pool)
	event := uniqueEventType()

	id := mustCreateRule(t, repo, reaction.Rule{
		AccountID: mine, OwnerScope: "project", OwnerID: myProject,
		Trigger: reaction.TriggerEvent, EventType: event,
		Actions: []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled: true, Why: "mine",
	})
	platform := mustCreateRule(t, repo, reaction.Rule{
		OwnerScope: "platform", Trigger: reaction.TriggerEvent, EventType: event,
		Actions: []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled: true, Why: "the platform's",
	})

	if err := repo.SetEnabled(ctx, theirs, id, false); err == nil {
		t.Fatal("another account switched off a rule that is not theirs")
	}
	if err := repo.SetEnabled(ctx, mine, platform, false); err == nil {
		t.Fatal("an account switched off the platform's rule")
	}
	if err := repo.SetEnabled(ctx, mine, id, false); err != nil {
		t.Fatal(err)
	}

	got, err := repo.RulesFor(ctx, mine, event, []reaction.ScopeRef{
		{Scope: "platform"}, {Scope: "project", ID: myProject},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != platform {
		t.Fatalf("the disabled rule is still in the chain: %+v", got)
	}
}

// `when_match` is jsonb and `Rule.When` is map[string]string. Nothing in the
// schema stops a number going in, and a reader that quietly dropped the entry
// would make the rule match events it must not. It has to fail loudly.
func TestAWhenThatIsNotStringsFailsLoudlyInsteadOfBeingDropped(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewReaction(pool)
	account, _, project := seedChain(t, pool)
	event := uniqueEventType()

	id := mustCreateRule(t, repo, reaction.Rule{
		AccountID: account, OwnerScope: "project", OwnerID: project,
		Trigger: reaction.TriggerEvent, EventType: event,
		When:    map[string]string{"count": "5"},
		Actions: []reaction.Action{{Name: reaction.ActionSendEmail}},
		Enabled: true, Why: "only when the count is five",
	})
	// Written behind the adapter's back, which is exactly how it would arrive:
	// a hand-written policy row, or a migration.
	if _, err := pool.Exec(ctx,
		`UPDATE reaction_rules SET when_match = '{"count": 5}'::jsonb WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	_, err := repo.RulesFor(ctx, account, event, []reaction.ScopeRef{{Scope: "project", ID: project}})
	if err == nil {
		t.Fatal("a `when` holding a number was accepted silently")
	}
	if !strings.Contains(err.Error(), "when") {
		t.Errorf("the failure does not say what is wrong: %v", err)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// seedChain creates a disposable account with a workspace and a project — one
// whole chain. Disposable because a test that reused another's rows would pass
// for the wrong reason.
func seedChain(t *testing.T, pool *pgxpool.Pool) (account, workspace, project string) {
	t.Helper()
	ctx := context.Background()
	handle := fmt.Sprintf("reaction-%d-%d", time.Now().UnixNano(), nextSeq())
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id::text`,
		handle).Scan(&account); err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (account_id, name) VALUES ($1,'reaction') RETURNING id::text`,
		account).Scan(&workspace); err != nil {
		t.Fatalf("creating the workspace: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (account_id, workspace_id, name) VALUES ($1,$2,'reaction') RETURNING id::text`,
		account, workspace).Scan(&project); err != nil {
		t.Fatalf("creating the project: %v", err)
	}
	return account, workspace, project
}

func mustCreateRule(t *testing.T, repo reaction.Repository, r reaction.Rule) string {
	t.Helper()
	key := fmt.Sprintf("reaction-rule-%d-%d", time.Now().UnixNano(), nextSeq())
	saved, err := repo.Create(context.Background(), &r, key)
	if err != nil {
		t.Fatalf("creating the %s rule: %v", r.OwnerScope, err)
	}
	return saved.ID
}

var seq atomic.Int64

func nextSeq() int64 { return seq.Add(1) }

// uniqueEventType keeps the tests independent of each other. A platform rule
// has no account to isolate it, so two tests sharing an event type would see
// each other's rules — and this database is reused across runs.
func uniqueEventType() string {
	return fmt.Sprintf("dop.identity.invite.created.%d.%d", time.Now().UnixNano(), nextSeq())
}
