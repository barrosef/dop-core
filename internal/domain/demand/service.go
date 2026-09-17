package demand

import (
	"context"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// Service concentrates the demand's rules. It takes only PORTS.
type Service struct {
	repo    Repository
	flows   FlowResolver
	watcher Watcher
	clock   ports.Clock
}

// NewService refuses a nil dependency.
//
// The panic here is deliberate, and for the same reason as identity.NewService:
// this is a WIRING error, and a wiring error has to surface at boot, not at
// three in the morning on the first demand somebody tries to start. Accepting
// nil and falling back internally is what turns a port into decoration.
func NewService(repo Repository, flows FlowResolver, watcher Watcher, clock ports.Clock) *Service {
	switch {
	case repo == nil:
		panic("demand.NewService: repository is required")
	case flows == nil:
		panic("demand.NewService: flow resolver is required — without it there is nothing to freeze")
	case watcher == nil:
		panic("demand.NewService: event subscription is required — WatchDemand depends on it")
	case clock == nil:
		panic("demand.NewService: clock is required — use clock.NewSystem()")
	}
	return &Service{repo: repo, flows: flows, watcher: watcher, clock: clock}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// ── reading ──────────────────────────────────────────────────────────────────

// List returns the project's demands, paginated by the id of the last one read.
func (s *Service) List(ctx context.Context, projectID string, size int, token string) ([]Demand, string, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, "", err
	}
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	// Ask for one more to know whether there is a next page without an extra count.
	list, err := s.repo.List(ctx, accountID, projectID, size+1, token)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(list) > size {
		list = list[:size]
		next = list[len(list)-1].ID
	}
	return list, next, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Demand, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, accountID, id)
}

func (s *Service) load(ctx context.Context, accountID, id string) (*Demand, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("demand not provided")
	}
	d, err := s.repo.ByID(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, errs.NotFound("demand")
	}
	return d, nil
}

// ── the start: where the flow is resolved and FROZEN ─────────────────────────

// Start resolves the project's effective flow and freezes it inside the demand.
//
// The freezing is this operation's whole point (ADR-0010 §4). A flow is
// editable, promotable and deletable; a demand in progress must not discover,
// halfway through, that the stage it was running stopped existing.
// After this, the stage machine obeys the SNAPSHOT — the live flow has no
// more power over this demand.
//
// Repeating the Start of the same external key returns the demand as it stands,
// re-resolving nothing: restarting would be precisely rewriting the past the
// freezing protects.
func (s *Service) Start(ctx context.Context, projectID, externalKey, idemKey string) (*Demand, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	projectID = strings.TrimSpace(projectID)
	externalKey = strings.TrimSpace(externalKey)
	if projectID == "" {
		return nil, errs.Invalid("a project is required to start a demand")
	}
	if externalKey == "" {
		return nil, errs.Invalid("an external key is required (e.g. SUOPT-1315)")
	}

	if existing, err := s.repo.ByExternalKey(ctx, accountID, projectID, externalKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	flow, err := s.flows.Resolve(ctx, accountID, "project", projectID)
	if err != nil {
		return nil, err
	}
	if err := validateFlow(flow); err != nil {
		return nil, err
	}

	now := s.now()
	snap := freeze(flow, now)
	d := &Demand{
		AccountID:   accountID,
		ProjectID:   projectID,
		ExternalKey: externalKey,
		// Title and card type come from the provider (Jira, ClickUp) through the
		// project's integration; until the sync happens, the external key is the
		// most honest label we have.
		Title:     externalKey,
		Status:    StatusNew,
		Flow:      snap,
		Stages:    instantiate(snap),
		CreatedBy: call.ActorID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	return s.repo.Create(ctx, d, Emission{
		Type: EventStarted,
		Payload: map[string]any{
			"project_id":    projectID,
			"external_key":  externalKey,
			"flow_id":       snap.FlowID,
			"flow_version":  snap.Version,
			"resolved_from": snap.ResolvedFrom,
			"stages":        stageKeys(snap),
		},
	}, idemKey)
}

// instantiate turns the frozen mould into the demand's stages. They are all
// born
// pending: progress is an event, not an initial state.
func instantiate(snap Snapshot) []Stage {
	out := make([]Stage, 0, len(snap.Stages))
	for i, spec := range snap.Stages {
		gate := spec.Gate
		if gate == "" {
			gate = GateNone
		}
		out = append(out, Stage{
			Key: spec.Key, Name: spec.Name, Type: spec.Type,
			Gate: gate, Status: StagePending, Position: i,
		})
	}
	return out
}

// validateFlow recusa congelar um fluxo quebrado.
//
// The validation is here, and not only in the flow domain, because this is the
// instant the mould becomes an immutable past: a flow with no stages or with a
// repeated key frozen into a demand is a defect no later fix of the flow
// desfaz.
func validateFlow(f Flow) error {
	if len(f.Stages) == 0 {
		return errs.Precondition(
			"the effective flow %q has no stages: nothing to execute", f.Name)
	}
	seen := make(map[string]bool, len(f.Stages))
	for _, st := range f.Stages {
		if strings.TrimSpace(st.Key) == "" {
			return errs.Precondition("the effective flow %q has a stage with no key", f.Name)
		}
		if seen[st.Key] {
			return errs.Precondition(
				"the effective flow %q repeats the stage key %q", f.Name, st.Key)
		}
		seen[st.Key] = true
	}
	return nil
}

func stageKeys(snap Snapshot) []string {
	keys := make([]string, 0, len(snap.Stages))
	for _, st := range snap.Stages {
		keys = append(keys, st.Key)
	}
	return keys
}

// ── the stage machine ────────────────────────────────────────────────────────

// AdvanceStage moves a stage inside the machine driven by its TYPE (ADR-0010).
func (s *Service) AdvanceStage(ctx context.Context, demandID, stageKey string, to StageStatus, idemKey string) (*Stage, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	st, noop, err := d.CheckAdvance(stageKey, to)
	if err != nil {
		return nil, err
	}
	if noop {
		return st, nil
	}

	now := s.now()
	from := st.Status
	st.Status = to
	switch to {
	case StageRunning:
		if st.StartedAt == nil {
			st.StartedAt = &now
		}
	case StageDone:
		st.FinishedAt = &now
	}

	return s.repo.SaveStage(ctx, accountID, d.ID, *st, d.ProjectStatus(), Emission{
		Type: EventStageAdvanced,
		Payload: map[string]any{
			"stage_key": st.Key, "stage_type": string(st.Type),
			"from": string(from), "to": string(to),
			// The GATE travels in the event: the attention box has to tell apart a
			// "a stage stopped waiting for a person" from "a stage stopped for
			// another reason", and without this it would have to query the demand to
			// decide — a projection that queries state stops being a projection.
			"gate":       string(st.Gate),
			"dop_status": string(d.ProjectStatus()),
		},
	}, idemKey)
}

// DecideGate records the gate's human decision.
//
// Rejecting does NOT send the stage back to pending: it goes to blocked, with
// the comment. Clearing the stage would erase from the screen the fact that there
// was a rejection — and that fact is half the value of human validation.
func (s *Service) DecideGate(ctx context.Context, demandID, stageKey string, approved bool, comment, idemKey string) (*Stage, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorKind == ctxutil.ActorAgent || call.ActorKind == ctxutil.ActorSubagent {
		return nil, errs.Permission(
			"a human gate is decided by people: an agent does not approve its own stage")
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	st, err := d.CheckDecideGate(stageKey)
	if err != nil {
		return nil, err
	}

	now := s.now()
	st.GateApproved = &approved
	st.GateComment = comment
	if approved {
		st.Status = StageDone
		st.FinishedAt = &now
	} else {
		st.Status = StageBlocked
	}

	return s.repo.SaveStage(ctx, accountID, d.ID, *st, d.ProjectStatus(), Emission{
		Type: EventGateDecided,
		Payload: map[string]any{
			"stage_key": st.Key, "stage_type": string(st.Type),
			"approved": approved, "comment": comment,
			"to": string(st.Status), "dop_status": string(d.ProjectStatus()),
		},
	}, idemKey)
}

// ── threads (ADR-0007) ───────────────────────────────────────────────────────

func (s *Service) ListThreads(ctx context.Context, demandID string) ([]Thread, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.load(ctx, accountID, demandID); err != nil {
		return nil, err
	}
	return s.repo.ThreadsOf(ctx, accountID, demandID)
}

// CreateThread launches a subagent: the thread is born together with its BRIEF
// (ADR-0007 §2) and shows up at once for the dev to follow or step in.
func (s *Service) CreateThread(ctx context.Context, demandID, key string, card AgentCard, idemKey string) (*Thread, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if err := ValidateThreadKey(key); err != nil {
		return nil, err
	}
	if err := validateCard(card); err != nil {
		return nil, err
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}

	now := s.now()
	t := &Thread{
		AccountID: accountID, DemandID: d.ID, Key: strings.TrimSpace(key),
		Card: card, State: ThreadOpen, CreatedBy: call.ActorID,
		CreatedAt: now, UpdatedAt: now,
	}
	return s.repo.CreateThread(ctx, t, Emission{
		Type: EventThreadCreated,
		Payload: map[string]any{
			"thread_key": t.Key, "purpose": card.Purpose,
			"model": card.Model, "effort": card.Effort,
			"tools": card.Tools, "budget_micros": card.BudgetMicros,
		},
	}, idemKey)
}

// validateCard: a subagent with no brief is a black box, which is exactly what
// ADR-0007 refuses. Purpose and model are the minimum for the dev to know who
// they are talking to and for the router to know what it costs.
func validateCard(c AgentCard) error {
	if strings.TrimSpace(c.Purpose) == "" {
		return errs.Invalid("the agent brief requires a purpose")
	}
	if c.BudgetMicros < 0 {
		return errs.Invalid("the agent budget cannot be negative")
	}
	switch c.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return errs.Invalid("unknown effort: %q", c.Effort)
	}
	return nil
}

func (s *Service) loadThread(ctx context.Context, accountID, id string) (*Thread, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("thread not provided")
	}
	t, err := s.repo.ThreadByID(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errs.NotFound("thread")
	}
	return t, nil
}

// PostMessage appends the message to the demand's log (ADR-0004): every message
// is an event. The thread leaves `open` and becomes `active` in the same transaction.
func (s *Service) PostMessage(ctx context.Context, threadID, text, idemKey string) (*Message, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if strings.TrimSpace(text) == "" {
		return nil, errs.Invalid("mensagem vazia")
	}
	t, err := s.loadThread(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	if err := t.CheckPost(); err != nil {
		return nil, err
	}

	m := &Message{
		AccountID: accountID, DemandID: t.DemandID, ThreadID: t.ID,
		AuthorID: call.ActorID, AuthorKind: string(call.ActorKind),
		AuthorName: call.ActorName, Text: text, At: s.now(),
	}
	return s.repo.AppendMessage(ctx, m, Emission{
		Type: EventMessagePosted,
		Payload: map[string]any{
			"thread_id": t.ID, "thread_key": t.Key, "text": text,
			"author_kind": m.AuthorKind, "author_id": m.AuthorID,
		},
	}, idemKey)
}

// SetThreadBlocked marks (or clears) the pending question.
//
// It is what feeds the attention box: a blocked thread is a queue item, and
// without that state the multi-agent model drowns the dev (spec §3, risk R-2).
// It has no RPC of its own yet; today the agent runtime calls it from the edge.
func (s *Service) SetThreadBlocked(ctx context.Context, threadID string, blocked bool, reason, idemKey string) (*Thread, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.loadThread(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	if t.State == ThreadConcluded {
		return nil, errs.Precondition("thread %q is concluded", t.Key)
	}
	state, evType := ThreadBlocked, EventThreadBlocked
	if !blocked {
		state, evType = ThreadActive, EventThreadResumed
	}
	if t.State == state {
		return t, nil
	}
	return s.repo.SaveThreadState(ctx, accountID, t.ID, state, Emission{
		Type: evType,
		Payload: map[string]any{
			"thread_id": t.ID, "thread_key": t.Key, "reason": reason,
		},
	}, idemKey)
}

// PublishFinding publishes the investigation's structured conclusion.
//
// It is what enters the siblings' context, the dossier and the project's memory
// (ADR-0007 §4) — and it is what UNLOCKS concluding the thread: with no finding
// publicado, ConcludeThread recusa.
func (s *Service) PublishFinding(ctx context.Context, demandID, threadID, title string, payload map[string]any, idemKey string) (*Finding, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if strings.TrimSpace(title) == "" {
		return nil, errs.Invalid("the finding needs a title")
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	t, err := s.loadThread(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	// A thread of another demand posting on this one's board would be a leak of
	// context between demands — and the demand is the security boundary
	// (ADR-0007 §6).
	if t.DemandID != d.ID {
		return nil, errs.Invalid("thread %q does not belong to this demand", t.Key)
	}
	if payload == nil {
		payload = map[string]any{}
	}

	f := &Finding{
		AccountID: accountID, DemandID: d.ID, ThreadID: t.ID,
		Title: title, Payload: payload, CreatedBy: call.ActorID,
		CreatedAt: s.now(),
	}
	return s.repo.CreateFinding(ctx, f, Emission{
		Type: EventFindingPublished,
		Payload: map[string]any{
			"thread_id": t.ID, "thread_key": t.Key,
			"title": title, "finding": payload,
		},
	}, idemKey)
}

// ConcludeThread closes the conversation — and only accepts it once the finding
//
// It is the spec's literal rule: "concluding requires publishing the finding;
// the thread does not die in silence". An investigation that ends with no
// finding disappears with the transcript, and the next agent redoes the work.
func (s *Service) ConcludeThread(ctx context.Context, threadID, idemKey string) (*Thread, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.loadThread(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	has, err := s.repo.HasFinding(ctx, accountID, t.ID)
	if err != nil {
		return nil, err
	}
	if err := t.CheckConclude(has); err != nil {
		return nil, err
	}
	if t.State == ThreadConcluded {
		return t, nil
	}
	return s.repo.SaveThreadState(ctx, accountID, t.ID, ThreadConcluded, Emission{
		Type:    EventThreadConcluded,
		Payload: map[string]any{"thread_id": t.ID, "thread_key": t.Key},
	}, idemKey)
}

// ── streaming ────────────────────────────────────────────────────────────────

// Watch delivers THIS demand's events live.
//
// The fan-out, the replay and the per-account isolation belong to the event
// service — here we only slice. The slice by demand is done on this side because
// the bus filter is by aggregate and type, not by id: subscribing to "demand"
// and discarding what belongs to another demand costs one string comparison per
// event and avoids duplicating the subscription machinery.
//
// The demand is loaded BEFORE opening the stream: a client asking for a demand
// that does not exist, or belongs to another account, gets a 404 right away and
// not a mute stream it would read as "nothing has happened yet".
func (s *Service) Watch(ctx context.Context, demandID string, emit func(ports.Event) error) error {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return err
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return err
	}
	return s.watcher.Watch(ctx, "", []string{Aggregate}, nil, func(e ports.Event) error {
		if e.AggregateID != d.ID {
			return nil
		}
		return emit(e)
	})
}

// Findings returns the findings published on the demand.
//
// No RPC of its own in the contract yet; it exists because the context package
// assembler needs them (ADR-0006) and the findings board is precisely what
// keeps an agent from redoing an investigation another already finished.
func (s *Service) Findings(ctx context.Context, demandID string) ([]Finding, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	// It goes through Get first: that guarantees the demand belongs to THIS
	// account, not merely that it exists — filtering only in ListFindings would
	// let another account's id return empty instead of denying, which leaks existence.
	if _, err := s.Get(ctx, demandID); err != nil {
		return nil, err
	}
	return s.repo.ListFindings(ctx, accountID, demandID)
}
