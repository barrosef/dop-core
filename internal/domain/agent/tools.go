package agent

import (
	"fmt"
	"sort"
	"strings"
)

// ════════════════════════════════════════════════════════════════════════════
// THE TOOL CATALOGUE — what the agent can DO, and not only say.
//
// The catalogue lives in the DOMAIN, and not in the substrate, for two reasons
// that pull the same way:
//
//  1. a tool declaration IS PROMPT. It goes into the body before the
//     conversation (guarantee 15) and is part of what the provider caches. If the
//     text came from the execution domain, a description change there would
//     invalidate the prefix of every thread of every account here — ADR-0012 §1's
//     silent invalidator, now with its trigger in a package that does not even
//     know it exists;
//
//  2. the contract with the model belongs to the runtime. Name, description and
//     schema are what the agent reads to decide what to do; the substrate only
//     knows how to run a command.
//
// ── WHY ONLY ONE TOOL ───────────────────────────────────────────────────────
//
// The temptation is to publish a menu: `read_file`, `write_file`, `list_dir`,
// `run_tests`, `git_status`. Every one of them is a command under another name,
// and each costs three things: permanent tokens in EVERY turn's prefix, one more
// surface for the model to choose wrongly from, and one more translation to
// maintain. `run_command` composes them all: `cat`, `ls`, `git status`, the
// project's test runner.
//
// And there is a gain that is not about economy. With a single tool, the
// demand's audit trail shows the COMMAND that ran — `sh -c 'npm test'` — instead
// of `run_tests{}`, which hides what was executed behind a name of ours. On a
// platform whose premise is telling apart what the agent did from what the human
// did (ADR-0006), hiding the command would be working against the premise.
//
// ── NO IMPLICIT SHELL ───────────────────────────────────────────────────────
//
// `command` is argv. Whoever wants a pipeline passes `["sh","-c","… | …"]`, and
// the shell shows up in the audit trail. Wrapping everything in `sh -c`
// internally would do the opposite: every command would read as safe and none
// would be.
// ════════════════════════════════════════════════════════════════════════════

// ToolRunCommand is the tool's name, exactly as it appears in the thread's brief
// (`AgentCard.Tools`) and in the declaration sent to the provider.
const ToolRunCommand = "run_command"

// Limits of ONE command, in the runtime's vocabulary.
//
// They belong to the DOMAIN and not to the adapter because they are cost policy,
// not a substrate detail: a tool's output becomes the next turn's context,
// context becomes tokens and tokens become an invoice (ADR-0011). The model's
// ceiling may even become configurable one day; what it cannot do is not
// exist.
const (
	// DefaultToolTimeoutSeconds is a tool command's deadline. Shorter than the
	// port's default (2 min) on purpose: here the caller is a loop that already
	// resends the conversation each round, and a command hanging for two minutes
	// per round multiplies the wait by the round ceiling.
	DefaultToolTimeoutSeconds = 90
	// MaxToolTimeoutSeconds is the ceiling the MODEL may ask for. It chooses
	// within the range; going past it is silently lowered to the ceiling — a
	// silence that is acceptable because it does not change the semantics, only
	// the waiting time.
	MaxToolTimeoutSeconds = 300
	// DefaultToolOutputBytes is how much of the output goes back to the model.
	// 32 KiB is on the order of 8 thousand tokens: a long log fits, and a `cat` of
	// a binary does not bring the turn down.
	DefaultToolOutputBytes = 32 << 10
)

// runCommandSpec is the tool's declaration.
//
// It is a FUNCTION and not a package variable for the same reason as
// `OutputSchema()`: it returns maps, and a variable would be shared — the first
// adapter that touched it by accident would change the contract of every turn of
// every account.
func runCommandSpec() ToolSpec {
	return ToolSpec{
		Name: ToolRunCommand,
		Description: "Runs a command inside this demand's sandbox and returns the output. " +
			"The working directory is " + SandboxWorkspaceHint + ". " +
			"There is NO implicit shell: for a pipeline or a redirection, use " +
			`["sh","-c","..."]` + ". " +
			"A non-zero exit code is a normal result — read the output and fix it.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "The command as a list of arguments (argv). E.g. [\"git\",\"status\"].",
				},
				"timeout_seconds": map[string]any{
					"type": "integer",
					"description": fmt.Sprintf(
						"The command's deadline in seconds. Default %d, maximum %d.",
						DefaultToolTimeoutSeconds, MaxToolTimeoutSeconds),
				},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	}
}

// SandboxWorkspaceHint is the workspace path AS THE AGENT READS IT.
//
// It repeats the value of `ports.SandboxWorkspacePath`, and the repetition is
// deliberate: this domain does not import `ports` (that is the infrastructure
// package), and importing it just for one string would put an infra port inside
// the runtime. The glue in internal/app is what guarantees the two values agree
// — and it is the same price already paid for `Micros` not importing `cost`.
const SandboxWorkspaceHint = "/workspace"

// catalogue is the complete menu, by name. A map is safe here because nothing
// leaves it by iteration: `ToolCatalog` builds the ORDERED list.
func catalogue() map[string]ToolSpec {
	return map[string]ToolSpec{ToolRunCommand: runCommandSpec()}
}

// ToolCatalog returns the tools GRANTED to this thread, ordered by name, plus
// the granted names that do not exist in the catalogue.
//
// Two decisions, and both become visible behaviour:
//
//   - the ORDER is alphabetical and not the brief's. The brief is account data,
//     written by a person, and the order in which somebody typed two tools is
//     nobody's choice — but it would change the prefix's bytes and the cache
//     entry with them (ADR-0012 §1, prompt.go's layer 3);
//
//   - a name GRANTED AND NONEXISTENT is neither silence nor an error: it comes
//     back in the second list, and the turn's cycle turns it into a readable
//     warning. Silencing it would make the agent look capable of something nobody
//     installed; failing the turn would let one wrong line in a thread's brief
//     stop all of its work.
func ToolCatalog(granted []string) (specs []ToolSpec, unknown []string) {
	if len(granted) == 0 {
		return nil, nil
	}
	available := catalogue()
	seen := make(map[string]bool, len(granted))
	for _, name := range granted {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if spec, ok := available[name]; ok {
			specs = append(specs, spec)
			continue
		}
		unknown = append(unknown, name)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	sort.Strings(unknown)
	return specs, unknown
}

// ── da chamada do modelo ao comando do substrato ────────────────────────────

// commandFrom translates a tool call into a substrate command.
//
// Every refusal path here returns a REASON, not an error: the loop receives the
// reason and hands it to the model as an error result. A wrong argument is
// something the model fixes on its own on the next round — as long as it knows
// what was wrong.
func commandFrom(call ToolCall, allowed []ToolSpec) (SandboxCommand, string) {
	if !toolAllowed(call.Name, allowed) {
		// D11: the model called what was not declared. Neither provider prevents
		// that, so the loop is what prevents it — and the list of valid names goes
		// along, because a refusal with no alternative makes the model try the same
		// name again.
		return SandboxCommand{}, fmt.Sprintf(
			"tool %q was not granted to this thread. Available: %s.",
			call.Name, namesOf(allowed))
	}
	if call.Input == nil {
		// D8: the provider sent an argument that does not decode. The raw text goes
		// along — "your argument is invalid" without saying which argument is a
		// message that fixes nothing.
		return SandboxCommand{}, fmt.Sprintf(
			"the arguments are not a valid JSON object. Received: %s",
			summarize(call.RawInput, 400))
	}

	argv, ok := stringList(call.Input["command"])
	if !ok || len(argv) == 0 {
		return SandboxCommand{}, `the "command" field is required and has to be a ` +
			`non-empty list of strings. E.g. {"command":["git","status"]}`
	}

	timeout := DefaultToolTimeoutSeconds
	if n, ok := intFrom(call.Input["timeout_seconds"]); ok && n > 0 {
		timeout = n
		if timeout > MaxToolTimeoutSeconds {
			timeout = MaxToolTimeoutSeconds
		}
	}
	return SandboxCommand{
		Command:        argv,
		TimeoutSeconds: timeout,
		MaxOutputBytes: DefaultToolOutputBytes,
	}, ""
}

func toolAllowed(name string, allowed []ToolSpec) bool {
	for _, s := range allowed {
		if s.Name == name {
			return true
		}
	}
	return false
}

func namesOf(specs []ToolSpec) string {
	if len(specs) == 0 {
		return "none"
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// stringList accepts the list as JSON decoding delivers it (`[]any`). An item
// that is not a string BRINGS DOWN the whole conversion, and here — unlike the
// finding's evidence — discarding would be worse: an argv with one argument
// fewer is not a worse command, it is ANOTHER command.
func stringList(v any) ([]string, bool) {
	raw, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// intFrom reads a number from JSON decoding, which always arrives as float64.
func intFrom(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// ── do resultado do substrato ao resultado do modelo ────────────────────────

// resultFrom builds what the MODEL will read.
//
// The format is fixed and readable on purpose: the model has to tell apart,
// without guessing, three things that look alike in loose output — the command
// finished well, the command failed, and the command did not finish at all. And
// `Truncated` appears WHENEVER there was a cut: an agent that concludes from cut
// output without knowing it was cut concludes wrongly, and nobody can explain
// why afterwards.
func resultFrom(call ToolCall, out SandboxOutput) ToolResult {
	var b strings.Builder
	switch {
	case out.TimedOut:
		fmt.Fprintf(&b, "The command did NOT finish within the deadline and was abandoned.\n")
	default:
		fmt.Fprintf(&b, "exit_code: %d\n", out.ExitCode)
	}
	if out.Truncated {
		b.WriteString("WARNING: the output was CUT by size — what follows is partial.\n")
	}
	b.WriteString("stdout:\n" + textOrEmpty(out.Stdout))
	b.WriteString("stderr:\n" + textOrEmpty(out.Stderr))

	return ToolResult{
		CallID:  call.ID,
		Name:    call.Name,
		Content: strings.TrimRight(b.String(), "\n"),
		IsError: out.Failed(),
	}
}

// errorResult is the refusal the model can correct.
func errorResult(call ToolCall, reason string) ToolResult {
	return ToolResult{CallID: call.ID, Name: call.Name, Content: reason, IsError: true}
}

func textOrEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)\n"
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// summarize cuts text to fit into an error message, saying that it cut.
func summarize(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "… (cut)"
}
