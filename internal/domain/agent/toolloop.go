package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ════════════════════════════════════════════════════════════════════════════
// THE TURN'S TOOL LOOP — the bridge between the agent and the sandbox.
//
// This domain's division of labour now has THREE pieces, and each one's frontier
// is what it may touch:
//
//   - turn.go     — interprets ONE model response. It touches no neighbour;
//   - toolloop.go — the loop: send, execute the tool, measure, decide whether to
//     continue. It touches TWO narrow ports (Routing, to measure, and Sandbox,
//     to act) and no other;
//   - service.go  — the complete cycle: context, thread, message, finding.
//
// The loop lives in a file of its own because it is the piece with the package's
// most expensive rule — the one deciding how much money a turn may spend — and
// that rule must not be diluted in the middle of type conversion.
//
// ── THE LOOP'S FIVE RULES ───────────────────────────────────────────────────
//
// 1. AN EXPLICIT ROUND CAP. A tool loop with no cap is a token bill with no cap,
//    and ADR-0011 exists to prevent that. A round's cost is not constant: each
//    round RESENDS the whole conversation plus the accumulated results, so round
//    N costs more than N-1. An agent in a cycle — run the test, read the error,
//    "fix" it, run again, same error — spends a lot and converges on nothing,
//    and it does not know it is in a cycle.
//
// 2. THE STOP IS READABLE. "I finished" and "I hit the cap" are different facts
//    and come out differently (see LoopStop): the first is an answer, the second
//    is a decision for a human. A loop returning both as success makes the
//    cockpit show incomplete work with the face of finished work.
//
// 3. EVERY ROUND IS MEASURED. `RecordUsage` happens EVERY round, not at the end.
//    Two reasons: the turn's total is the sum of the rounds (recording only the
//    last would underestimate the spend by a factor equal to the number of
//    rounds), and it is the record that returns the budget's state — without
//    measuring every round, the ceiling would only be consulted once the money
//    had already been spent.
//
// 4. A BLOWN BUDGET STOPS THE LOOP, IT DOES NOT KILL THE TURN (ADR-0011 §2).
//    What already ran is delivered whole: the reply goes to the thread, the
//    finding is published, the consumption is recorded. What does not happen is
//    the NEXT round.
//
// 5. A TOOL FAILURE IS A RESULT; AN INFRASTRUCTURE FAILURE GOES UP. The model
//    needs to see that the command exited with code 1 in order to fix it —
//    delivering that as a turn error would take from it the one piece of
//    information that solves the problem. A sandbox that went down, by contrast,
//    is not something the model can fix: spending more tokens asking it to try
//    again is burning money against a wall.
// ════════════════════════════════════════════════════════════════════════════

// LoopStop is WHY the loop stopped. A closed vocabulary: whoever reads the
// result decides from here, and an open vocabulary would become string
// comparison scattered across three consumers.
type LoopStop string

const (
	// LoopFinished: the model finished speaking. It is the only stop that means
	// "the reply below is complete".
	LoopFinished LoopStop = "finished"
	// LoopMaxRounds: it hit the round cap. The work stopped midway.
	LoopMaxRounds LoopStop = "max_rounds"
	// LoopBudget: the budget blew between one round and the next (ADR-0011 §2).
	LoopBudget LoopStop = "budget_exceeded"
	// LoopNoSandbox: the model asked for a tool and there is no executor
	// wired.
	LoopNoSandbox LoopStop = "sandbox_unavailable"
)

// Concluded says whether the stop admits concluding the thread.
//
// Only `LoopFinished`. An agent that hit the cap in the middle of a loop may
// have marked `concluded` in its last utterance — and accepting that would
// publish a finding written before the work finished, which is worse than having
// no finding at all: it enters the project's memory and its siblings' context as
// if it were true.
func (s LoopStop) Concluded() bool { return s == LoopFinished }

// DefaultMaxToolRounds is the tool-round cap per turn.
//
// EIGHT, and the choice has arithmetic behind it. The loop's cost is
// superlinear: round N resends the prefix (cached, ~0.1×) plus ALL the
// conversation accumulated so far (new input, 1×), so the spend grows with the
// square of the number of rounds in the volatile part. With eight rounds, a
// turn's worst case is on the order of eight model calls — auditable, and a
// number that fits in the head of whoever reads the invoice.
//
// Why not fewer: real agent work with tools spends two to five rounds in the
// common case (look, act, verify, fix, verify again). A cap of three would cut
// the NORMAL case, and a cap that cuts the normal case is a cap someone raises
// until it becomes decoration.
//
// Why not more: above that, the pattern that shows up is not convergence, it is
// a cycle — and a cycle does not improve with more rounds, it only gets more
// expensive. The cap is not for the competent agent; it is for the agent that
// did not notice it got stuck.
const DefaultMaxToolRounds = 8

// loopResult is what the loop produced, in domain facts.
type loopResult struct {
	// exec is the interpretation of the model's LAST response.
	exec *TurnExecution
	// totalUsage is the sum of the rounds — what the turn actually consumed.
	totalUsage Usage
	// totalCost is the sum of each round's cost, computed with the price of the
	// model that SERVED each one.
	totalCost Micros
	// priceKnown is false when SOME round ran on a model with no price table.
	// False brings the whole total down on purpose: a cost summed with a part
	// missing is more dangerous than an absent cost, because it looks complete
	// (ADR-0011 §2).
	priceKnown bool
	currency   string
	rounds     int
	calls      int
	stop       LoopStop
	accounting Accounting
	warnings   []string
}

// loop is the loop's luggage: the ports it may touch and the limits that apply.
type loop struct {
	provider  AgentProvider
	sandbox   Sandbox
	routing   Routing
	info      ProviderInfo
	demandID  string
	threadID  string
	model     string
	effort    Effort
	maxRounds int
	// usageKey derives the idempotency key of EACH round's measurement.
	usageKey func(round int) string
	// allowed are the tools declared on this turn. Empty = a one-round loop,
	// which is exactly the behaviour from before tools existed.
	allowed []ToolSpec
}

// run executes the loop until the model finishes, the cap is hit or the budget
// blows.
func (l loop) run(ctx context.Context, turn Turn) (*loopResult, error) {
	res := &loopResult{priceKnown: true, stop: LoopFinished}

	for {
		res.rounds++

		exec, err := executeTurn(ctx, l.provider, turn, l.model, l.effort)
		if err != nil {
			// A PROVIDER failure on round 1 is the turn's failure. On round N
			// consumption has already been recorded, and it stays recorded —
			// the error goes up anyway, because half an answer from a loop that
			// broke midway is worse than the honest failure.
			return nil, err
		}
		res.exec = exec
		res.warnings = appendWarnings(res.warnings, exec.Warnings...)

		// ── rule 3: EVERY round is measured ─────────────────────────────────
		accounting, err := l.measure(ctx, res, exec.ModelReply)
		if err != nil {
			return nil, err
		}
		res.accounting = accounting

		calls := exec.ModelReply.ToolCalls
		if len(calls) == 0 {
			// The model spoke and asked for nothing: it is over. Note that
			// `StopToolUse` with NO call at all lands here on purpose —
			// stopping work waiting for tools the provider did not send would
			// be freezing over an incoherence of theirs.
			res.stop = LoopFinished
			return res, nil
		}
		res.calls += len(calls)

		// ── rule 5, first half: no executor, no action ─────────────────────
		if l.sandbox == nil {
			res.stop = LoopNoSandbox
			res.warnings = append(res.warnings,
				"the model asked for a tool and this installation has no executor "+
					"wired: the turn stopped here WITHOUT executing anything")
			return res, nil
		}

		// ── rule 4: a blown budget stops BEFORE the next round ──────────────
		if accounting.BudgetExceeded {
			res.stop = LoopBudget
			res.warnings = append(res.warnings,
				"the budget blew and the tool loop STOPPED before the next round: "+
					"the calls requested in the last response were not executed (ADR-0011 §2)")
			return res, nil
		}

		// ── rule 1: the cap ─────────────────────────────────────────────────
		if res.rounds >= l.maxRounds {
			res.stop = LoopMaxRounds
			res.warnings = append(res.warnings, fmt.Sprintf(
				"the tool loop hit the cap of %d round(s) and STOPPED: the work did not "+
					"finish, and the %d call(s) from the last response were not executed. "+
					"This is a decision for a human — continuing costs more tokens (ADR-0011)",
				l.maxRounds, len(calls)))
			return res, nil
		}

		results, err := l.execute(ctx, calls)
		if err != nil {
			return nil, err
		}

		// The history grows: the model's utterance WITH the calls, and the
		// results right after. Both messages are mandatory and in this order —
		// both providers refuse a result that does not follow the call
		// justifying it (D11).
		turn.Messages = append(turn.Messages,
			Message{Role: RoleAssistant, Text: exec.ModelReply.Text, ToolCalls: calls},
			Message{Role: RoleToolResult, ToolResults: results},
		)
	}
}

// measure records THIS round's consumption and accumulates the turn's total.
//
// The idempotency key is derived from the round (`…:usage:1`, `…:usage:2`), and
// it is what makes repeating the whole request repeat zero effects. A repetition
// needing MORE rounds than the original writes new lines for the new rounds only
// — and that is right: those tokens were really spent.
func (l loop) measure(ctx context.Context, res *loopResult, r *Reply) (Accounting, error) {
	model := r.Model
	if model == "" {
		model = l.model
	}
	res.totalUsage.InputTokens += r.Usage.InputTokens
	res.totalUsage.OutputTokens += r.Usage.OutputTokens
	res.totalUsage.CacheReadTokens += r.Usage.CacheReadTokens
	res.totalUsage.CacheCreationTokens += r.Usage.CacheCreationTokens

	price, known := l.info.PriceFor(model)
	var cost Micros
	if known {
		cost = price.CostMicros(r.Usage)
		res.totalCost += cost
		if res.currency == "" {
			res.currency = price.Currency
		}
	} else {
		res.priceKnown = false
	}

	return l.routing.RecordUsage(ctx, Consumption{
		DemandID:            l.demandID,
		ThreadID:            l.threadID,
		Model:               model,
		InputTokens:         r.Usage.InputTokens,
		OutputTokens:        r.Usage.OutputTokens,
		CacheReadTokens:     r.Usage.CacheReadTokens,
		CacheCreationTokens: r.Usage.CacheCreationTokens,
		CostMicros:          cost,
		Currency:            price.Currency,
	}, l.usageKey(res.rounds))
}

// execute runs the requested tools, IN ORDER (D10).
//
// Sequentially, and not in parallel: a round's calls share the demand's
// workspace, and `git checkout` running alongside `npm test` in the same
// directory is a race the model did not ask for and cannot debug. The
// parallelism that pays off is the provider asking for several per round — that
// already saves the round, which is where the cost is.
func (l loop) execute(ctx context.Context, calls []ToolCall) ([]ToolResult, error) {
	out := make([]ToolResult, 0, len(calls))
	for _, call := range calls {
		cmd, reason := commandFrom(call, l.allowed)
		if reason != "" {
			out = append(out, errorResult(call, reason))
			continue
		}
		output, err := l.sandbox.RunCommand(ctx, l.demandID, cmd)
		if err != nil {
			if isInfraFailure(err) {
				// Rule 5, second half: this is not the model's business.
				return nil, err
			}
			// A refusal ABOUT THE CALL (name, argument, permission): the model
			// can fix it, so it comes back as a result.
			out = append(out, errorResult(call,
				"the tool refused the call: "+err.Error()))
			continue
		}
		out = append(out, resultFrom(call, output))
	}
	return out, nil
}

// isInfraFailure separates "the executor went down" from "the call was wrong".
//
// The ruler is `errs.Kind`, and the split is by WHO CAN FIX IT:
//
//   - UNAVAILABLE and INTERNAL are ours or the cluster's. No amount of tokens
//     spent by the model solves them, and insisting is burning money against a
//     wall — they go up and kill the turn;
//   - INVALID, NOT FOUND, PERMISSION and PRECONDITION are about the call or
//     about the state the request presupposed. The model reads it, understands
//     it and tries something else — they become error results.
//
// PRECONDITION is the arguable frontier: "sandbox suspended" lands here and the
// model does not resume a sandbox. It stays a result anyway, because the
// alternative — killing the turn — would erase the reply the agent had already
// written, and the message says exactly what happened for whoever reads the
// thread.
func isInfraFailure(err error) bool {
	switch errs.KindOf(err) {
	case errs.KindUnavailable, errs.KindInternal:
		return true
	}
	return false
}

// appendWarnings joins warnings WITHOUT repeating.
//
// It exists because adapter warnings are per CALL and the loop makes several:
// the downgraded-effort warning and the "this provider does not report cache
// creation" one would come out identical eight times in an eight-round turn.
// Repetition informs nothing and drowns the warning that demands a decision —
// which is the problem the attention box already has without help.
//
// Linear comparison on purpose: there are few warnings, and a map here would
// change their ORDER, which is the order in which they appeared.
func appendWarnings(dst []string, added ...string) []string {
	for _, n := range added {
		repeated := false
		for _, j := range dst {
			if j == n {
				repeated = true
				break
			}
		}
		if !repeated {
			dst = append(dst, n)
		}
	}
	return dst
}

// ── the human reading of the stop ───────────────────────────────────────────

// LoopNotice is the sentence the thread shows when the loop did NOT finish on
// its own.
//
// Empty when it did finish: the attention box is only useful if what enters it
// demands a decision, and noise per turn empties it of meaning — the same rule
// as the context-truncation warning.
func LoopNotice(stop LoopStop, rounds, roundCap int) string {
	switch stop {
	case LoopMaxRounds:
		return "⚠️ The agent stopped at the cap of " + strconv.Itoa(roundCap) + " tool round(s) " +
			"(ADR-0011). The work did NOT finish: the reply above is the state it " +
			"stopped in. Decide whether it is worth continuing — each round costs a model call."
	case LoopBudget:
		return "⚠️ The budget blew in the middle of the tool loop and it stopped on round " +
			strconv.Itoa(rounds) + ". What already ran is delivered; the next round does not go out."
	case LoopNoSandbox:
		return "⚠️ The agent asked to run a command and there is no executor " +
			"wired in this installation. It answered without executing anything."
	}
	return ""
}

// unknownToolsWarning drafts the warning for a granted name that does not exist
// in the catalog.
func unknownToolsWarning(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "this thread's card grants tool(s) that do not exist in the runtime's catalog " +
		"and were IGNORED: " + strings.Join(names, ", ")
}
