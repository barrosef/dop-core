package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/agent"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ── doubles for the four ports ──────────────────────────────────────────────

type fakeProvider struct {
	info    agent.ProviderInfo
	reply   *agent.Reply
	failure error
	// replies is the QUEUE of responses, one per loop round. When it runs out,
	// the last one repeats — that is what allows exercising an agent that keeps
	// asking for a tool and hits the cap. Empty, `reply` applies to every round,
	// which is the behaviour from before the loop.
	replies []*agent.Reply

	askedModel  string
	askedEffort agent.Effort
	askedTurn   agent.Turn
	// turns keeps ALL the turns sent, and it is what allows asking whether the
	// tool's result went back to the model on the next round.
	turns []agent.Turn
}

func (p *fakeProvider) Info() agent.ProviderInfo { return p.info }

func (p *fakeProvider) Render(t agent.Turn, model string, e agent.Effort) ([]byte, []string, error) {
	b, err := json.Marshal(map[string]any{"model": model, "prefix": t.StablePrefix})
	return b, nil, err
}

func (p *fakeProvider) Send(_ context.Context, t agent.Turn, model string, e agent.Effort) (*agent.Reply, error) {
	p.askedModel, p.askedEffort, p.askedTurn = model, e, t
	p.turns = append(p.turns, t)
	if p.failure != nil {
		return nil, p.failure
	}
	current := p.reply
	if n := len(p.replies); n > 0 {
		i := len(p.turns) - 1
		if i >= n {
			i = n - 1 // the last one repeats: an agent that insists
		}
		current = p.replies[i]
	}
	r := *current
	if r.Capabilities == nil {
		r.Capabilities = p.info.Capabilities
	}
	if r.Provider == "" {
		r.Provider = p.info.Name
	}
	return &r, nil
}

func (p *fakeProvider) For(context.Context, string) (agent.AgentProvider, error) { return p, nil }

type fakeKnowledge struct{ pkg agent.ContextPackage }

func (c fakeKnowledge) ContextPackage(context.Context, string) (agent.ContextPackage, error) {
	return c.pkg, nil
}

type fakeCost struct {
	decision   agent.Decision
	accounting agent.Accounting
	usages     []agent.Consumption
	usageKeys  []string
}

func (c *fakeCost) Route(context.Context, string, string) (agent.Decision, error) {
	return c.decision, nil
}

func (c *fakeCost) RecordUsage(_ context.Context, u agent.Consumption, k string) (agent.Accounting, error) {
	c.usages = append(c.usages, u)
	c.usageKeys = append(c.usageKeys, k)
	return c.accounting, nil
}

// recordedMessage keeps what the event log would keep — in particular WHO spoke,
// which is what the turn's cycle decides.
type recordedMessage struct {
	text      string
	key       string
	actorKind ctxutil.ActorKind
	actorID   string
	actorName string
}

type fakeConversation struct {
	thread   agent.Thread
	messages []recordedMessage
	findings []recordedMessage
}

func (c *fakeConversation) Thread(context.Context, string, string) (agent.Thread, error) {
	return c.thread, nil
}

func (c *fakeConversation) PostMessage(ctx context.Context, _, text, idemKey string) (string, error) {
	call, _ := ctxutil.From(ctx)
	c.messages = append(c.messages, recordedMessage{
		text: text, key: idemKey,
		actorKind: call.ActorKind, actorID: call.ActorID, actorName: call.ActorName,
	})
	return "msg-" + idemKey, nil
}

func (c *fakeConversation) PublishFinding(ctx context.Context, _, _, title string,
	payload map[string]any, idemKey string) (agent.FindingRef, error) {
	call, _ := ctxutil.From(ctx)
	c.findings = append(c.findings, recordedMessage{
		text: title, key: idemKey, actorKind: call.ActorKind, actorID: call.ActorID,
	})
	_ = payload
	return agent.FindingRef{ID: "fnd-1", Title: title}, nil
}

// ── assembly ────────────────────────────────────────────────────────────────

func callCtx() context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		RequestID: "req-1", AccountID: "acct-1",
		ActorID: "usr-ana", ActorKind: ctxutil.ActorUser, ActorName: "Ana",
	})
}

func providerSheet() agent.ProviderInfo {
	return agent.ProviderInfo{
		Name: "provider-x",
		Catalog: map[agent.ModelClass]string{
			agent.ClassCheap:  "x-small",
			agent.ClassMedium: "x-medium",
			agent.ClassStrong: "x-large",
		},
		Capabilities: agent.Capabilities{agent.CapCacheCreationAccounting},
		Prices: map[string]agent.Price{
			"x-large": {Currency: "USD", InputPer1k: 5_000, OutputPer1k: 25_000,
				CacheReadPer1k: 500, CacheCreationPer1k: 6_250},
		},
	}
}

func concludingReply() *agent.Reply {
	return &agent.Reply{
		Text:  `{"reply":"done"}`,
		Model: "x-large",
		Usage: agent.Usage{InputTokens: 1000, OutputTokens: 200,
			CacheReadTokens: 400, CacheCreationTokens: 100},
		StopReason:    agent.StopCompleted,
		EffortApplied: agent.EffortHigh,
		Data: map[string]any{
			"reply": "done", "concluded": true,
			"finding_title": "the bug was in the cache", "finding_summary": "a concrete summary",
			"finding_evidence": []any{"the log on line 42"},
		},
	}
}

type scenario struct {
	svc  *agent.Service
	prov *fakeProvider
	cost *fakeCost
	conv *fakeConversation
}

func setup(t *testing.T, pkg agent.ContextPackage, reply *agent.Reply,
	card agent.AgentCard, accounting agent.Accounting) scenario {
	t.Helper()
	prov := &fakeProvider{info: providerSheet(), reply: reply}
	cost := &fakeCost{
		decision: agent.Decision{TaskKind: "implementation", Class: agent.ClassStrong,
			Model: "claude-opus", Effort: agent.EffortHigh, Reason: "ADR-0011 §3: because so"},
		accounting: accounting,
	}
	conv := &fakeConversation{thread: agent.Thread{ID: "thr-1", Key: "main", Card: card}}
	return scenario{
		svc:  agent.NewService(prov, fakeKnowledge{pkg}, cost, conv),
		prov: prov, cost: cost, conv: conv,
	}
}

func request() agent.TurnRequest {
	return agent.TurnRequest{
		DemandID: "dem-1", ThreadID: "thr-1",
		Text: "why did the build break?", TaskKind: "implementation",
	}
}

// ── tests ───────────────────────────────────────────────────────────────────

// Authorship is the reason the platform exists: telling what the human did from
// what the agent did. If this test falls, the event log — which is the demand's
// truth (ADR-0006) — starts lying about who did what.
func TestReplyAuthorshipBelongsToTheAgent(t *testing.T) {
	c := setup(t, agent.ContextPackage{}, concludingReply(), agent.AgentCard{}, agent.Accounting{})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-1"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(c.conv.messages) != 2 {
		t.Fatalf("expected 2 messages (question and reply), got %d", len(c.conv.messages))
	}

	question, reply := c.conv.messages[0], c.conv.messages[1]
	if question.actorKind != ctxutil.ActorUser || question.actorID != "usr-ana" {
		t.Fatalf("the QUESTION stopped being the human's: %+v", question)
	}
	if reply.actorKind != ctxutil.ActorAgent {
		t.Fatalf("the REPLY was recorded as %q: an agent's utterance recorded as a "+
			"human's makes the event log lie about who did what", reply.actorKind)
	}
	if reply.actorID != "thr-1" || reply.actorName != "main" {
		t.Fatalf("the agent's actor is the THREAD (dop.v1.ActorRef): got id=%q name=%q",
			reply.actorID, reply.actorName)
	}
	// And the finding too: it is durable, it goes to the project's memory, and
	// going out signed by the human would make the audit point at the wrong
	// person.
	if len(c.conv.findings) != 1 || c.conv.findings[0].actorKind != ctxutil.ActorAgent {
		t.Fatalf("the FINDING did not go out signed by the agent: %+v", c.conv.findings)
	}
}

// The writes ALL derive from the same key: it is what makes resending the
// request repeat zero effects.
//
// The consumption's carries the loop's ROUND at the end (`:usage:1`), and it is
// no detail: with tools a turn consumes once per round, and a single key would
// make the cost domain discard everything from the second onwards as a duplicate
// — the budget would start seeing a fraction of the real spend.
func TestIdempotencyKeysAreDerived(t *testing.T) {
	c := setup(t, agent.ContextPackage{Dropped: agent.ContextDropped{Rules: 1}},
		concludingReply(), agent.AgentCard{}, agent.Accounting{})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-42"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	expected := []string{"turn-42:msg-in", "turn-42:notice", "turn-42:msg-out"}
	for i, want := range expected {
		if c.conv.messages[i].key != want {
			t.Fatalf("message %d with key %q, expected %q", i, c.conv.messages[i].key, want)
		}
	}
	if c.cost.usageKeys[0] != "turn-42:usage:1" {
		t.Fatalf("consumption with key %q", c.cost.usageKeys[0])
	}
	if c.conv.findings[0].key != "turn-42:finding" {
		t.Fatalf("finding with key %q", c.conv.findings[0].key)
	}
}

func TestIdempotencyKeyIsMandatory(t *testing.T) {
	c := setup(t, agent.ContextPackage{}, concludingReply(), agent.AgentCard{}, agent.Accounting{})
	_, err := c.svc.RunTurn(callCtx(), request(), "  ")
	if err == nil {
		t.Fatal("a turn with no key should be refused: generating one here would turn a " +
			"network retry into double consumption")
	}
	if errs.KindOf(err) != errs.KindInvalid {
		t.Fatalf("error of kind %q, expected invalid argument", errs.KindOf(err))
	}
}

// Concluding REQUIRES a finding (spec §1): with no title and summary, the
// conclusion is refused and the thread stays active, with a warning.
func TestConclusionWithNoFindingIsRefused(t *testing.T) {
	r := concludingReply()
	r.Data = map[string]any{"reply": "I think I am done", "concluded": true}
	c := setup(t, agent.ContextPackage{}, r, agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.Concluded {
		t.Fatal("an empty conclusion was accepted: the thread would die in silence with a decorative `true`")
	}
	if len(c.conv.findings) != 0 {
		t.Fatal("published an empty finding")
	}
	if !hasWarning(out.Warnings, "REFUSED") {
		t.Fatalf("the refusal did not become a readable warning: %v", out.Warnings)
	}
}

// A blown budget PAUSES and does not kill: the turn that already ran is
// delivered whole.
func TestBlownBudgetPausesWithoutLosingTheTurn(t *testing.T) {
	accounting := agent.Accounting{BudgetExceeded: true, Exceeded: []agent.BudgetView{
		{Scope: "demand", ScopeID: "dem-1", LimitMicros: 1000, SpentMicros: 4200, Currency: "USD"},
	}}
	c := setup(t, agent.ContextPackage{}, concludingReply(), agent.AgentCard{}, accounting)

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatal("a blown budget became an ERROR: ADR-0011 §2 refused the hard cut")
	}
	if !out.Paused {
		t.Fatal("the overrun did not pause")
	}
	if out.Reply == "" || out.Finding == nil {
		t.Fatal("the already-paid turn was thrown away: the reply and the finding have to come out whole")
	}
	if !strings.Contains(out.Notice, "4200") || !strings.Contains(out.Notice, "dem-1") {
		t.Fatalf("the notice does not say what the attention box needs to show: %q", out.Notice)
	}
}

// The truncation shows up in the CONVERSATION, not only in the result.
func TestTruncationBecomesAMessageOnTheThread(t *testing.T) {
	pkg := agent.ContextPackage{Dropped: agent.ContextDropped{Rules: 2, Memories: 3}}
	c := setup(t, pkg, concludingReply(), agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !out.ContextTruncated {
		t.Fatal("the package came truncated and the result did not say so")
	}
	if len(c.conv.messages) != 3 || !strings.Contains(c.conv.messages[1].text, "truncated") {
		t.Fatalf("the truncation warning did not enter the thread: %+v", c.conv.messages)
	}
	// And the agent needs to know too, BEFORE asserting things about what it
	// did not read.
	if !strings.Contains(c.prov.askedTurn.StablePrefix, "TRUNCATED") {
		t.Fatal("the prefix did not warn the agent that the context came partial")
	}
}

// The CLASS arrives whole and becomes a name through the ACTIVE provider's
// catalog — it is the translation that retired the BFF's `catalog.py`
// (ADR-0023).
func TestClassBecomesANameThroughTheProviderCatalog(t *testing.T) {
	c := setup(t, agent.ContextPackage{}, concludingReply(), agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	// The router returned "claude-opus" (ITS catalog) with class `strong`; the
	// active provider calls the strong class something else.
	if c.prov.askedModel != "x-large" {
		t.Fatalf("sent %q to the provider, expected the name from its catalog (x-large)",
			c.prov.askedModel)
	}
	if out.Routing.Class != agent.ClassStrong || out.Routing.Reason == "" {
		t.Fatalf("the decision lost its class or its justification: %+v", out.Routing)
	}
}

// The thread's card beats the router, and its name passes through INTACT.
func TestThreadCardBeatsTheRouter(t *testing.T) {
	card := agent.AgentCard{Purpose: "forensics", Model: "frozen-thread-model", Effort: "max"}
	c := setup(t, agent.ContextPackage{}, concludingReply(), card, agent.Accounting{})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if c.prov.askedModel != "frozen-thread-model" {
		t.Fatalf("the card's model did not pass through intact: %q", c.prov.askedModel)
	}
	if c.prov.askedEffort != agent.EffortMax {
		t.Fatalf("the card's effort did not win: %q", c.prov.askedEffort)
	}
	if !out.Routing.FromAgentCard {
		t.Fatal("the result did not record that the card won")
	}
}

// An unknown price does NOT silently become zero.
func TestUnknownPriceComesOutAsAbsence(t *testing.T) {
	r := concludingReply()
	r.Model = "model-with-no-table"
	c := setup(t, agent.ContextPackage{}, r, agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.Usage.CostKnown {
		t.Fatal("claimed to know the price of a model outside the table")
	}
	if out.Usage.CostMicros != 0 || out.Usage.Currency != "" {
		t.Fatalf("invented a cost: %+v", out.Usage)
	}
	if !hasWarning(out.Warnings, "LACK of a table") {
		t.Fatalf("the zeroed cost came with no explanation: %v", out.Warnings)
	}
	// And the consumption in TOKENS was recorded anyway: a measurement that
	// vanishes when the price is missing is a measurement that vanishes exactly
	// when it matters.
	if c.cost.usages[0].InputTokens != 1000 {
		t.Fatalf("the consumption in tokens was not recorded: %+v", c.cost.usages[0])
	}
}

// The cost is INTEGER arithmetic, per 1,000 tokens, and it reaches the record.
func TestCostInIntegerMicros(t *testing.T) {
	c := setup(t, agent.ContextPackage{}, concludingReply(), agent.AgentCard{}, agent.Accounting{})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	// 1000*5000 + 200*25000 + 400*500 + 100*6250 = 10,825,000 → /1000 = 10,825
	const expected = agent.Micros(10_825)
	if out.Usage.CostMicros != expected {
		t.Fatalf("cost %d, expected %d", out.Usage.CostMicros, expected)
	}
	if c.cost.usages[0].CostMicros != expected || c.cost.usages[0].Currency != "USD" {
		t.Fatalf("the cost did not reach the record: %+v", c.cost.usages[0])
	}
}

// When the provider does not report cache creation, the zero comes out DECLARED
// as an absence — and not as an assertion that nothing was written (D1).
func TestUnknownCacheCreationComesOutDeclared(t *testing.T) {
	prov := &fakeProvider{info: agent.ProviderInfo{
		Name:         "no-accounting",
		Catalog:      map[agent.ModelClass]string{agent.ClassStrong: "y-large"},
		Capabilities: agent.Capabilities{},
	}, reply: concludingReply()}
	cost := &fakeCost{decision: agent.Decision{Class: agent.ClassStrong, Effort: agent.EffortHigh}}
	conv := &fakeConversation{thread: agent.Thread{ID: "thr-1", Key: "main"}}
	svc := agent.NewService(prov, fakeKnowledge{}, cost, conv)

	out, err := svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.Usage.CacheCreationKnown {
		t.Fatal("claimed to know cache creation on a provider that does not report it")
	}
	if !hasWarning(out.Warnings, "ABSENCE of information") {
		t.Fatalf("the zero came with no explanation: %v", out.Warnings)
	}
}

// The provider's unavailability goes up INTACT, with the Kind translated —
// never as an internal error of ours.
func TestProviderUnavailabilityDoesNotBecomeOurError(t *testing.T) {
	c := setup(t, agent.ContextPackage{}, nil, agent.AgentCard{}, agent.Accounting{})
	c.prov.failure = agent.Unavailability("provider-x", agent.ReasonRejectedCredential, "raw")

	_, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if k := errs.KindOf(err); k != errs.KindUnauthorized {
		t.Fatalf("Kind %q, expected unauthenticated — the classifier is not registered", k)
	}
	if strings.Contains(err.Error(), "raw") {
		t.Fatalf("the provider's raw detail leaked into the message: %v", err)
	}
	// The human's question is ALREADY on the thread: if the provider goes down,
	// the conversation shows what was asked instead of a hole.
	if len(c.conv.messages) != 1 {
		t.Fatalf("the question did not enter before the model call: %+v", c.conv.messages)
	}
}

func TestRunTurnRefusesAnIncompleteRequest(t *testing.T) {
	c := setup(t, agent.ContextPackage{}, concludingReply(), agent.AgentCard{}, agent.Accounting{})
	cases := map[string]agent.TurnRequest{
		"no_text":      {DemandID: "d", ThreadID: "t", TaskKind: "implementation"},
		"no_task_kind": {DemandID: "d", ThreadID: "t", Text: "hi"},
		"no_thread":    {DemandID: "d", Text: "hi", TaskKind: "implementation"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.svc.RunTurn(callCtx(), req, "turn-1"); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
	// And with no active account there is no turn: multi-tenant isolation is a
	// constraint.
	if _, err := c.svc.RunTurn(context.Background(), request(), "turn-1"); err == nil {
		t.Fatal("a turn with no active account should be refused")
	}
}

func hasWarning(warnings []string, fragment string) bool {
	for _, w := range warnings {
		if strings.Contains(w, fragment) {
			return true
		}
	}
	return false
}
