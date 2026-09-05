package agent

import (
	"context"
	"strings"
)

// ════════════════════════════════════════════════════════════════════════════
// THE TURN — talking to the model and INTERPRETING what came back.
//
// The split from service.go is deliberate and survived the crossing from Python:
//
//   - HERE lives everything that does not touch the neighbouring domains:
//     calling the provider's port and turning the structured response into domain
//     FACTS (what to reply, whether it concluded, which finding);
//   - THERE lives the cycle with the neighbours — context, routing, measurement,
//     message, finding, budget pause.
//
// The practical consequence that earns the split: the conversation with the
// model is testable with no neighbour at all, and the cycle is testable with no
// provider at all. The two halves fail for different reasons — a provider being
// down is not a database being down — and merging them would make every failure
// look the same.
//
// On "CONCLUDING REQUIRES PUBLISHING A FINDING": the conversation spec §1 says
// the thread does not die in silence, and `demand.Service.ConcludeThread`
// already refuses to conclude without a published finding. Here the rule appears
// one step earlier: if the model marked `concluded` but wrote neither a title nor
// a summary, the conclusion is REFUSED — the thread stays active and the response
// gains a warning. Accepting the empty conclusion would let the thread die in
// silence behind a decorative `true`, and the next agent would redo the work.
// ════════════════════════════════════════════════════════════════════════════

// Finding is the finding exactly as the model produced it, before it becomes a
// record.
type Finding struct {
	Title   string
	Payload map[string]any
}

// TurnExecution is what the turn produced, in domain facts.
//
// `Reply` is ALWAYS text for the thread — even when the model returned
// structured JSON, because it is a person who reads the thread. `Finding` is nil
// while the thread has not concluded, rather than an empty finding: an empty
// finding published would be the durable record of nothing.
type TurnExecution struct {
	Reply      string
	Concluded  bool
	Finding    *Finding
	Turn       Turn
	ModelReply *Reply
	Warnings   []string
}

// replyText is the utterance that goes to the thread.
//
// When there is structured output, it is the `reply` field. When there is not —
// the provider returned loose text because the schema failed, say — it is the
// raw text: returning empty would hide from the human the only thing the model
// produced.
func replyText(r *Reply) string {
	if s := strings.TrimSpace(str(r.Data["reply"])); s != "" {
		return s
	}
	return strings.TrimSpace(r.Text)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func findingFrom(data map[string]any) *Finding {
	title := strings.TrimSpace(str(data["finding_title"]))
	summary := strings.TrimSpace(str(data["finding_summary"]))
	if title == "" || summary == "" {
		return nil
	}
	// The evidence arrives as `[]any` from the JSON decoding. An item that is not
	// a string is DISCARDED rather than turned into an error: a finding with one
	// piece of evidence fewer is still worth more than a turn lost over a type.
	var evidence []string
	if list, ok := data["finding_evidence"].([]any); ok {
		for _, e := range list {
			if s := strings.TrimSpace(str(e)); s != "" {
				evidence = append(evidence, s)
			}
		}
	}
	if evidence == nil {
		evidence = []string{}
	}
	return &Finding{
		Title:   title,
		Payload: map[string]any{"summary": summary, "evidence": evidence},
	}
}

// executeTurn is ONE round: send the conversation, read the response, decide
// whether it concluded.
//
// It does not retry and it is DELIBERATELY a single round: the loop in
// toolloop.go is what chains rounds, and it has the ports to measure and to act.
// Keeping this function loop-free is what lets the budget interrupt BETWEEN one
// round and the next (ADR-0011 §2) rather than in the middle of one, and it is
// what keeps the response interpretation testable without a executor and
// without the cost domain.
func executeTurn(ctx context.Context, p AgentProvider, t Turn,
	model string, effort Effort) (*TurnExecution, error) {

	response, err := p.Send(ctx, t, model, effort)
	if err != nil {
		return nil, err
	}
	data := response.Data
	if data == nil {
		data = map[string]any{}
	}
	warnings := append([]string(nil), response.Warnings...)

	concluded, _ := data["concluded"].(bool)
	if len(response.ToolCalls) > 0 {
		// Asking for a tool IS saying "I am not done yet", and the request
		// outweighs the field: a model that marks `concluded` and in the same
		// breath asks to run the tests concluded nothing. No warning, on purpose —
		// this is not a contract violation, it is the field arriving early in a
		// response the loop is going to continue anyway.
		concluded = false
	}
	var finding *Finding
	if concluded {
		finding = findingFrom(data)
	}
	if concluded && finding == nil {
		// Concluding REQUIRES a finding (conversation spec §1). Without one the
		// conclusion does not hold: the thread stays active and the agent is asked
		// again on the next turn, instead of disappearing off the radar behind a
		// decorative `true`.
		concluded = false
		warnings = append(warnings,
			"the model marked a conclusion with no finding (title and summary): the "+
				"conclusion was REFUSED — concluding requires publishing a finding (spec §1)")
	}

	switch response.StopReason {
	case StopToolUse:
		// The loop's NORMAL stop: the model asked for a tool. No warning — warning
		// on every round would fill the result with noise and hide the warnings
		// that require a decision.
	case StopMaxTokens:
		// A truncated response is not a finished response. Without this warning, a
		// cut-off turn would look like nothing more than a short answer.
		warnings = append(warnings,
			"the response was CUT OFF by the output token limit: the content "+
				"below is incomplete")
	case StopRefused:
		warnings = append(warnings, "the provider REFUSED the request under its own policy")
	}

	if !response.Capabilities.Has(CapCacheCreationAccounting) {
		// ADR-0012 §1: without this capability, CacheCreationTokens comes back
		// zero, and that means "cannot be known", not "nothing was written to the
		// cache".
		warnings = append(warnings,
			"provider '"+response.Provider+"' does not report cache creation: the "+
				"cache_creation_tokens field comes back zero from the ABSENCE of information (D1)")
	}

	return &TurnExecution{
		Reply:      replyText(response),
		Concluded:  concluded,
		Finding:    finding,
		Turn:       t,
		ModelReply: response,
		Warnings:   warnings,
	}, nil
}
