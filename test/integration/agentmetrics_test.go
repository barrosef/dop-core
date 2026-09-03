//go:build integration

// The agent's metrics against a real Postgres (P-23 phase 1).
//
// What is proven here is only provable against the database: the collection
// happens MORE THAN ONCE over the same file — the agent is still working while
// we read — so re-reading must not duplicate, and the cursor must not move past
// what was written.
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/domain/agentmetrics"
)

func TestCollectingTheSameStretchTwiceDoesNotDuplicate(t *testing.T) {
	pool := openPool(t)
	repo := postgres.NewAgentMetricsRepo(pool)
	ctx := context.Background()
	account := metricsAccount(t, pool)

	s, err := repo.EnsureSession(ctx, agentmetrics.Session{
		AccountID: account, ExternalID: fmt.Sprintf("sess-%d", time.Now().UnixNano()),
		CWD: "/workspace", GitBranch: "main", ToolVersion: "2.1.0",
		StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.ByteOffset != 0 {
		t.Fatalf("a fresh session starts at %d", s.ByteOffset)
	}

	turns := []agentmetrics.Turn{{
		UUID: "t1", OccurredAt: time.Now(), Model: "claude-opus-5",
		InputTokens: 100, OutputTokens: 50, CacheReadTokens: 9000,
		ToolUses: 2, Tools: []string{"Bash", "Read"}, RawUsage: map[string]any{"speed": "fast"},
	}}
	if err := repo.RecordTurns(ctx, s.ID, turns, 4096); err != nil {
		t.Fatal(err)
	}
	// The same stretch again — which is what a second pass over a growing file
	// does every time.
	if err := repo.RecordTurns(ctx, s.ID, turns, 4096); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_turns WHERE session_id = $1`, s.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d rows for one turn read twice", n)
	}
}

func TestTheCursorSurvivesAndNeverGoesBackwards(t *testing.T) {
	// The cursor is what stops the collector from re-reading 40 MB on every
	// pass. Going backwards would re-read forever; the guard is in the UPDATE.
	pool := openPool(t)
	repo := postgres.NewAgentMetricsRepo(pool)
	ctx := context.Background()
	account := metricsAccount(t, pool)
	external := fmt.Sprintf("sess-%d", time.Now().UnixNano())

	s, err := repo.EnsureSession(ctx, agentmetrics.Session{
		AccountID: account, ExternalID: external, GitBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordTurns(ctx, s.ID, nil, 8192); err != nil {
		t.Fatal(err)
	}
	// A second pass, arriving with an OLDER offset — a retry, a race between two
	// collectors. It must not rewind.
	if err := repo.RecordTurns(ctx, s.ID, nil, 100); err != nil {
		t.Fatal(err)
	}

	again, err := repo.EnsureSession(ctx, agentmetrics.Session{
		AccountID: account, ExternalID: external, GitBranch: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != s.ID {
		t.Fatal("the same session came back with another id")
	}
	if again.ByteOffset != 8192 {
		t.Errorf("the cursor is at %d, want 8192", again.ByteOffset)
	}
	// And the metadata IS refreshed: a running session knows more about itself
	// now than it did on its first line.
	var branch string
	if err := pool.QueryRow(ctx,
		`SELECT git_branch FROM agent_sessions WHERE id = $1`, s.ID).Scan(&branch); err != nil {
		t.Fatal(err)
	}
	if branch != "other" {
		t.Errorf("the branch was not refreshed: %q", branch)
	}
}

// metricsAccount creates a disposable account — the metrics hang off one, and a
// test that reused another's rows would pass for the wrong reason.
func metricsAccount(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	handle := fmt.Sprintf("metrics-%d", time.Now().UnixNano())
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id::text`,
		handle).Scan(&id); err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, id)
	})
	return id
}
