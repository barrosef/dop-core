package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/agent"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ════════════════════════════════════════════════════════════════════════════
// THE TOOL LOOP, tested with no real provider and no real executor.
//
// What these tests protect is expensive and silent: a loop with no cap is a bill
// with no cap; a measurement that only counts the last round makes the budget
// see a fraction of the spend; a tool failure delivered as a turn error takes
// from the model the one piece of information that solves its problem.
// ════════════════════════════════════════════════════════════════════════════

// fakeSandbox is the double for the executor's narrow port.
//
// It keeps EVERYTHING it received — including for the credential sweep, which is
// the only way to prove, from the outside, that nothing from the platform leaks
// into the sandbox.
type fakeSandbox struct {
	commands []agent.SandboxCommand
	demands  []string
	output   agent.SandboxOutput
	failure  error
	calls    int
	perCall  []agent.SandboxOutput
}

func (s *fakeSandbox) RunCommand(_ context.Context, demandID string,
	cmd agent.SandboxCommand) (agent.SandboxOutput, error) {

	s.calls++
	s.demands = append(s.demands, demandID)
	s.commands = append(s.commands, cmd)
	if s.failure != nil {
		return agent.SandboxOutput{}, s.failure
	}
	if n := len(s.perCall); n > 0 {
		i := s.calls - 1
		if i >= n {
			i = n - 1
		}
		return s.perCall[i], nil
	}
	return s.output, nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

func cardWithTool() agent.AgentCard {
	return agent.AgentCard{Purpose: "implement", Tools: []string{agent.ToolRunCommand}}
}

func replyAskingForTool(id, argument string) *agent.Reply {
	return &agent.Reply{
		Text:  "I am going to look at the repository",
		Model: "x-large",
		Usage: agent.Usage{InputTokens: 100, OutputTokens: 20},
		// The stop is `tool_use`, but what decides to continue is the PRESENCE
		// of the calls — see the loop: a tool stop with no call at all is an
		// incoherence of the provider's and must not freeze the turn.
		StopReason:    agent.StopToolUse,
		EffortApplied: agent.EffortHigh,
		ToolCalls: []agent.ToolCall{{
			ID: id, Name: agent.ToolRunCommand,
			Input: map[string]any{"command": []any{"sh", "-c", argument}},
		}},
	}
}

// setupWithTools assembles the scenario with the executor wired.
func setupWithTools(t *testing.T, sb agent.Sandbox, accounting agent.Accounting,
	replies []*agent.Reply, opts ...agent.Option) scenario {

	t.Helper()
	prov := &fakeProvider{info: providerSheet(), replies: replies, reply: replies[0]}
	cost := &fakeCost{
		decision: agent.Decision{TaskKind: "implementation", Class: agent.ClassStrong,
			Model: "claude-opus", Effort: agent.EffortHigh, Reason: "ADR-0008 §3"},
		accounting: accounting,
	}
	conv := &fakeConversation{thread: agent.Thread{
		ID: "thr-1", Key: "main", Card: cardWithTool(),
	}}
	all := append([]agent.Option{agent.WithSandbox(sb)}, opts...)
	return scenario{
		svc:  agent.NewService(prov, fakeKnowledge{}, cost, conv, all...),
		prov: prov, cost: cost, conv: conv,
	}
}

// ── the happy path ──────────────────────────────────────────────────────────

// The agent asks, the tool runs in the sandbox, the result comes back, the agent
// concludes. It is the whole bridge in one test.
func TestLoopExecutesTheToolAndContinues(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{
		ExitCode: 0, Stdout: "COMMAND-OUTPUT-MARKER\n",
	}}
	c := setupWithTools(t, sb, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "git status"),
		concludingReply(),
	})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-1")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.ToolRounds != 2 {
		t.Fatalf("expected 2 rounds, got %d", out.ToolRounds)
	}
	if out.LoopStop != agent.LoopFinished {
		t.Fatalf("stop %q, expected %q", out.LoopStop, agent.LoopFinished)
	}
	if sb.calls != 1 {
		t.Fatalf("the sandbox was called %d time(s), expected 1", sb.calls)
	}
	// The tool ran the command the MODEL asked for, and on the right demand.
	if got := sb.commands[0].Command; strings.Join(got, " ") != "sh -c git status" {
		t.Fatalf("the command reached the executor mangled: %v", got)
	}
	if sb.demands[0] != "dem-1" {
		t.Fatalf("the command went to demand %q", sb.demands[0])
	}

	// And the RESULT went back to the model on the next round — without that
	// the loop is just an extra call the agent never reads.
	if len(c.prov.turns) != 2 {
		t.Fatalf("the provider received %d turn(s)", len(c.prov.turns))
	}
	second := c.prov.turns[1]
	if !messageWithResult(second, "COMMAND-OUTPUT-MARKER") {
		t.Fatalf("the command's output did not go back to the model:\n%+v", second.Messages)
	}
	// The assistant's utterance with the call is resent alongside: without it,
	// the result is orphaned and both providers refuse with a 400 (D11).
	if !messageWithCall(second, "call-1") {
		t.Fatalf("the call was not resent in the history:\n%+v", second.Messages)
	}
	// It really concluded: a finding was published.
	if !out.Concluded || out.Finding == nil {
		t.Fatalf("the turn did not conclude after the tool: %+v", out)
	}
}

// Every round consumes, and the turn's total is the SUM. Recording only the last
// would underestimate the spend by a factor equal to the number of rounds
// (ADR-0008 §2).
func TestConsumptionSumsEveryRound(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	c := setupWithTools(t, sb, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "ls"),
		concludingReply(), // 1000 in, 200 out
	})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-9")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(c.cost.usages) != 2 {
		t.Fatalf("expected 2 consumption records (one per round), got %d", len(c.cost.usages))
	}
	// DERIVED keys, distinct per round: a single key would make the cost domain
	// discard the second as a duplicate, and the budget would see half.
	if c.cost.usageKeys[0] != "turn-9:usage:1" || c.cost.usageKeys[1] != "turn-9:usage:2" {
		t.Fatalf("consumption keys: %v", c.cost.usageKeys)
	}
	want := int64(100 + 1000)
	if out.Usage.InputTokens != want {
		t.Fatalf("FICTIONAL BUDGET: summed input came to %d, expected %d — recording only "+
			"the last round hides the other rounds' spend", out.Usage.InputTokens, want)
	}
	if out.Usage.OutputTokens != 20+200 {
		t.Fatalf("summed output came to %d", out.Usage.OutputTokens)
	}
	// And the cost is summed too, with each round's model price.
	if !out.Usage.CostKnown || out.Usage.CostMicros <= 0 {
		t.Fatalf("summed cost came out %+v", out.Usage)
	}
}

// ── the cap ─────────────────────────────────────────────────────────────────

// A loop with no cap is a bill with no cap. "I hit the cap" needs to be
// distinguishable from "I finished" — and an interrupted loop does NOT conclude
// a thread.
func TestRoundCapStopsTheLoopReadably(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	// A stubborn agent: asks for a tool forever, and marks a conclusion too.
	insistent := replyAskingForTool("call-x", "npm test")
	insistent.Data = map[string]any{
		"reply": "almost there", "concluded": true,
		"finding_title": "a title", "finding_summary": "a summary",
	}
	c := setupWithTools(t, sb, agent.Accounting{},
		[]*agent.Reply{insistent}, agent.WithMaxToolRounds(3))

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-cap")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.ToolRounds != 3 {
		t.Fatalf("LOOP WITH NO CAP: it took %d rounds against a cap of 3", out.ToolRounds)
	}
	if out.LoopStop != agent.LoopMaxRounds {
		t.Fatalf("stop %q, expected %q — 'I hit the cap' is different from 'I finished'",
			out.LoopStop, agent.LoopMaxRounds)
	}
	if out.MaxToolRounds != 3 {
		t.Fatalf("the cap that applied did not travel in the result: %d", out.MaxToolRounds)
	}
	// The last round asked for a tool and it did NOT run: the cap stops before.
	if sb.calls != 2 {
		t.Fatalf("the sandbox ran %d time(s); with a cap of 3, the last round does not execute", sb.calls)
	}
	if !someWarningContains(out.Warnings, "cap") {
		t.Fatalf("hitting the cap produced no readable warning: %v", out.Warnings)
	}
	// The thread has to SAY the work stopped midway.
	if !someMessageContains(c.conv.messages, "cap of 3 tool round") {
		t.Fatal("the thread did not get the stop warning: whoever reads the conversation " +
			"would see interrupted work with the face of a conclusion")
	}
	// And the conclusion the model marked is REFUSED: a finding written
	// mid-work is durable and starts counting as truth.
	if out.Concluded || out.Finding != nil || len(c.conv.findings) != 0 {
		t.Fatal("the loop stopped at the cap and the turn CONCLUDED anyway: the finding " +
			"would enter the project's memory as if the work had finished")
	}
}

// The caller may LOWER the cap, never raise it: a cap the client raises is not a
// cap, it is a suggestion.
func TestCallerCapOnlyLowers(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	c := setupWithTools(t, sb, agent.Accounting{},
		[]*agent.Reply{replyAskingForTool("c", "ls")}, agent.WithMaxToolRounds(4))

	req := request()
	req.MaxToolRounds = 99 // an attempt to raise it
	high, err := c.svc.RunTurn(callCtx(), req, "turn-a")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if high.MaxToolRounds != 4 {
		t.Fatalf("the caller RAISED the cap to %d: a cap the client raises is not a "+
			"cap (ADR-0008 §2)", high.MaxToolRounds)
	}

	c2 := setupWithTools(t, &fakeSandbox{}, agent.Accounting{},
		[]*agent.Reply{replyAskingForTool("c", "ls")}, agent.WithMaxToolRounds(4))
	req.MaxToolRounds = 2
	low, err := c2.svc.RunTurn(callCtx(), req, "turn-b")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if low.MaxToolRounds != 2 || low.ToolRounds != 2 {
		t.Fatalf("the caller could not LOWER the cap: %+v", low)
	}
}

// ── budget ──────────────────────────────────────────────────────────────────

// A budget blown mid-loop STOPS the loop; it does not kill the turn. What
// already ran is delivered (ADR-0008 §2).
func TestBlownBudgetStopsTheLoopWithoutKillingTheTurn(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	blown := agent.Accounting{
		BudgetExceeded: true,
		Exceeded: []agent.BudgetView{{
			Scope: "demand", ScopeID: "dem-1", LimitMicros: 10, SpentMicros: 99, Currency: "USD",
		}},
	}
	c := setupWithTools(t, sb, blown, []*agent.Reply{
		replyAskingForTool("call-1", "npm test"),
		concludingReply(),
	})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-budget")
	if err != nil {
		t.Fatalf("a blown budget KILLED the turn: %v — ADR-0008 §2 refused the hard cut", err)
	}
	if out.LoopStop != agent.LoopBudget {
		t.Fatalf("stop %q, expected %q", out.LoopStop, agent.LoopBudget)
	}
	if out.ToolRounds != 1 {
		t.Fatalf("the loop took %d rounds after the overrun", out.ToolRounds)
	}
	if sb.calls != 0 {
		t.Fatal("the tool ran AFTER the budget blew: the loop stops before the next " +
			"round, and the next round includes executing what was asked for")
	}
	if !out.Paused || out.Notice == "" {
		t.Fatalf("the turn did not come out paused with a notice: %+v", out)
	}
	// What ALREADY ran is delivered: the model's reply is on the thread.
	if out.Reply == "" || len(out.MessageIDs) < 2 {
		t.Fatalf("the interrupted turn did not deliver what it already had: %+v", out)
	}
	// And that round's consumption stayed recorded: paid tokens do not vanish.
	if len(c.cost.usages) != 1 {
		t.Fatalf("consumption recorded %d time(s)", len(c.cost.usages))
	}
}

// ── tool failure × infrastructure failure ───────────────────────────────────

// A command exiting with a code != 0 is a RESULT: the model needs to see the
// failure in order to fix it. The turn does not die.
func TestToolFailureIsAResultAndNotATurnError(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{
		ExitCode: 2, Stdout: "", Stderr: "COMMAND-FAILURE: the test failed",
	}}
	c := setupWithTools(t, sb, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "npm test"),
		concludingReply(),
	})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-failure")
	if err != nil {
		t.Fatalf("A TOOL FAILURE KILLED THE TURN: %v — the model needs to SEE that the "+
			"command failed in order to fix it", err)
	}
	if out.LoopStop != agent.LoopFinished {
		t.Fatalf("stop %q", out.LoopStop)
	}
	res := resultInHistory(c.prov.turns[1], "call-1")
	if res == nil {
		t.Fatal("the result did not reach the model")
	}
	if !res.IsError {
		t.Fatal("the result of a command that exited with code 2 was not marked as an ERROR: " +
			"the model would read the failure as normal output")
	}
	if !strings.Contains(res.Content, "COMMAND-FAILURE") || !strings.Contains(res.Content, "exit_code: 2") {
		t.Fatalf("the result does not tell the model what happened:\n%s", res.Content)
	}
}

// A sandbox that went down is a different thing: it goes up and kills the turn.
// No amount of tokens spent by the model fixes a cluster that is down.
func TestInfrastructureFailureGoesUpAndKillsTheTurn(t *testing.T) {
	sb := &fakeSandbox{failure: errs.New(errs.KindUnavailable, "the cluster did not answer")}
	c := setupWithTools(t, sb, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "ls"),
		concludingReply(),
	})

	_, err := c.svc.RunTurn(callCtx(), request(), "turn-infra")
	if err == nil {
		t.Fatal("a executor that is down became a tool result: insisting with the model " +
			"against a wall is burning money")
	}
	if errs.KindOf(err) != errs.KindUnavailable {
		t.Fatalf("error of kind %q, expected unavailable", errs.KindOf(err))
	}
}

// A refusal ABOUT THE CALL (wrong name, permission) is something the model
// fixes: it becomes an error result, not a turn error.
func TestRefusalAboutTheCallBecomesAResult(t *testing.T) {
	sb := &fakeSandbox{failure: errs.Invalid("no command provided")}
	c := setupWithTools(t, sb, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "ls"),
		concludingReply(),
	})

	out, err := c.svc.RunTurn(callCtx(), request(), "turn-refusal")
	if err != nil {
		t.Fatalf("a refusal about the call killed the turn: %v", err)
	}
	res := resultInHistory(c.prov.turns[1], "call-1")
	if res == nil || !res.IsError {
		t.Fatalf("the refusal did not come back as an error result: %+v", res)
	}
	if out.LoopStop != agent.LoopFinished {
		t.Fatalf("stop %q", out.LoopStop)
	}
}

// ── undeclared tool and unreadable argument (D8, D11) ───────────────────────

func TestUngrantedToolBecomesAnErrorResult(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0}}
	asking := replyAskingForTool("call-1", "ls")
	asking.ToolCalls[0].Name = "drop_the_database"
	c := setupWithTools(t, sb, agent.Accounting{},
		[]*agent.Reply{asking, concludingReply()})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-ut"); err != nil {
		t.Fatalf("an unknown tool killed the turn: %v", err)
	}
	if sb.calls != 0 {
		t.Fatal("THE EXECUTOR EXECUTED A TOOL THAT WAS NOT GRANTED: neither provider " +
			"stops the model from calling what does not exist — the loop is what stops it")
	}
	res := resultInHistory(c.prov.turns[1], "call-1")
	if res == nil || !res.IsError {
		t.Fatalf("the refusal did not go back to the model: %+v", res)
	}
	// The list of valid names travels along: a refusal with no alternative
	// makes the model try the same name again, and each attempt is a paid round.
	if !strings.Contains(res.Content, agent.ToolRunCommand) {
		t.Fatalf("the refusal does not say what exists:\n%s", res.Content)
	}
}

func TestUnreadableArgumentBecomesAnErrorResultWithTheRawText(t *testing.T) {
	sb := &fakeSandbox{}
	asking := replyAskingForTool("call-1", "ls")
	// It is what the adapter delivers when the provider sends something that
	// does not decode (D8): a nil Input, RawInput with what came.
	asking.ToolCalls[0].Input = nil
	asking.ToolCalls[0].RawInput = `{"command": ["sh", "-c", "npm te`
	c := setupWithTools(t, sb, agent.Accounting{},
		[]*agent.Reply{asking, concludingReply()})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-arg"); err != nil {
		t.Fatalf("an unreadable argument killed the turn: %v", err)
	}
	if sb.calls != 0 {
		t.Fatal("the loop executed a call whose arguments did not decode")
	}
	res := resultInHistory(c.prov.turns[1], "call-1")
	if res == nil || !res.IsError {
		t.Fatalf("the refusal did not go back to the model: %+v", res)
	}
	if !strings.Contains(res.Content, "npm te") {
		t.Fatalf("the RAW text did not go back to the model: 'your argument is invalid' "+
			"without saying which argument fixes nothing:\n%s", res.Content)
	}
}

// An argument valid as JSON but wrong against the schema is also the model's
// business.
func TestMissingCommandBecomesAnErrorResult(t *testing.T) {
	sb := &fakeSandbox{}
	asking := replyAskingForTool("call-1", "ls")
	asking.ToolCalls[0].Input = map[string]any{"cmd": "git status"} // the wrong field
	c := setupWithTools(t, sb, agent.Accounting{},
		[]*agent.Reply{asking, concludingReply()})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-cmd"); err != nil {
		t.Fatalf("an argument outside the schema killed the turn: %v", err)
	}
	if sb.calls != 0 {
		t.Fatal("the loop executed a call with no command")
	}
	res := resultInHistory(c.prov.turns[1], "call-1")
	if res == nil || !strings.Contains(res.Content, "command") {
		t.Fatalf("the refusal does not say which field is missing: %+v", res)
	}
}

// ── the most expensive guarantee: nothing from the platform enters the sandbox

// The sandbox runs agent code, which reads untrusted content (execution spec
// §6). The model provider's credential lives in the vault and is used in the
// same process (ADR-0016) — and it must not leak into the sandbox through any
// crack in the loop.
//
// The structural proof is the port: `SandboxCommand` has no environment field
// and no credential field (nor does `ports.ExecRequest`). This test is the
// BEHAVIOURAL proof: nothing that crosses the loop — not the prompt's prefix,
// which contains the thread's card, nor the user's text — reaches the executor.
func TestNothingCrossingTheLoopCarriesACredential(t *testing.T) {
	const key = "sk-CREDENTIAL-SENTINEL-MUST-NOT-REACH-THE-SANDBOX"

	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	// A hostile model: it tries to drag the key inside along three paths — in
	// the command, in an extra argument field, and in the call's id.
	asking := replyAskingForTool("call-"+key, "echo hi")
	asking.ToolCalls[0].Input["credential"] = key
	asking.Text = "I am going to use " + key

	c := setupWithTools(t, sb, agent.Accounting{},
		[]*agent.Reply{asking, concludingReply()})
	// And the provider carries the key in its own data sheet, which is the
	// worst case: a sheet leaking into the command would hand over the key of
	// every account.
	c.prov.info.Prices = map[string]agent.Price{key: {Currency: "USD"}}

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-cred"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if sb.calls != 1 {
		t.Fatalf("the sandbox was called %d time(s)", sb.calls)
	}
	// The sweep is over the WHOLE command, serialized: it is the only way not
	// to depend on remembering which fields exist today.
	raw, err := json.Marshal(sb.commands[0])
	if err != nil {
		t.Fatalf("serializing the command: %v", err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("THE CREDENTIAL CROSSED THE LOOP AND REACHED THE EXECUTOR: %s", raw)
	}
	// And the demand must not carry anything beyond the id either.
	if strings.Contains(sb.demands[0], key) {
		t.Fatal("the credential ended up in the demand's identifier")
	}
}

// ── an installation with no executor ───────────────────────────────────────

// A card granting a tool in an installation with no executor: the turn runs,
// the tools are NOT declared, and the warning says why. The three worse
// alternatives are declaring (the agent plans on top of what does not exist),
// staying silent (the card looks honoured) and refusing (one line of a card
// stopping the whole job).
func TestCardWithToolAndNoExecutorWarnsAndDoesNotDeclare(t *testing.T) {
	prov := &fakeProvider{info: providerSheet(), reply: concludingReply()}
	cost := &fakeCost{decision: agent.Decision{TaskKind: "implementation", Class: agent.ClassStrong}}
	conv := &fakeConversation{thread: agent.Thread{ID: "thr-1", Key: "main", Card: cardWithTool()}}
	svc := agent.NewService(prov, fakeKnowledge{}, cost, conv) // NO WithSandbox

	out, err := svc.RunTurn(callCtx(), request(), "turn-no-sb")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(prov.askedTurn.Tools) != 0 {
		t.Fatalf("the tools were DECLARED with no executor to execute them: %+v",
			prov.askedTurn.Tools)
	}
	if !someWarningContains(out.Warnings, "executor") {
		t.Fatalf("the card promised action, nothing was wired, and nobody warned: %v", out.Warnings)
	}
}

// A model asking for a tool in an installation with no executor: the loop stops
// and SAYS so.
func TestToolRequestWithNoExecutorStopsTheLoopWithAWarning(t *testing.T) {
	prov := &fakeProvider{
		info: providerSheet(), reply: replyAskingForTool("call-1", "ls"),
	}
	cost := &fakeCost{decision: agent.Decision{TaskKind: "implementation", Class: agent.ClassStrong}}
	conv := &fakeConversation{thread: agent.Thread{ID: "thr-1", Key: "main"}}
	svc := agent.NewService(prov, fakeKnowledge{}, cost, conv)

	out, err := svc.RunTurn(callCtx(), request(), "turn-no-sb2")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if out.LoopStop != agent.LoopNoSandbox {
		t.Fatalf("stop %q, expected %q", out.LoopStop, agent.LoopNoSandbox)
	}
	if out.ToolRounds != 1 {
		t.Fatalf("the loop took %d rounds with nothing to execute", out.ToolRounds)
	}
}

// ── the catalog ─────────────────────────────────────────────────────────────

func TestToolCatalog(t *testing.T) {
	t.Run("no_grant_means_no_tool", func(t *testing.T) {
		specs, unknown := agent.ToolCatalog(nil)
		if len(specs) != 0 || len(unknown) != 0 {
			t.Fatalf("a card with no tool declared %d: an agent with no grant does not act", len(specs))
		}
	})

	t.Run("a_nonexistent_name_comes_back_separately", func(t *testing.T) {
		specs, unknown := agent.ToolCatalog(
			[]string{"drop_the_database", agent.ToolRunCommand, "  "})
		if len(specs) != 1 || specs[0].Name != agent.ToolRunCommand {
			t.Fatalf("the valid grants did not come out: %+v", specs)
		}
		if len(unknown) != 1 || unknown[0] != "drop_the_database" {
			t.Fatalf("the nonexistent name did not come back separately: %v — silencing it "+
				"would make the agent look capable of something nobody installed", unknown)
		}
	})

	t.Run("alphabetical_order_and_no_repetition", func(t *testing.T) {
		// The order is the catalog's, not the card's: the order in which
		// somebody typed two tools is nobody's choice, but it would change the
		// prefix's bytes and the cache entry with it (ADR-0008 §1).
		a, _ := agent.ToolCatalog([]string{agent.ToolRunCommand, agent.ToolRunCommand})
		if len(a) != 1 {
			t.Fatalf("a repeated name became two declarations: %+v", a)
		}
	})

	t.Run("the_schema_is_not_shared", func(t *testing.T) {
		// Two calls have to return DIFFERENT maps: a shared map would let the
		// first adapter that altered it change the contract of every turn of
		// every account.
		a, _ := agent.ToolCatalog([]string{agent.ToolRunCommand})
		b, _ := agent.ToolCatalog([]string{agent.ToolRunCommand})
		a[0].InputSchema["poisoned"] = true
		if _, ok := b[0].InputSchema["poisoned"]; ok {
			t.Fatal("the tool's schema is SHARED between calls")
		}
	})
}

// ── reading helpers ─────────────────────────────────────────────────────────

func messageWithResult(t agent.Turn, fragment string) bool {
	for _, m := range t.Messages {
		for _, r := range m.ToolResults {
			if strings.Contains(r.Content, fragment) {
				return true
			}
		}
	}
	return false
}

func messageWithCall(t agent.Turn, id string) bool {
	for _, m := range t.Messages {
		if m.Role != agent.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == id {
				return true
			}
		}
	}
	return false
}

func resultInHistory(t agent.Turn, callID string) *agent.ToolResult {
	for _, m := range t.Messages {
		for i, r := range m.ToolResults {
			if r.CallID == callID {
				return &m.ToolResults[i]
			}
		}
	}
	return nil
}

func someWarningContains(warnings []string, fragment string) bool {
	for _, w := range warnings {
		if strings.Contains(w, fragment) {
			return true
		}
	}
	return false
}

func someMessageContains(msgs []recordedMessage, fragment string) bool {
	for _, m := range msgs {
		if strings.Contains(m.text, fragment) {
			return true
		}
	}
	return false
}

// Only "I finished" admits concluding a thread.
//
// The rule lives here, in its pure form, because in the turn's cycle it is the
// SECOND lock on a door `executeTurn` already locked (see service.go): every
// stop that is not `finished` happens with a pending tool call, and a pending
// call already zeroes `concluded`. The redundancy is deliberate — the two rules
// are about different things — and testing it here is what stops it from rotting
// without anyone noticing.
func TestOnlyTheLoopsEndAdmitsAConclusion(t *testing.T) {
	if !agent.LoopFinished.Concluded() {
		t.Fatal("the loop finished on its own and the conclusion was refused")
	}
	for _, stop := range []agent.LoopStop{
		agent.LoopMaxRounds, agent.LoopBudget, agent.LoopNoSandbox,
	} {
		if stop.Concluded() {
			t.Fatalf("stop %q admitted a conclusion: a finding written mid-work is durable "+
				"and starts counting as truth in the project's memory", stop)
		}
	}
}

// ════════════════════════════════════════════════════════════════════════════
// THE FIVE PROBES — what the suite did NOT catch after every expected break had
// failed as it should.
//
// Each test below was born from a deliberate break that PASSED CLEAN. It is the
// same method that discovered, in the previous delivery, that erasing
// Anthropic's cache breakpoint failed nothing and multiplied the bill by ten:
// breaking the obvious proves the suite works; breaking the non-obvious is what
// discovers what it does not see. None of the five fails with an error — all
// five fail with an invoice, with a wrong conclusion, or with a command nobody
// asked for.
// ════════════════════════════════════════════════════════════════════════════

// PROBE 1 — the most expensive one. The stable prefix has to be BYTE FOR BYTE
// the same on every round of the loop.
//
// The break that passed clean: adding one byte to the prefix on every round.
// Nothing goes wrong. The loop keeps working, the agent keeps answering, the
// tests stay green — and every round starts paying for the WHOLE prefix as new
// input, at 10× the price of a cache read (ADR-0008 §1). In an eight-round turn
// with a large context package, it is the difference between cents and dollars
// per turn, multiplied by every thread of every account.
func TestStablePrefixDoesNotChangeBetweenRounds(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0, Stdout: "ok"}}
	c := setupWithTools(t, sb, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "ls"),
		replyAskingForTool("call-2", "cat go.mod"),
		concludingReply(),
	})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-cache"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(c.prov.turns) != 3 {
		t.Fatalf("expected 3 rounds, got %d", len(c.prov.turns))
	}
	base := c.prov.turns[0]
	for i, turn := range c.prov.turns[1:] {
		if turn.StablePrefix != base.StablePrefix {
			t.Fatalf("THE STABLE PREFIX CHANGED ON ROUND %d: nothing fails because of it, "+
				"and every round starts paying for the whole prefix as new input (~10× a "+
				"cache read, ADR-0008 §1). It is the defect only the invoice reports.\n"+
				"round 1: %q\nround %d: %q",
				i+2, base.StablePrefix, i+2, turn.StablePrefix)
		}
		if turn.Fingerprint() != base.Fingerprint() {
			t.Fatalf("the prefix's fingerprint changed on round %d", i+2)
		}
		// The tool DECLARATION is prefix in both providers too (guarantee 15).
		// A list that changes content or order between rounds invalidates the
		// cache through the same door.
		if len(turn.Tools) != len(base.Tools) {
			t.Fatalf("the tool declaration changed on round %d: %+v", i+2, turn.Tools)
		}
		for j := range turn.Tools {
			if turn.Tools[j].Name != base.Tools[j].Name {
				t.Fatalf("the tools' ORDER changed on round %d: %+v", i+2, turn.Tools)
			}
		}
	}
}

// PROBE 2 — a cut output that does not announce itself.
//
// The break that passed clean: erasing the cut warning from the result that goes
// back to the model. The command keeps running, the result keeps arriving, the
// cap keeps being respected — and the agent concludes from half a log thinking
// it read the whole log. It is the same class as the context truncation
// (ADR-0008), which the runtime already announces in two places: the conclusion
// comes out wrong and nobody can explain why afterwards.
func TestCutOutputIsAnnouncedToTheModel(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{
		ExitCode: 0, Stdout: "the log's first lines", Truncated: true,
	}}
	c := setupWithTools(t, sb, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "cat /var/log/huge"),
		concludingReply(),
	})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-cut"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	res := resultInHistory(c.prov.turns[1], "call-1")
	if res == nil {
		t.Fatal("the result did not reach the model")
	}
	if !strings.Contains(strings.ToUpper(res.Content), "CUT") {
		t.Fatalf("THE OUTPUT WAS CUT AND THE MODEL WAS NOT WARNED: it is going to conclude "+
			"from half the log thinking it read everything, and the wrong conclusion will "+
			"have no explanation afterwards.\nresult:\n%s", res.Content)
	}
}

// PROBE 3 — the deadline the MODEL asks for must not be unlimited.
//
// The break that passed clean: removing the `timeout_seconds` cap. The schema
// accepts the field, the model may write 86400, and a single hung command starts
// holding the turn for a day. It is not an error: it is a thread that never
// answers and a sandbox that never suspends for idleness because the command is
// still "alive".
func TestDeadlineAskedForByTheModelIsCapped(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0}}
	asking := replyAskingForTool("call-1", "sleep infinity")
	asking.ToolCalls[0].Input["timeout_seconds"] = float64(86400) // a day
	c := setupWithTools(t, sb, agent.Accounting{},
		[]*agent.Reply{asking, concludingReply()})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-deadline"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got := sb.commands[0].TimeoutSeconds; got > agent.MaxToolTimeoutSeconds {
		t.Fatalf("THE MODEL CHOSE A DEADLINE OF %ds, ABOVE THE CAP OF %ds: a hung command "+
			"starts holding the turn indefinitely, and the thread never answers",
			got, agent.MaxToolTimeoutSeconds)
	}
	// And the default still applies when it asks for nothing.
	sb2 := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0}}
	c2 := setupWithTools(t, sb2, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "ls"), concludingReply(),
	})
	if _, err := c2.svc.RunTurn(callCtx(), request(), "turn-deadline2"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if sb2.commands[0].TimeoutSeconds != agent.DefaultToolTimeoutSeconds {
		t.Fatalf("with no request from the model, the deadline came out as %ds", sb2.commands[0].TimeoutSeconds)
	}
}

// PROBE 4 — the results' order is the calls' order (D10).
//
// The break that passed clean: reversing the results before returning them. Both
// providers accept it — the link is by id, not by position — and nothing fails.
// What changes is the model's READING: "I ran the test and then read the log"
// becomes "I read the log and then ran the test", and it starts reasoning about a
// sequence of events that did not happen.
func TestResultsComeBackInTheCallsOrder(t *testing.T) {
	sb := &fakeSandbox{perCall: []agent.SandboxOutput{
		{ExitCode: 0, Stdout: "FIRST-OUTPUT"},
		{ExitCode: 0, Stdout: "SECOND-OUTPUT"},
	}}
	// Two calls in the SAME round — it is the parallelism both providers do by
	// default (D10).
	asking := replyAskingForTool("call-1", "first")
	asking.ToolCalls = append(asking.ToolCalls, agent.ToolCall{
		ID: "call-2", Name: agent.ToolRunCommand,
		Input: map[string]any{"command": []any{"sh", "-c", "second"}},
	})
	c := setupWithTools(t, sb, agent.Accounting{},
		[]*agent.Reply{asking, concludingReply()})

	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-order"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	var results []agent.ToolResult
	for _, m := range c.prov.turns[1].Messages {
		if m.Role == agent.RoleToolResult {
			results = append(results, m.ToolResults...)
		}
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].CallID != "call-1" || results[1].CallID != "call-2" {
		t.Fatalf("THE RESULTS CAME BACK OUT OF THE CALLS' ORDER (%q, %q): the providers "+
			"accept it, because the link is by id — the one who reads it wrong is the "+
			"MODEL, which starts reasoning about a sequence of events that did not "+
			"happen (D10)",
			results[0].CallID, results[1].CallID)
	}
	if !strings.Contains(results[0].Content, "FIRST-OUTPUT") {
		t.Fatalf("the first call's result is not the first execution's:\n%s",
			results[0].Content)
	}
}

// PROBE 5 — the DOMAIN's output cap has to reach the executor.
//
// The break that passed clean: sending zero in `MaxOutputBytes`. The port treats
// zero as "use my default", so nothing fails and nothing blows up — what changes
// is that the cap becomes the PORT's (64 KiB) instead of the domain's (32 KiB),
// and every tool result enters the next turn's context at twice the size. It is
// cost policy (ADR-0008) decided by omission, in the wrong layer.
func TestDomainOutputCapReachesTheExecutor(t *testing.T) {
	sb := &fakeSandbox{output: agent.SandboxOutput{ExitCode: 0}}
	c := setupWithTools(t, sb, agent.Accounting{}, []*agent.Reply{
		replyAskingForTool("call-1", "ls"), concludingReply(),
	})
	if _, err := c.svc.RunTurn(callCtx(), request(), "turn-output-cap"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got := sb.commands[0].MaxOutputBytes; got != agent.DefaultToolOutputBytes {
		t.Fatalf("the output cap reached the executor as %d, expected %d: zero makes the "+
			"port use ITS default, and the cost policy starts being decided by omission in "+
			"the wrong layer (ADR-0008)", got, agent.DefaultToolOutputBytes)
	}
}
