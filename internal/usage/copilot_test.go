package usage

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// copilotLines are trimmed from Copilot CLI 1.0.112's events.jsonl: a main
// agent's request, a subagent's, another model's, and events without usage.
var copilotLines = []string{
	`{"type":"session.start","data":{"sessionId":"s1","producer":"copilot-agent","copilotVersion":"1.0.112"},"id":"e1","timestamp":"2026-11-15T19:24:13.369Z","parentId":null}`,
	`{"type":"session.model_change","data":{"source":"startup","newModel":"claude-sonnet-5.5"},"id":"e2","timestamp":"2026-11-15T19:24:13.400Z","parentId":"e1"}`,
	`{"type":"user.message","data":{"content":"Replace line2 in a.txt"},"id":"e3","timestamp":"2026-11-15T19:24:14.500Z","parentId":"e2"}`,
	`{"type":"session.usage_record","data":{"usage":{"model":"claude-sonnet-5.5","responseModel":"claude-sonnet-5-5","inputTokens":26316,"outputTokens":123,"cacheReadTokens":18762,"cacheWriteTokens":7550,"reasoningTokens":0,"initiator":"user"}},"id":"e4","timestamp":"2026-11-15T19:24:16.000Z","parentId":"e3"}`,
	`{"type":"assistant.message","data":{"model":"claude-sonnet-5.5","content":"","toolRequests":[{"toolCallId":"t1","name":"edit"}]},"id":"e5","timestamp":"2026-11-15T19:24:16.100Z","parentId":"e4"}`,
	`{"type":"session.usage_record","data":{"usage":{"model":"gpt-5.6-sol","inputTokens":13257,"outputTokens":42,"cacheReadTokens":0,"cacheWriteTokens":0,"reasoningTokens":11,"initiator":"sub-agent"}},"agentId":"a1","id":"e6","timestamp":"2026-11-15T19:24:17.000Z","parentId":"e5"}`,
	`{"type":"assistant.message","data":{"model":"gpt-5.6-sol","content":"Created e.txt.","toolRequests":[]},"agentId":"a1","id":"e7","timestamp":"2026-11-15T19:24:17.100Z","parentId":"e6"}`,
	`{"type":"session.usage_record","data":{"usage":{"model":"claude-sonnet-5.5","inputTokens":27806,"outputTokens":180,"cacheReadTokens":27706,"cacheWriteTokens":98,"reasoningTokens":0,"initiator":"agent"}},"id":"e8","timestamp":"2026-11-15T19:24:18.000Z","parentId":"e7"}`,
	`{"type":"assistant.message","data":{"model":"claude-sonnet-5.5","content":"All five steps ran.","toolRequests":[]},"id":"e9","timestamp":"2026-11-15T19:24:18.100Z","parentId":"e8"}`,
	`{"type":"assistant.message","data":{"model":"claude-sonnet-5.5","content":"","toolRequests":[]},"id":"e10","timestamp":"2026-11-15T19:24:18.200Z","parentId":"e9"}`,
	`{"type":"hook.start","data":{"hookInvocationId":"h1","hookType":"agentStop","input":{"sessionId":"a1","agentId":"a1","parentAgentId":"s1"}},"agentId":"a1","id":"e11","timestamp":"2026-11-15T19:24:18.250Z","parentId":"e10"}`,
	`{"type":"hook.start","data":{"hookInvocationId":"h2","hookType":"agentStop","input":{"sessionId":"s1"}},"id":"e12","timestamp":"2026-11-15T19:24:18.300Z","parentId":"e11"}`,
}

func TestCopilotEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-state", "s1", "events.jsonl")
	write(t, path, copilotLines...)
	var tr Tracker
	if err := tr.Update(CopilotEvents, path); err != nil {
		t.Fatal(err)
	}
	want := []Totals{
		{Model: "claude-sonnet-5.5", Speed: "standard", Requests: 2,
			InputTokens: 4 + 2, CacheReadInputTokens: 18762 + 27706, CacheCreation5mInputTokens: 7550 + 98, OutputTokens: 123 + 180},
		{Model: "gpt-5.6-sol", Speed: "standard", Subagent: true, Requests: 1,
			InputTokens: 13257, OutputTokens: 42, ReasoningOutputTokens: 11},
	}
	if got := tr.Totals(); !reflect.DeepEqual(got, want) {
		t.Errorf("totals:\n got %+v\nwant %+v", got, want)
	}

	// Read incrementally: a further request counts once.
	appendTo(t, path, copilotLines[3]+"\n")
	if err := tr.Update(CopilotEvents, path); err != nil {
		t.Fatal(err)
	}
	if got := tr.Totals()[0].Requests; got != 3 {
		t.Errorf("requests after an appended record = %d, want 3", got)
	}
}

func TestCopilotResponse(t *testing.T) {
	copilotResponseWait, copilotResponsePoll = 100*time.Millisecond, 10*time.Millisecond
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write(t, path, copilotLines...)
	if got := CopilotResponse(path); got != "All five steps ran." {
		t.Errorf("response = %q", got)
	}
	if got := CopilotResponse(filepath.Join(t.TempDir(), "missing.jsonl")); got != "" {
		t.Errorf("response of a missing file = %q", got)
	}

	// A new turn: its end is written after its Stop hook runs, and until
	// then, neither an earlier message of the turn nor the last turn's reply
	// is the reply.
	appendTo(t, path, `{"type":"user.message","data":{"content":"and now?"},"id":"e13"}`+"\n"+
		`{"type":"assistant.message","data":{"content":"Looking into it."},"id":"e14"}`+"\n")
	go func() {
		time.Sleep(30 * time.Millisecond)
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			f.WriteString(`{"type":"assistant.message","data":{"content":"Now done."},"id":"e15"}` + "\n" +
				`{"type":"hook.start","data":{"hookType":"agentStop"},"id":"e16"}` + "\n")
			f.Close()
		}
	}()
	if got := CopilotResponse(path); got != "Now done." {
		t.Errorf("response written late = %q", got)
	}

	// A turn that ended on a tool call has no reply.
	appendTo(t, path, `{"type":"user.message","data":{"content":"once more"},"id":"e17"}`+"\n"+
		`{"type":"hook.start","data":{"hookType":"agentStop"},"id":"e18"}`+"\n")
	if got := CopilotResponse(path); got != "" {
		t.Errorf("response of a turn without one = %q", got)
	}
}
