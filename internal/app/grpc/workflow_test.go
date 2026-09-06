package grpc

import (
	"testing"
	"time"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
)

// TestPublicationToProtoRendersTheReferenceServerSide is the one thing task
// 10's brief singles out: the reference is FORMATTED BY THE SERVER, from the
// publisher's handle plus the publication's own slug and version — never
// assembled by a client. workflow.PublicationRef.String is unit-tested in
// isolation elsewhere; this proves the COMPOSITION through publicationToProto
// actually produces the documented shape, "@acme/backend-go@v3".
func TestPublicationToProtoRendersTheReferenceServerSide(t *testing.T) {
	published := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	p := &workflow.Publication{
		ID:          "pub-1",
		FlowID:      "flow-1",
		AccountID:   "acct-1",
		Slug:        "backend-go",
		Version:     3,
		Notes:       "third cut",
		PublishedAt: published,
	}

	out := publicationToProto(p, "acme")

	if out.GetReference() != "@acme/backend-go@v3" {
		t.Fatalf("reference = %q, want %q", out.GetReference(), "@acme/backend-go@v3")
	}
	if out.GetId() != p.ID || out.GetFlowId() != p.FlowID {
		t.Fatalf("id/flow_id not carried through: got id=%q flow_id=%q", out.GetId(), out.GetFlowId())
	}
	if out.GetVersion() != p.Version {
		t.Fatalf("version = %d, want %d", out.GetVersion(), p.Version)
	}
	if out.GetNotes() != p.Notes {
		t.Fatalf("notes = %q, want %q", out.GetNotes(), p.Notes)
	}
	if !out.GetPublishedAt().AsTime().Equal(published) {
		t.Fatalf("published_at = %v, want %v", out.GetPublishedAt().AsTime(), published)
	}
	// A publication still in circulation carries no withdrawn_at at all — not a
	// zero-value Timestamp, which would read as "withdrawn at the Unix epoch".
	if out.GetWithdrawnAt() != nil {
		t.Fatalf("withdrawn_at = %v, want nil for a publication never withdrawn", out.GetWithdrawnAt())
	}
}

// TestPublicationToProtoUnpinnedHasNoVersionSuffix confirms the OTHER shape
// PublicationRef.String produces: a publication that IS the latest still
// renders without "@vN" — the suffix exists only for a version deliberately
// pinned, and the two shapes are easy to conflate at the formatting call site.
func TestPublicationToProtoUnpinnedHasNoVersionSuffix(t *testing.T) {
	p := &workflow.Publication{Slug: "backend-go", Version: 0}
	out := publicationToProto(p, "acme")
	if out.GetReference() != "@acme/backend-go" {
		t.Fatalf("reference = %q, want %q", out.GetReference(), "@acme/backend-go")
	}
}

// TestPublicationToProtoCarriesWithdrawnAt proves the other half of the
// IsZero guard: a publication that WAS withdrawn renders a real timestamp, not
// nil.
func TestPublicationToProtoCarriesWithdrawnAt(t *testing.T) {
	withdrawn := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	p := &workflow.Publication{Slug: "backend-go", Version: 1, WithdrawnAt: withdrawn}
	out := publicationToProto(p, "acme")
	if out.GetWithdrawnAt() == nil || !out.GetWithdrawnAt().AsTime().Equal(withdrawn) {
		t.Fatalf("withdrawn_at = %v, want %v", out.GetWithdrawnAt(), withdrawn)
	}
}

// TestAStageActionSurvivesBothMapperDirections covers the half the Postgres
// round trip cannot reach: a flow authored through the API arrives as proto and
// leaves as proto, and a mapper that drops `actions` in either direction makes
// a stage that declared one indistinguishable from a stage that never did —
// with no error anywhere, which is the whole reason DecideStage was unreachable
// from a real flow.
func TestAStageActionSurvivesBothMapperDirections(t *testing.T) {
	in := &dopv1.Flow{
		Id: "flow-1", Name: "with actions", Version: 3,
		OwnerScope: "account", OwnerId: "acct-1",
		Stages: []*dopv1.StageSpec{{
			Key: "implementation", Name: "Implementation",
			Type: dopv1.StageType_STAGE_TYPE_IMPLEMENTATION,
			Gate: dopv1.Gate_GATE_NONE,
			Actions: []*dopv1.StageAction{
				{On: dopv1.StageMoment_STAGE_MOMENT_EXIT, Name: "provision_bench",
					Params: map[string]string{"size": "large"}},
			},
		}},
	}

	domain := flowFromProto(in)
	if len(domain.Stages[0].Actions) != 1 {
		t.Fatalf("inbound: the stage declared one action and %d arrived", len(domain.Stages[0].Actions))
	}
	got := domain.Stages[0].Actions[0]
	if got.On != workflow.MomentExit || got.Name != "provision_bench" || got.Params["size"] != "large" {
		t.Fatalf("inbound: the action arrived as %+v", got)
	}

	out := flowToProto(&domain)
	if len(out.GetStages()[0].GetActions()) != 1 {
		t.Fatalf("outbound: the action did not come back")
	}
	back := out.GetStages()[0].GetActions()[0]
	if back.GetOn() != dopv1.StageMoment_STAGE_MOMENT_EXIT ||
		back.GetName() != "provision_bench" || back.GetParams()["size"] != "large" {
		t.Fatalf("outbound: the action came back as %+v", back)
	}
}

// TestAMomentTheContractDoesNotKnowStaysEmpty pins the one place this mapper
// deliberately refuses the gate's habit of defaulting. Guessing "enter" would
// fire the action at a moment nobody wrote; the empty string is what makes
// workflow.Validate name the problem instead.
func TestAMomentTheContractDoesNotKnowStaysEmpty(t *testing.T) {
	if m := flowMomentFromProto(dopv1.StageMoment_STAGE_MOMENT_UNSPECIFIED); m != "" {
		t.Fatalf("an unspecified moment must not be guessed, got %q", m)
	}
}
