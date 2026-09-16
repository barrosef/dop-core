package agentmetrics_test

import (
	"strings"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/agentmetrics"
)

// A session file, in the shape the tool really writes it — the fields checked
// against a real one before this was written.
const fixture = `{"type":"user","sessionId":"sess-abc","timestamp":"2026-09-03T10:00:00.000Z","cwd":"/workspace","gitBranch":"main","version":"2.1.0","message":{"role":"user","content":"go"}}
{"type":"assistant","sessionId":"sess-abc","uuid":"t1","parentUuid":"","timestamp":"2026-09-03T10:00:05.000Z","isSidechain":false,"message":{"model":"claude-opus-5","stop_reason":"tool_use","usage":{"input_tokens":120,"output_tokens":40,"cache_creation_input_tokens":5000,"cache_read_input_tokens":0,"service_tier":"standard","iterations":1,"speed":"fast"},"content":[{"type":"thinking"},{"type":"tool_use","name":"Bash"},{"type":"tool_use","name":"Read"}]}}
{"type":"assistant","sessionId":"sess-abc","uuid":"t2","parentUuid":"t1","timestamp":"2026-09-03T10:00:20.000Z","isSidechain":true,"message":{"model":"claude-fable-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":90,"cache_creation_input_tokens":0,"cache_read_input_tokens":5000},"content":[{"type":"text"}]}}
{"type":"file-history-snapshot","sessionId":"sess-abc"}
`

func TestItReadsWhatTheToolWroteAndNothingElse(t *testing.T) {
	s, turns := agentmetrics.Parse([]byte(fixture), 0)

	if s.ExternalID != "sess-abc" || s.CWD != "/workspace" || s.GitBranch != "main" || s.ToolVersion != "2.1.0" {
		t.Errorf("the session came out wrong: %+v", s)
	}
	if len(turns) != 2 {
		t.Fatalf("%d turns, want 2 — only the assistant turns with usage count", len(turns))
	}

	one := turns[0]
	if one.Model != "claude-opus-5" || one.StopReason != "tool_use" || one.Sidechain {
		t.Errorf("turn 1: %+v", one)
	}
	if one.InputTokens != 120 || one.OutputTokens != 40 ||
		one.CacheCreationTokens != 5000 || one.CacheReadTokens != 0 {
		t.Errorf("turn 1's counters: %+v", one)
	}
	if one.ToolUses != 2 || one.Thinking != 1 || len(one.Tools) != 2 || one.Tools[0] != "Bash" {
		t.Errorf("turn 1's blocks: uses=%d thinking=%d tools=%v", one.ToolUses, one.Thinking, one.Tools)
	}
	// What has no column of its own is kept whole, so the day a question needs
	// it the data is already there.
	if one.RawUsage["iterations"] == nil || one.RawUsage["speed"] == nil {
		t.Errorf("the rest of usage was dropped: %v", one.RawUsage)
	}
	if one.RawUsage["input_tokens"] != nil {
		t.Error("what has a column was duplicated into raw")
	}
	if !turns[1].Sidechain {
		t.Error("a subagent's turn was not marked, and its cost would be averaged into the main thread's")
	}
}

func TestAnUnfinishedLastLineIsLeftForTheNextPass(t *testing.T) {
	// The file is appended to WHILE it is read. Reading half a record is wrong;
	// counting it as read is worse — the turn would be lost and nothing would
	// say so.
	partial := fixture + `{"type":"assistant","sessionId":"sess-abc","uuid":"t3","message":{"model":"x","usage":{"inp`
	s, turns := agentmetrics.Parse([]byte(partial), 0)
	if len(turns) != 2 {
		t.Fatalf("it read the half-written turn: %d turns", len(turns))
	}
	if s.ByteOffset != int64(len(fixture)) {
		t.Errorf("the cursor moved past a line nobody read: %d, want %d", s.ByteOffset, len(fixture))
	}

	// And continuing from that cursor finds the turn once it is complete.
	rest := `{"type":"assistant","sessionId":"sess-abc","uuid":"t3","timestamp":"2026-09-03T10:01:00.000Z","message":{"model":"claude-opus-5","usage":{"input_tokens":7,"output_tokens":3}}}` + "\n"
	_, more := agentmetrics.Parse([]byte(rest), s.ByteOffset)
	if len(more) != 1 || more[0].UUID != "t3" {
		t.Fatalf("the turn was lost between the two passes: %+v", more)
	}
}

func TestGarbageInTheMiddleDoesNotLoseTheRest(t *testing.T) {
	// Another program writes this file. One unreadable line must not cost a
	// whole session's measurement.
	broken := strings.Replace(fixture, `{"type":"file-history-snapshot","sessionId":"sess-abc"}`, `}{not json`, 1)
	_, turns := agentmetrics.Parse([]byte(broken), 0)
	if len(turns) != 2 {
		t.Fatalf("a bad line took the good ones with it: %d", len(turns))
	}
}

func TestTheCacheRatioIsWhatSaysTheDisciplineIsWorking(t *testing.T) {
	_, turns := agentmetrics.Parse([]byte(fixture), 0)
	// Turn 2 read 5000 from cache and 10 raw: almost everything was cached.
	if r := turns[1].CacheRatio(); r < 0.99 {
		t.Errorf("cache ratio = %.4f", r)
	}
	// Turn 1 CREATED the cache and read nothing from it.
	if r := turns[0].CacheRatio(); r != 0 {
		t.Errorf("a turn that read no cache has ratio %.4f", r)
	}
	// And a turn with no incoming context has no ratio — reporting 100% would
	// say the cache did something it did not.
	if r := (agentmetrics.Turn{OutputTokens: 10}).CacheRatio(); r != 0 {
		t.Errorf("a turn with no context reported %.4f", r)
	}
	if b := turns[0].Billable(); b != 120+40+5000 {
		t.Errorf("billable = %d", b)
	}
}

func TestAnEmptyFileIsNotAnError(t *testing.T) {
	s, turns := agentmetrics.Parse(nil, 0)
	if len(turns) != 0 || s.ByteOffset != 0 {
		t.Errorf("%+v %v", s, turns)
	}
}
