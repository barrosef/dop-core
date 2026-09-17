package agentmetrics

import (
	"context"
	"strings"

	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Service is the ingestion and the reading of the metrics.
//
// ── Who may write here, and why it is not the usual rule ────────────────────
//
// Everywhere else in this core the actor is a person or an agent acting for
// one. Here the writer is the COLLECTOR: a container beside the agent in the
// sandbox's pod, which has no person behind it and never will.
//
// So the authorization is by CALLER, not by role (ADR-0022): only a call signed
// as `collector` writes, and that caller reaches this service and nothing else.
// A stolen collector key is worth polluting telemetry — never reading an
// account. The reading side is the ordinary rule: whoever is in the account.
type Service struct{ repo Repository }

func NewService(repo Repository) *Service {
	if repo == nil {
		panic("agentmetrics.NewService: repository is required")
	}
	return &Service{repo: repo}
}

// CollectorCaller is the name the collector signs its assertions with.
const CollectorCaller = "collector"

// Ingest records a batch of turns and advances the cursor.
//
// It returns how many were NEW. A batch fully seen before answers zero, and
// that is the collector's signal that it is idle rather than broken — it
// re-reads the tail of a file that is still being written, so a repeat is the
// normal case.
func (s *Service) Ingest(ctx context.Context, in Session, turns []Turn) (int, error) {
	call, _ := ctxutil.From(ctx)
	if call.Caller != CollectorCaller {
		return 0, errs.Permission("only the collector writes metrics").
			WithCode(KeyOnlyCollector, nil)
	}
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(in.ExternalID) == "" {
		return 0, errs.Invalid("a batch with no session").WithCode(KeySessionMissing, nil)
	}
	in.AccountID = accountID

	stored, err := s.repo.EnsureSession(ctx, in)
	if err != nil {
		return 0, err
	}
	// What the collector has ALREADY had accepted is dropped here rather than at
	// the database: the cursor is the cheap filter, and re-sending is normal.
	fresh := make([]Turn, 0, len(turns))
	for _, t := range turns {
		if strings.TrimSpace(t.UUID) == "" {
			continue
		}
		fresh = append(fresh, t)
	}
	if err := s.repo.RecordTurns(ctx, stored.ID, fresh, in.ByteOffset); err != nil {
		return 0, err
	}
	return len(fresh), nil
}

// Consumption reads a demand's aggregate. Ordinary authorization: the active
// account's, and a demand of another account simply is not found.
func (s *Service) Consumption(ctx context.Context, demandID string) (*Consumption, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("demand not provided").WithCode(KeyDemandMissing, nil)
	}
	return s.repo.ConsumptionOf(ctx, accountID, demandID)
}

// Translation keys for the refusals a person reads.
const (
	KeyOnlyCollector  = "metrics.only_collector"
	KeySessionMissing = "metrics.session_missing"
	KeyDemandMissing  = "metrics.demand_missing"
)
