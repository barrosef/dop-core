//go:build integration

// The flow's stages against a real Postgres.
//
// What is proven here and cannot be proven by a unit test on the mappers: the
// JSONB half. A stage's `Actions` travel through flow_versions.stages as a
// document, and a document that silently drops a field looks exactly like a
// flow that never declared one — which is the failure this file exists to
// catch. reaction.DecideStage reads a FROZEN version (ADR-0014 §4), so
// VersionOf is the read that actually matters, not only ByID.
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Digital-Business-One/dop-core/internal/adapter/postgres"
	"github.com/Digital-Business-One/dop-core/internal/domain/reaction"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
)

// seedFlowAccount creates the one account a flow needs an owner for. It does
// not reuse flow_sharing_test.go's seedThreeAccounts: that helper's cast is
// three accounts and a grant between them, and this file needs one owner.
func seedFlowAccount(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	handle := fmt.Sprintf("wf-%d-%d", time.Now().UnixNano(), nextSeq())
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO accounts (kind, handle, display_name) VALUES ('personal',$1,$1) RETURNING id::text`,
		handle).Scan(&id); err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id::text = $1`, id) })
	return id
}

func TestAStageActionSurvivesTheRoundTripThroughPostgres(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	repo := postgres.NewWorkflowRepo(pool)
	account := seedFlowAccount(t, pool)

	f := workflow.Flow{
		AccountID: account, OwnerScope: workflow.ScopeAccount, OwnerID: account,
		Name: "flow with actions",
		Stages: []workflow.StageSpec{{
			Key: "implementation", Name: "Implementation", Type: workflow.TypeImplementation,
			Gate: workflow.GateNone,
			Actions: []workflow.StageAction{
				{On: workflow.MomentEnter, Name: string(reaction.ActionOpenAttention),
					Params: map[string]string{"severity": "info"}},
				{On: workflow.MomentExit, Name: string(reaction.ActionProvisionBench)},
			},
		}},
	}
	created, err := repo.Create(ctx, &f, fmt.Sprintf("wf-actions-%d-%d", time.Now().UnixNano(), nextSeq()))
	if err != nil {
		t.Fatalf("creating the flow: %v", err)
	}

	// The frozen version is what a running demand reads, and what DecideStage
	// is handed. If the actions do not survive HERE, DecideStage can never be
	// reached from a real flow.
	frozen, err := repo.VersionOf(ctx, account, created.ID, created.Version)
	if err != nil {
		t.Fatalf("reading the frozen version: %v", err)
	}
	assertTheStageKeptItsActions(t, "VersionOf", frozen)

	byID, err := repo.ByID(ctx, account, created.ID)
	if err != nil {
		t.Fatalf("reading the flow: %v", err)
	}
	assertTheStageKeptItsActions(t, "ByID", byID)
}

func assertTheStageKeptItsActions(t *testing.T, read string, f *workflow.Flow) {
	t.Helper()
	if len(f.Stages) != 1 {
		t.Fatalf("%s: one stage was written, %d came back", read, len(f.Stages))
	}
	got := f.Stages[0].Actions
	if len(got) != 2 {
		t.Fatalf("%s: the stage declared 2 actions and %d came back — a flow whose "+
			"actions are dropped is indistinguishable from one that never had any", read, len(got))
	}
	if got[0].On != workflow.MomentEnter || got[0].Name != string(reaction.ActionOpenAttention) {
		t.Fatalf("%s: the entry action came back as %+v", read, got[0])
	}
	if got[0].Params["severity"] != "info" {
		t.Fatalf("%s: the action's params did not survive: %+v", read, got[0].Params)
	}
	if got[1].On != workflow.MomentExit || got[1].Name != string(reaction.ActionProvisionBench) {
		t.Fatalf("%s: the exit action came back as %+v", read, got[1])
	}
}
