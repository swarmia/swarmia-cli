package agent

import (
	"errors"
	"reflect"
	"testing"
)

func TestLookup(t *testing.T) {
	if Lookup("codex") != Codex || Lookup("claude-code") != ClaudeCode || Lookup("cursor") != nil {
		t.Error("Lookup")
	}
	if OfRecord("") != ClaudeCode || OfRecord("codex") != Codex || OfRecord("cursor") != nil {
		t.Error("OfRecord")
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		agent   *Agent
		payload string
		want    Event
	}{
		{"claude prompt", ClaudeCode,
			`{"hook_event_name":"UserPromptSubmit","session_id":"s","transcript_path":"/t.jsonl","prompt":"hi"}`,
			Event{Name: "UserPromptSubmit", SessionID: "s", TranscriptPath: "/t.jsonl", Prompt: "hi"}},
		{"claude edit", ClaudeCode,
			`{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"Edit","tool_use_id":"t1","tool_input":{"file_path":"/r/a.go"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "Edit", UseID: "t1", File: "/r/a.go", Edits: true, Status: StatusOK}}},
		{"claude shell", ClaudeCode,
			`{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Bash","tool_use_id":"t2","tool_input":{"command":"ls"}}`,
			Event{Name: "PreToolUse", SessionID: "s", Tool: Tool{Name: "Bash", UseID: "t2", Command: "ls", Shell: true}}},
		{"claude shell, nonzero exit counted as success", ClaudeCode,
			`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"grep x f"},"tool_response":{"stdout":"","interrupted":false,"returnCodeInterpretation":"No matches found"}}`,
			Event{Name: "PostToolUse", Tool: Tool{Name: "Bash", Command: "grep x f", Shell: true, Status: StatusOK}}},
		{"claude shell interrupted", ClaudeCode,
			`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"sleep 9"},"tool_response":{"stdout":"","interrupted":true}}`,
			Event{Name: "PostToolUse", Tool: Tool{Name: "Bash", Command: "sleep 9", Shell: true, Status: StatusInterrupted}}},
		{"claude shell failed", ClaudeCode,
			`{"hook_event_name":"PostToolUseFailure","tool_name":"Bash","tool_input":{"command":"make"},"error":"Exit code 2\nmake: *** [test] Error 1","is_interrupt":false}`,
			Event{Name: "PostToolUseFailure", Tool: Tool{Name: "Bash", Command: "make", Shell: true, Status: StatusFailed, ExitCode: ptr(2)}}},
		{"claude shell failed without an exit code", ClaudeCode,
			`{"hook_event_name":"PostToolUseFailure","tool_name":"Bash","tool_input":{"command":"make"},"error":"Command timed out"}`,
			Event{Name: "PostToolUseFailure", Tool: Tool{Name: "Bash", Command: "make", Shell: true, Status: StatusFailed}}},
		{"claude shell interrupted by the user", ClaudeCode,
			`{"hook_event_name":"PostToolUseFailure","tool_name":"Bash","tool_input":{"command":"make"},"error":"Exit code 130","is_interrupt":true}`,
			Event{Name: "PostToolUseFailure", Tool: Tool{Name: "Bash", Command: "make", Shell: true, Status: StatusInterrupted, ExitCode: ptr(130)}}},
		{"claude edit failed: not a shell, so no exit code", ClaudeCode,
			`{"hook_event_name":"PostToolUseFailure","tool_name":"Edit","tool_input":{"file_path":"/r/a.go"},"error":"Exit code 1"}`,
			Event{Name: "PostToolUseFailure", Tool: Tool{Name: "Edit", File: "/r/a.go", Edits: true, Status: StatusFailed}}},
		{"codex patch: its command is the patch", Codex,
			`{"hook_event_name":"PostToolUse","session_id":"s","model":"gpt","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Model: "gpt", Tool: Tool{Name: "apply_patch", Edits: true}}},
		{"codex argv command", Codex,
			`{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"mcp","tool_input":{"command":["a","b"]}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "mcp"}}},
	} {
		got, ok := tc.agent.Parse([]byte(tc.payload))
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, %v; want %+v", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := ClaudeCode.Parse([]byte(`"not an object"`)); ok {
		t.Error("parsed a JSON string")
	}
}

func ptr(n int) *int { return &n }

// Claude Code's project is the directory it passes; Codex's is the enclosing
// repository, wherever in it the session runs.
func TestProjectHooksFiles(t *testing.T) {
	want := []string{"/r/pkg/.claude/settings.json", "/r/pkg/.claude/settings.local.json"}
	if got := ClaudeCode.ProjectHooksFiles("/r/pkg", "/r"); !reflect.DeepEqual(got, want) {
		t.Errorf("Claude Code: %q", got)
	}
	if got := Codex.ProjectHooksFiles("/r/pkg", "/r"); !reflect.DeepEqual(got, []string{"/r/.codex/hooks.json"}) {
		t.Errorf("Codex: %q", got)
	}
	if got := Codex.ProjectHooksFiles("/r/pkg", ""); got != nil {
		t.Errorf("Codex outside a repository: %q", got)
	}
}

func TestHooksFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/me/.claude")
	t.Setenv("CODEX_HOME", "/home/me/.codex")
	for _, tc := range []struct {
		agent *Agent
		scope Scope
		want  string
	}{
		{ClaudeCode, ScopeProject, "/r/.claude/settings.json"},
		{ClaudeCode, ScopeLocal, "/r/.claude/settings.local.json"},
		{ClaudeCode, ScopeUser, "/home/me/.claude/settings.json"},
		{Codex, ScopeProject, "/r/.codex/hooks.json"},
		{Codex, ScopeUser, "/home/me/.codex/hooks.json"},
	} {
		if got, err := tc.agent.HooksFile("/r", tc.scope); err != nil || got != tc.want {
			t.Errorf("%s %s: %q, %v", tc.agent.Name, tc.scope, got, err)
		}
	}
	var noScope *NoScopeError
	if _, err := Codex.HooksFile("/r", ScopeLocal); !errors.As(err, &noScope) {
		t.Errorf("Codex local scope: %v", err)
	}
}
