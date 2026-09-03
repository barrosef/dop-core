// Package agentmetrics is what the platform LEARNS from a session, not what it
// produces (P-23 phase 1).
//
// ── Why this exists, and why it does not produce anything ───────────────────
//
// Phase 1 measures a demand's token consumption while the loop is Claude Code's.
// The tool already writes, per turn, everything worth measuring: input, output,
// cache creation, cache read, the model, the stop reason, the turn tree, whether
// it was a subagent, and what the turn was made of. Producing our own version of
// that would be a second and poorer truth.
//
// So the package parses what is already written and gives it a shape that can be
// queried. Nothing here invents a number.
//
// ── Why the cache split is the whole point ──────────────────────────────────
//
// Measured in a real session: 2.55 BILLION tokens read from cache against 23
// thousand of raw input. Anything that recorded "input tokens" alone would be
// wrong by five orders of magnitude about what a demand costs — and the tuning
// phase 1 exists for is exactly about moving tokens from one column to another.
package agentmetrics

import (
	"encoding/json"
	"strings"
	"time"
)

// Session is one run of the agent, as the tool identifies it.
type Session struct {
	ID          string
	AccountID   string
	DemandID    string
	ProjectID   string
	ExternalID  string // the tool's own sessionId
	CWD         string
	GitBranch   string
	ToolVersion string
	StartedAt   time.Time
	EndedAt     time.Time
	// ByteOffset is the collector's cursor into the session file. The file grows
	// while the agent works; re-reading it whole on each pass is not collection.
	ByteOffset int64
}

// Turn is one assistant turn with its consumption.
type Turn struct {
	UUID        string
	ParentUUID  string
	OccurredAt  time.Time
	Model       string
	ServiceTier string
	StopReason  string
	Sidechain   bool

	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64

	ToolUses int
	Thinking int
	Texts    int
	Tools    []string
	// RawUsage keeps everything the tool reported that has no column yet — the
	// cache TTL split, output details, iterations, speed. Kept whole so that the
	// day a question needs it, the data is there instead of starting to be
	// collected then.
	RawUsage map[string]any
}

// Billable is what actually reaches the model on this turn.
//
// A cache READ is not free, and it is not full price either; the ratio between
// the three is the number phase 1 is trying to move. The method exists so the
// arithmetic lives in one place instead of in each query.
func (t Turn) Billable() int64 {
	return t.InputTokens + t.CacheCreationTokens + t.CacheReadTokens + t.OutputTokens
}

// CacheRatio is the share of the incoming context that came from cache — the
// single number that says whether the prefix discipline is working.
//
// It returns 0 for a turn with no incoming context rather than dividing by
// zero: a turn that read nothing has no ratio, and reporting 100% would say the
// cache did something it did not.
func (t Turn) CacheRatio() float64 {
	in := t.InputTokens + t.CacheCreationTokens + t.CacheReadTokens
	if in == 0 {
		return 0
	}
	return float64(t.CacheReadTokens) / float64(in)
}

// ── the tool's file ─────────────────────────────────────────────────────────

// Parse reads the lines of a session file and returns what was found.
//
// It is deliberately FORGIVING: a line it does not understand is skipped, not
// an error. The file is written by another program, while it runs, and it is
// appended to as we read — a truncated last line is the normal state of things,
// not a defect. Refusing the whole file over one bad line would mean losing a
// session's measurement because we read it a millisecond too early.
//
// `offset` is where reading started, used only to report where it ended.
func Parse(content []byte, offset int64) (Session, []Turn) {
	var s Session
	var turns []Turn
	consumed := int64(0)

	// ONLY terminated lines are read, and only they advance the cursor. The file
	// is appended to while we read it, so the last line is very often half
	// written: parsing it would be reading half a record, and counting it would
	// move the cursor past a line nobody ever read — a loss that shows up as a
	// turn missing from the metrics and as nothing at all in the logs.
	for _, raw := range terminatedLines(content) {
		consumed += int64(len(raw)) + 1
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // an unfinished line: the writer is still writing
		}
		if rec.SessionID != "" && s.ExternalID == "" {
			s.ExternalID = rec.SessionID
		}
		if rec.CWD != "" {
			s.CWD = rec.CWD
		}
		if rec.GitBranch != "" {
			s.GitBranch = rec.GitBranch
		}
		if rec.Version != "" {
			s.ToolVersion = rec.Version
		}
		if at, ok := parseTime(rec.Timestamp); ok {
			if s.StartedAt.IsZero() || at.Before(s.StartedAt) {
				s.StartedAt = at
			}
			if at.After(s.EndedAt) {
				s.EndedAt = at
			}
		}
		if rec.Type != "assistant" || rec.Message == nil || rec.Message.Usage == nil {
			continue
		}
		turns = append(turns, turnOf(rec))
	}
	s.ByteOffset = offset + consumed
	return s, turns
}

func turnOf(rec record) Turn {
	u := rec.Message.Usage
	at, _ := parseTime(rec.Timestamp)
	t := Turn{
		UUID: rec.UUID, ParentUUID: rec.ParentUUID, OccurredAt: at,
		Model: rec.Message.Model, StopReason: rec.Message.StopReason,
		Sidechain: rec.IsSidechain,
		RawUsage:  map[string]any{},
	}
	t.InputTokens = intOf(u["input_tokens"])
	t.OutputTokens = intOf(u["output_tokens"])
	t.CacheCreationTokens = intOf(u["cache_creation_input_tokens"])
	t.CacheReadTokens = intOf(u["cache_read_input_tokens"])
	if v, ok := u["service_tier"].(string); ok {
		t.ServiceTier = v
	}
	// Everything with no column of its own is kept whole.
	for k, v := range u {
		switch k {
		case "input_tokens", "output_tokens", "cache_creation_input_tokens",
			"cache_read_input_tokens", "service_tier":
		default:
			t.RawUsage[k] = v
		}
	}
	for _, block := range rec.Message.blocks() {
		switch block.Type {
		case "tool_use":
			t.ToolUses++
			if block.Name != "" {
				t.Tools = append(t.Tools, block.Name)
			}
		case "thinking":
			t.Thinking++
		case "text":
			t.Texts++
		}
	}
	if t.Tools == nil {
		t.Tools = []string{}
	}
	return t
}

type record struct {
	Type        string   `json:"type"`
	SessionID   string   `json:"sessionId"`
	UUID        string   `json:"uuid"`
	ParentUUID  string   `json:"parentUuid"`
	Timestamp   string   `json:"timestamp"`
	CWD         string   `json:"cwd"`
	GitBranch   string   `json:"gitBranch"`
	Version     string   `json:"version"`
	IsSidechain bool     `json:"isSidechain"`
	Message     *message `json:"message"`
}

type message struct {
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Usage      map[string]any `json:"usage"`
	// Content is RAW because the same field is a string on a user's message and
	// an array of blocks on the assistant's. Typing it as an array made every
	// user line fail to parse — and with it the cwd, the branch and the tool
	// version, which only those lines carry.
	Content json.RawMessage `json:"content"`
}

// blocks decodes the content only when it is an array. Anything else is a
// message with no blocks, which is the truthful answer.
func (m message) blocks() []block {
	if len(m.Content) == 0 || m.Content[0] != '[' {
		return nil
	}
	var out []block
	if err := json.Unmarshal(m.Content, &out); err != nil {
		return nil
	}
	return out
}

type block struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func intOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// terminatedLines returns only the lines that ended with a newline. An
// unterminated tail is left for the next pass, which is what makes the cursor
// safe to trust.
func terminatedLines(b []byte) []string {
	parts := strings.Split(string(b), "\n")
	return parts[:len(parts)-1] // the element after the last "\n" is the tail
}
