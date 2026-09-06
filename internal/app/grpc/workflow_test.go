package grpc

import (
	"testing"
	"time"

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
