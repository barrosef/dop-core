package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/attention"
	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/hierarchy"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/secondfactor"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
)

// The glue between domains.
//
// Each domain declares the NARROW port of what it needs from its neighbour,
// instead of importing that neighbour's package. The price is this file; what it
// buys is that `demand` does not know `workflow` exists, and neither breaks when
// the other changes shape internally.
//
// It is deliberate that the glue is dull and mechanical: on the day one of these
// functions needs an `if` of business rule, the rule is in the wrong domain.

// ── workflow → demand ───────────────────────────────────────────────────────

// demandFlows resolves the effective flow for the demand to freeze.
//
// The two vocabularies match STRING FOR STRING (`StageType`, `ArtifactKind`,
// gate and scope), so the conversion is a named-type swap, not a translation. If
// they ever diverge, this is where it breaks — and breaking here is better than
// silently freezing a flow with a stage of an unknown type.
type demandFlows struct{ wf *workflow.Service }

func (a demandFlows) Resolve(ctx context.Context, _ string, scope, scopeID string) (demand.Flow, error) {
	// The account comes from the context in the flow service — the port's
	// accountID parameter exists in case another adapter needs it.
	ef, err := a.wf.Resolve(ctx, workflow.Scope(scope), scopeID)
	if err != nil {
		return demand.Flow{}, err
	}
	stages := make([]demand.StageSpec, 0, len(ef.Flow.Stages))
	for _, s := range ef.Flow.Stages {
		artifacts := make([]demand.ArtifactKind, 0, len(s.Artifacts))
		for _, a := range s.Artifacts {
			artifacts = append(artifacts, demand.ArtifactKind(a))
		}
		stages = append(stages, demand.StageSpec{
			Key:       s.Key,
			Name:      s.Name,
			Type:      demand.StageType(s.Type),
			Gate:      demand.Gate(s.Gate),
			Artifacts: artifacts,
			Subtypes:  s.Subtypes,
		})
	}
	return demand.Flow{
		ID:           ef.Flow.ID,
		Name:         ef.Flow.Name,
		Version:      ef.Flow.Version,
		ResolvedFrom: ef.ResolvedFrom,
		Stages:       stages,
	}, nil
}

// ── event → demand ──────────────────────────────────────────────────────────

// demandWatcher hands the demand domain the fan-out the event domain already
// has — replay, per-account isolation and slow-consumer policy included. A
// second fan-out implementation would be a second chance to get isolation
// between accounts wrong.
type demandWatcher struct{ ev *event.Service }

func (a demandWatcher) Watch(ctx context.Context, since string, aggregates, types []string, emit func(ports.Event) error) error {
	return a.ev.Watch(ctx, since, event.Filter{Aggregates: aggregates, Types: types}, emit)
}

// ── identity → workflow ─────────────────────────────────────────────────────

// workflowAccess adapts identity to the flow domain's narrow port, which speaks
// ROLE as a plain string: the role vocabulary belongs to the identity domain, and
// importing it inside workflow would couple two domains that do not need to know
// each other.
type workflowAccess struct{ id *identity.Service }

func (a workflowAccess) RoleOf(ctx context.Context, userID, accountID string) (string, error) {
	m, err := a.id.Authorize(ctx, userID, accountID)
	if err != nil {
		return "", err
	}
	return string(m.Role), nil
}

// ── demand → knowledge ──────────────────────────────────────────────────────

// knowledgeDemands answers "what this demand is" for the context assembler.
//
// The knowledge domain receives only a `demand_id` and needs the project, the
// title, the spec, the repositories and the findings — which live in three
// places. Putting that together is composition's work, not either domain's.
type knowledgeDemands struct {
	demands   *demand.Service
	hierarchy *hierarchy.Service
}

func (a knowledgeDemands) ContextOf(ctx context.Context, _ string, demandID string) (*knowledge.DemandContext, error) {
	d, err := a.demands.Get(ctx, demandID)
	if err != nil {
		return nil, err
	}

	// The findings ALREADY published are the resumption layer: without them, an
	// agent that picks the demand up midway redoes an investigation another has
	// concluded — the exact waste the board of findings exists to avoid
	// (ADR-0009).
	findings, err := a.demands.Findings(ctx, demandID)
	if err != nil {
		return nil, err
	}
	converted := make([]knowledge.Finding, 0, len(findings))
	for _, f := range findings {
		converted = append(converted, knowledge.Finding{
			ID:       f.ID,
			ThreadID: f.ThreadID,
			Title:    f.Title,
			Summary:  findingSummary(f),
		})
	}

	// The repositories carve out the code index: the package brings the index OF
	// THE DEMAND'S REPOS, never of the whole project. Failing here is not worth
	// the trip — without the list, the index comes out empty instead of coming
	// out wrong.
	var repos []string
	if p, err := a.hierarchy.GetProject(ctx, d.ProjectID); err == nil {
		for _, r := range p.Repos {
			repos = append(repos, r.ID)
		}
	}

	return &knowledge.DemandContext{
		DemandID:  d.ID,
		ProjectID: d.ProjectID,
		Title:     d.Title,
		// Spec stays empty: it is a stage ARTIFACT, and per-stage artifact
		// storage does not exist yet. A declared degradation, not an oversight —
		// the package loses the spec, it does not become incorrect.
		Repos:    repos,
		Findings: converted,
	}, nil
}

// findingSummary extracts the finding's text from the free-form payload.
//
// The payload is a `map[string]any` because the finding's format belongs to the
// agent that published it, not to the platform. Accepting the two likeliest keys
// and falling back to the title is better than demanding a schema — a finding
// with no summary is still worth more in the context than an absent finding.
func findingSummary(f demand.Finding) string {
	// "resumo" is kept as a legacy key: findings published before the platform
	// standardized on English are still in the database, and dropping it here
	// would silently empty their summary in the context.
	for _, key := range []string{"summary", "resumo"} {
		if v, ok := f.Payload[key].(string); ok && v != "" {
			return v
		}
	}
	return f.Title
}

// ── demand → delivery ───────────────────────────────────────────────────────

// deliveryDemands is READ-ONLY, and that is ADR-0015 §5's rule turned into a
// type: delivery has no way to stop any demand, because the port offers none.
// `Active` exists so the event tells the truth — "the directive was decided and
// demand 1 keeps running" — never to decide whether it stops.
type deliveryDemands struct{ d *demand.Service }

func (a deliveryDemands) Demand(ctx context.Context, _ string, id string) (*delivery.DemandInfo, error) {
	dm, err := a.d.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &delivery.DemandInfo{
		ID:        dm.ID,
		ProjectID: dm.ProjectID,
		Active:    dm.Status != demand.StatusDelivered,
	}, nil
}

// ── demand → execution ──────────────────────────────────────────────────────

// executionDemands answers whose the demand is, and nothing else.
//
// A nonexistent demand and a demand from ANOTHER account arrive here as the SAME
// error, because `demand.Service.Get` already filters by account: telling the
// two cases apart would leak the existence of other people's ids to whoever kept
// trying.
type executionDemands struct{ d *demand.Service }

func (a executionDemands) DemandAccount(ctx context.Context, demandID string) (string, error) {
	dm, err := a.d.Get(ctx, demandID)
	if err != nil {
		return "", err
	}
	return dm.AccountID, nil
}

// ── event → attention ───────────────────────────────────────────────────────

// attentionWatcher hands the box the event domain's same fan-out, with replay,
// per-account isolation and the slow-consumer policy already solved.
type attentionWatcher struct{ ev *event.Service }

func (a attentionWatcher) Watch(ctx context.Context, since string, aggregates, types []string, emit func(attention.Event) error) error {
	return a.ev.Watch(ctx, since, event.Filter{Aggregates: aggregates, Types: types}, func(e ports.Event) error {
		// The payload arrives as the envelope's bytes; the box's rule works with
		// a map. Decoding here — and not in the domain — keeps the domain from
		// knowing there is JSON along the way.
		var env struct {
			ID          string         `json:"id"`
			AccountID   string         `json:"account_id"`
			Aggregate   string         `json:"aggregate"`
			AggregateID string         `json:"aggregate_id"`
			Type        string         `json:"type"`
			Payload     map[string]any `json:"payload"`
			OccurredAt  time.Time      `json:"occurred_at"`
		}
		if err := json.Unmarshal(e.Payload, &env); err != nil {
			return nil // unreadable does not improve with a retry
		}
		return emit(attention.Event{
			ID: env.ID, AccountID: env.AccountID, Aggregate: env.Aggregate,
			AggregateID: env.AggregateID, Type: env.Type,
			OccurredAt: env.OccurredAt, Payload: env.Payload,
		})
	})
}

// ── identity → secondfactor ─────────────────────────────────────────────────

// secondFactorUsers is the NARROW slice of identity the second factor needs:
// who the person is (to address the code and to label the QR) and which scope
// their seed lives in.
//
// It does not receive the whole identity service by accident — the second factor
// must not be able to change a role or read an invite.
type secondFactorUsers struct{ id *identity.Service }

func (u secondFactorUsers) UserProfile(ctx context.Context, userID string) (string, string, bool, error) {
	usr, err := u.id.GetUser(ctx, userID)
	if err != nil {
		return "", "", false, err
	}
	return usr.Email, usr.Name, usr.EmailVerified, nil
}

func (u secondFactorUsers) PersonalAccountOf(ctx context.Context, userID string) (string, error) {
	return u.id.PersonalAccountOf(ctx, userID)
}

var _ secondfactor.Users = secondFactorUsers{}
