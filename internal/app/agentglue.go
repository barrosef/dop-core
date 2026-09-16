package app

import (
	"context"

	"github.com/barrosef/dop-core/internal/domain/agent"
	"github.com/barrosef/dop-core/internal/domain/cost"
	"github.com/barrosef/dop-core/internal/domain/demand"
	"github.com/barrosef/dop-core/internal/domain/execution"
	"github.com/barrosef/dop-core/internal/domain/knowledge"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// The glue between the agent runtime and the three domains it uses.
//
// The same discipline as glue.go, and it lives in a file of its own only so as
// not to mix the new glue with the one that already existed: each domain
// declares the NARROW port of what it needs from its neighbour, instead of
// importing that neighbour's package. The price is this file; what it buys is
// that `agent` does not know `knowledge`, `cost` and `demand` exist.
//
// It is deliberate that the glue is dull and mechanical: on the day one of these
// functions needs an `if` of business rule, the rule is in the wrong domain.

// ── knowledge → agent ───────────────────────────────────────────────────────

// agentKnowledge hands the runtime the demand's carry-on luggage.
//
// The budget goes in as ZERO on purpose: `BuildContextPackage` reads zero as "use
// the service's ceiling" (see knowledge.Budget.Normalize). Choosing a ceiling
// here would put context cost policy in the composition root, far from where it
// is decided and measured.
type agentKnowledge struct{ k *knowledge.Service }

var _ agent.Knowledge = agentKnowledge{}

func (a agentKnowledge) ContextPackage(ctx context.Context, demandID string) (agent.ContextPackage, error) {
	pkg, err := a.k.BuildContextPackage(ctx, demandID, knowledge.Budget{})
	if err != nil {
		return agent.ContextPackage{}, err
	}
	return agent.ContextPackage{
		Rules:    pkg.Rules,
		Index:    packageArtifacts(pkg.Index),
		Memories: packageArtifacts(pkg.Memories),
		Findings: achadosDoPacote(pkg.Findings),
		Dropped: agent.ContextDropped{
			Rules:    pkg.Dropped.Rules,
			Findings: pkg.Dropped.Findings,
			Index:    pkg.Dropped.Index,
			Memories: pkg.Dropped.Memories,
		},
	}, nil
}

// packageArtifacts converts while preserving the curation's ORDER: it is
// `SelectPackage`'s priority (ADR-0009 §3), and reordering here would undo the
// selection that consumed the whole budget.
//
// `ID` and `Version` are left behind because the runtime's port does not even
// have them: they change when the core rewrites the artifact without the content
// changing, and they would enter the cached prefix and invalidate it for nothing
// (ADR-0012 §1).
func packageArtifacts(itens []knowledge.Artifact) []agent.ContextArtifact {
	out := make([]agent.ContextArtifact, 0, len(itens))
	for _, a := range itens {
		out = append(out, agent.ContextArtifact{
			Name: a.Name, Body: a.Body, ObjectRef: a.ObjectRef,
		})
	}
	return out
}

func achadosDoPacote(itens []knowledge.Finding) []agent.ContextFinding {
	out := make([]agent.ContextFinding, 0, len(itens))
	for _, f := range itens {
		out = append(out, agent.ContextFinding{Title: f.Title, Summary: f.Summary})
	}
	return out
}

// ── cost → agent ────────────────────────────────────────────────────────────

// agentRouting wires the cost domain's decision and measurement.
//
// The CLASS crosses here — it is the field the network boundary used to eat when
// the runtime lived in the BFF, and it is what retires the reverse translation
// table that existed there (ADR-0023).
type agentRouting struct{ c *cost.Service }

var _ agent.Routing = agentRouting{}

func (a agentRouting) Route(ctx context.Context, taskKind, demandID string) (agent.Decision, error) {
	d, err := a.c.RouteModel(ctx, cost.TaskKind(taskKind), demandID)
	if err != nil {
		return agent.Decision{}, err
	}
	// The vocabularies match STRING FOR STRING (class and effort). The
	// conversion is a named-type swap, not a translation; if they ever diverge,
	// this is where it breaks — and breaking here is better than silently
	// routing to the wrong model.
	return agent.Decision{
		TaskKind: string(d.TaskKind),
		Class:    agent.ModelClass(d.Class),
		Model:    d.Model,
		Effort:   agent.Effort(d.Effort),
		Reason:   d.Reason,
	}, nil
}

func (a agentRouting) RecordUsage(ctx context.Context, c agent.Consumption, idemKey string) (agent.Accounting, error) {
	out, err := a.c.RecordUsage(ctx, cost.UsageEvent{
		DemandID:            c.DemandID,
		ThreadID:            c.ThreadID,
		Model:               c.Model,
		InputTokens:         c.InputTokens,
		OutputTokens:        c.OutputTokens,
		CacheReadTokens:     c.CacheReadTokens,
		CacheCreationTokens: c.CacheCreationTokens,
		CostMicros:          cost.Micros(c.CostMicros),
		Currency:            c.Currency,
		// AccountID and At are left out: the cost service takes them from the
		// context and from its own clock. Filling them in here would allow
		// posting consumption to the neighbour's account and writing into the
		// wrong partition.
	}, idemKey)
	if err != nil {
		return agent.Accounting{}, err
	}
	exceeded := make([]agent.BudgetView, 0, len(out.Exceeded))
	for _, b := range out.Exceeded {
		exceeded = append(exceeded, agent.BudgetView{
			Scope:       string(b.Scope),
			ScopeID:     b.ScopeID,
			LimitMicros: agent.Micros(b.LimitMicros),
			SpentMicros: agent.Micros(b.SpentMicros),
			Currency:    b.Currency,
		})
	}
	return agent.Accounting{BudgetExceeded: out.BudgetExceeded, Exceeded: exceeded}, nil
}

// ── demand → agent ──────────────────────────────────────────────────────────

// agentConversation wires the thread, the message and the finding.
//
// Note what does NOT pass through here: AUTHORSHIP. `demand.Service` reads it
// from `ctxutil.Call`, and it is the runtime that swaps the actor for the agent
// before publishing the reply. If authorship were a parameter of this glue, it
// would become something one can forget to pass — and the agent's utterance
// would enter the log as a human's, which is the one thing this system must not
// confuse.
type agentConversation struct{ d *demand.Service }

var _ agent.Conversation = agentConversation{}

func (a agentConversation) Thread(ctx context.Context, demandID, threadID string) (agent.Thread, error) {
	threads, err := a.d.ListThreads(ctx, demandID)
	if err != nil {
		return agent.Thread{}, err
	}
	for _, t := range threads {
		if t.ID == threadID {
			return agent.Thread{
				ID:  t.ID,
				Key: t.Key,
				Card: agent.AgentCard{
					Purpose:      t.Card.Purpose,
					Tools:        t.Card.Tools,
					Model:        t.Card.Model,
					Effort:       t.Card.Effort,
					BudgetMicros: t.Card.BudgetMicros,
				},
			}, nil
		}
	}
	// A nonexistent thread and a thread from ANOTHER demand come out as the
	// SAME error: `ListThreads` already filters by account and by demand, and
	// telling the two cases apart would leak the existence of other people's ids
	// to whoever kept trying.
	return agent.Thread{}, errs.NotFound("thread %s in this demand", threadID)
}

func (a agentConversation) PostMessage(ctx context.Context, threadID, text, idemKey string) (string, error) {
	m, err := a.d.PostMessage(ctx, threadID, text, idemKey)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (a agentConversation) PublishFinding(ctx context.Context, demandID, threadID, title string,
	payload map[string]any, idemKey string) (agent.FindingRef, error) {
	f, err := a.d.PublishFinding(ctx, demandID, threadID, title, payload, idemKey)
	if err != nil {
		return agent.FindingRef{}, err
	}
	return agent.FindingRef{ID: f.ID, Title: f.Title}, nil
}

// ── execution → agent ───────────────────────────────────────────────────────

// agentSandbox wires the tool loop to the executor.
//
// The conversion is mechanical on purpose: the agent domain speaks
// `SandboxCommand`, the execution one speaks `ports.ExecRequest`, and neither
// needs to know the other's vocabulary. If an `if` of business rule shows up
// here, the rule is in the wrong domain.
//
// This glue's ERROR CONTRACT is the delicate point: an error only when the
// EXECUTOR failed. A non-zero exit code, a blown deadline and a cut output are
// a RESULT — the model needs to see them in order to fix things, and turning
// them into a turn error would take from it exactly the information that would
// make it get the next round right.
type agentSandbox struct{ exec *execution.Service }

func (a agentSandbox) RunCommand(ctx context.Context, demandID string, cmd agent.SandboxCommand) (agent.SandboxOutput, error) {
	r, err := a.exec.RunCommand(ctx, demandID, ports.ExecRequest{
		Command:        cmd.Command,
		TimeoutSeconds: cmd.TimeoutSeconds,
		MaxOutputBytes: cmd.MaxOutputBytes,
	})
	if err != nil {
		return agent.SandboxOutput{}, err
	}
	return agent.SandboxOutput{
		ExitCode:  r.ExitCode,
		Stdout:    r.Stdout,
		Stderr:    r.Stderr,
		Truncated: r.Truncated,
		TimedOut:  r.TimedOut,
	}, nil
}
