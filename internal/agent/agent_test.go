package agent

import (
	"os"
	"os/exec"
	"path/filepath"
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
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "Edit", UseID: "t1", File: "/r/a.go", Edits: true}}},
		{"claude shell", ClaudeCode,
			`{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Bash","tool_use_id":"t2","tool_input":{"command":"ls"}}`,
			Event{Name: "PreToolUse", SessionID: "s", Tool: Tool{Name: "Bash", UseID: "t2", Command: "ls", Shell: true}}},
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

// Claude Code's project is the directory it passes; Codex's is the enclosing
// repository, wherever in it the session runs.
func TestProjectHooksFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	sub := filepath.Join(root, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(sub, ".claude", "settings.json"), filepath.Join(sub, ".claude", "settings.local.json")}
	if got := ClaudeCode.ProjectHooksFiles(sub); !reflect.DeepEqual(got, want) {
		t.Errorf("Claude Code: %q", got)
	}
	if got := Codex.ProjectHooksFiles(sub); !reflect.DeepEqual(got, []string{filepath.Join(root, ".codex", "hooks.json")}) {
		t.Errorf("Codex: %q", got)
	}
}
