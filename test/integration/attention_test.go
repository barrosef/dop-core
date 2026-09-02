//go:build integration

// The test that was missing — and whose absence let a real bug through.
//
// The attention box read `status` on an event the demand domain emitted with
// `to`. It never matched: the box only knew how to CLOSE an item it never
// opened. The unit tests on both sides passed, because each used the shape it
// ASSUMED of the other — the box's test fabricated the event, and the demand's
// never looked at the box.
//
// No test that stays inside one domain catches this. Only one that crosses the
// event log for real, which is what this file does.
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres/projection"
	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

func TestAStageOnAHumanGateBECOMESAnAttentionItem(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, env("TEST_DATABASE_URL",
		"postgres://dop:dop-local-dev@localhost:5432/dop?sslmode=disable"))
	if err != nil || pool.Ping(ctx) != nil {
		t.Skipf("Postgres unavailable: %v", err)
	}
	defer pool.Close()

	var account string
	// A unique handle per run: `uuidFrom` is deterministic, and reusing it would
	// make the second run fail at creating the account instead of at what we
	// want to test — failing for the wrong reason hides the real failure.
	handle := fmt.Sprintf("attention-%d", time.Now().UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id`,
		handle).Scan(&account); err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	box := projection.NewAttention(pool)

	// The event with the EXACT payload `demand.Service.AdvanceStage` emits. If
	// that payload changes shape, this test breaks — which is the point.
	envelope := mustJSON(map[string]any{
		"id":           uuidFrom(fmt.Sprintf("%d", time.Now().UnixNano())),
		"account_id":   account,
		"aggregate":    "demand",
		"aggregate_id": uuidFrom("d1"),
		"type":         "dop.demand.stage.advanced",
		"occurred_at":  time.Now().UTC(),
		"payload": map[string]any{
			// The key comes from the workflow catalog seeded in
			// migrations/0005_workflow.sql — it is stored data, not a literal
			// of this test.
			"stage_key":  "validacao-humana",
			"stage_type": "human_validation",
			"from":       "running",
			"to":         "blocked",
			"gate":       "human",
			"dop_status": "doing",
		},
	})

	if err := box.Handle(ctx, ports.Event{Payload: envelope}); err != nil {
		t.Fatalf("projection: %v", err)
	}

	repo := postgres.NewAttentionRepo(pool)
	items, err := repo.List(ctx, account, "", false, 10)
	if err != nil {
		t.Fatalf("reading the box: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("a stage stopped on a human gate did NOT become an item — " +
			"the box is reading a field the event does not have")
	}
	if items[0].Kind != attention.KindGatePending {
		t.Errorf("item kind: %q, expected gate_pending", items[0].Kind)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM attention_items WHERE account_id = $1`, account)
	})
}
