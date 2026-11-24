package agent

import (
	"errors"
	"reflect"
	"testing"
)

func TestLookup(t *testing.T) {
	if Lookup("codex") != Codex || Lookup("claude-code") != ClaudeCode || Lookup("copilot") != Copilot || Lookup("cursor") != nil {
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
		{"claude task notification", ClaudeCode,
			`{"hook_event_name":"UserPromptSubmit","prompt":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>\n"}`,
			Event{Name: "UserPromptSubmit", Prompt: "<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>\n", Injected: true}},
		{"claude subagent report, then a notification", ClaudeCode,
			`{"hook_event_name":"UserPromptSubmit","prompt":"<agent-message from=\"a1\">\n  done\n</agent-message>\n<task-notification>x</task-notification>"}`,
			Event{Name: "UserPromptSubmit", Prompt: "<agent-message from=\"a1\">\n  done\n</agent-message>\n<task-notification>x</task-notification>", Injected: true}},
		{"claude prompt quoting a notification", ClaudeCode,
			`{"hook_event_name":"UserPromptSubmit","prompt":"why this?\n<task-notification>x</task-notification>"}`,
			Event{Name: "UserPromptSubmit", Prompt: "why this?\n<task-notification>x</task-notification>"}},
		{"codex has no injected prompts", Codex,
			`{"hook_event_name":"UserPromptSubmit","prompt":"<task-notification>x</task-notification>"}`,
			Event{Name: "UserPromptSubmit", Prompt: "<task-notification>x</task-notification>"}},
		{"system reminder cut from a prompt", ClaudeCode,
			`{"hook_event_name":"UserPromptSubmit","prompt":"\n<system-reminder>\nYou are operating in an isolated workspace.\nWorkspace path: /src/app-wt\n</system-reminder>\nfix the send failures"}`,
			Event{Name: "UserPromptSubmit", Prompt: "fix the send failures"}},
		{"system reminders cut from the middle, in any agent", Codex,
			`{"hook_event_name":"UserPromptSubmit","prompt":"look at this:\n<system-reminder>a</system-reminder>\n<system-reminder id=\"2\">b</system-reminder> and fix it"}`,
			Event{Name: "UserPromptSubmit", Prompt: "look at this:\nand fix it"}},
		{"a prompt of system reminders alone is injected", ClaudeCode,
			`{"hook_event_name":"UserPromptSubmit","prompt":"<system-reminder>\nWorkspace path: /src/app-wt\n</system-reminder>\n"}`,
			Event{Name: "UserPromptSubmit", Injected: true}},
		{"an unclosed system reminder is kept", ClaudeCode,
			`{"hook_event_name":"UserPromptSubmit","prompt":"what does <system-reminder> do?"}`,
			Event{Name: "UserPromptSubmit", Prompt: "what does <system-reminder> do?"}},
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
		{"claude skill: its name, not its arguments", ClaudeCode,
			`{"hook_event_name":"PostToolUse","tool_name":"Skill","tool_input":{"skill":"db-migration","args":"add a column"},"tool_response":{"success":true,"commandName":"db-migration"}}`,
			Event{Name: "PostToolUse", Tool: Tool{Name: "Skill", Skill: "db-migration", Status: StatusOK}}},
		{"claude skill named in command (earlier versions)", ClaudeCode,
			`{"hook_event_name":"PostToolUse","tool_name":"Skill","tool_input":{"command":"pdf"},"tool_response":{"success":true}}`,
			Event{Name: "PostToolUse", Tool: Tool{Name: "Skill", Skill: "pdf", Status: StatusOK}}},
		{"another agent's tool named Skill isn't a skill", Codex,
			`{"hook_event_name":"PostToolUse","tool_name":"Skill","tool_input":{"skill":"x","command":"y"}}`,
			Event{Name: "PostToolUse", Tool: Tool{Name: "Skill", Command: "y"}}},
		{"a skill field on another tool isn't a skill", ClaudeCode,
			`{"hook_event_name":"PostToolUse","tool_name":"mcp__x__y","tool_input":{"skill":"z"}}`,
			Event{Name: "PostToolUse", Tool: Tool{Name: "mcp__x__y", Status: StatusOK}}},
		{"codex patch: its command is the patch", Codex,
			`{"hook_event_name":"PostToolUse","session_id":"s","model":"gpt","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Model: "gpt", Tool: Tool{Name: "apply_patch", Edits: true}}},
		{"codex argv command", Codex,
			`{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"mcp","tool_input":{"command":["a","b"]}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "mcp"}}},

		// Copilot CLI 1.0.112's payloads, trimmed.
		{"copilot prompt", Copilot,
			`{"hook_event_name":"UserPromptSubmit","session_id":"s","timestamp":"2026-11-15T19:24:14.527Z","cwd":"/r","prompt":"fix it"}`,
			Event{Name: "UserPromptSubmit", SessionID: "s", Cwd: "/r", Prompt: "fix it"}},
		{"copilot edit: its file is in path", Copilot,
			`{"hook_event_name":"PostToolUse","session_id":"s","timestamp":"t","cwd":"/r","tool_use_id":"toolu_1","tool_name":"Edit","tool_input":{"path":"/r/a.txt","old_str":"a","new_str":"b"},"tool_result":{"result_type":"success","text_result_for_llm":"File /r/a.txt updated with changes."}}`,
			Event{Name: "PostToolUse", SessionID: "s", Cwd: "/r", Tool: Tool{Name: "Edit", UseID: "toolu_1", File: "/r/a.txt", Edits: true, Status: StatusOK}}},
		{"copilot patch: a bare string", Copilot,
			`{"hook_event_name":"PostToolUse","session_id":"s","timestamp":"t","tool_use_id":"call_1","tool_name":"Edit","tool_input":"*** Begin Patch\n*** Add File: e.txt\n+sub\n*** End Patch\n","tool_result":{"result_type":"success"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "Edit", UseID: "call_1", Edits: true, Status: StatusOK}}},
		{"copilot create failed", Copilot,
			`{"hook_event_name":"PostToolUseFailure","session_id":"s","timestamp":"t","tool_use_id":"toolu_2","tool_name":"Write","tool_input":{"path":"/r/a.txt","file_text":"x"},"error":"Path already exists"}`,
			Event{Name: "PostToolUseFailure", SessionID: "s", Tool: Tool{Name: "Write", UseID: "toolu_2", File: "/r/a.txt", Edits: true, Status: StatusFailed}}},
		{"copilot shell", Copilot,
			`{"hook_event_name":"PreToolUse","session_id":"s","timestamp":"t","tool_use_id":"toolu_3","tool_name":"Bash","tool_input":{"command":"printf x > c.txt","description":"Write c.txt"}}`,
			Event{Name: "PreToolUse", SessionID: "s", Tool: Tool{Name: "Bash", UseID: "toolu_3", Command: "printf x > c.txt", Shell: true}}},
		{"copilot shell exiting nonzero succeeds as a call", Copilot,
			`{"hook_event_name":"PostToolUse","session_id":"s","timestamp":"t","tool_use_id":"toolu_4","tool_name":"Bash","tool_input":{"command":"false"},"tool_result":{"result_type":"success","text_result_for_llm":"\n<shellId: 1 completed with exit code 1>"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "Bash", UseID: "toolu_4", Command: "false", Shell: true, Status: StatusFailed, ExitCode: ptr(1)}}},
		{"copilot shell exiting zero", Copilot,
			`{"hook_event_name":"PostToolUse","session_id":"s","timestamp":"t","tool_use_id":"toolu_5","tool_name":"Bash","tool_input":{"command":"true"},"tool_result":{"result_type":"success","text_result_for_llm":"\n<shellId: 0 completed with exit code 0>"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "Bash", UseID: "toolu_5", Command: "true", Shell: true, Status: StatusOK}}},
		{"copilot async shell starts", Copilot,
			`{"hook_event_name":"PreToolUse","session_id":"s","timestamp":"t","tool_use_id":"toolu_6","tool_name":"Bash","tool_input":{"command":"sleep 2; echo bg > d.txt","mode":"async","shellId":"bg1"}}`,
			Event{Name: "PreToolUse", SessionID: "s", Tool: Tool{Name: "Bash", UseID: "toolu_6", Command: "sleep 2; echo bg > d.txt", Shell: true, Background: "bg1"}}},
		{"copilot async shell runs on", Copilot,
			`{"hook_event_name":"PostToolUse","session_id":"s","timestamp":"t","tool_use_id":"toolu_6","tool_name":"Bash","tool_input":{"command":"sleep 2; echo bg > d.txt","mode":"async"},"tool_result":{"result_type":"success","text_result_for_llm":"<command started in background with shellId: bg1>"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "Bash", UseID: "toolu_6", Command: "sleep 2; echo bg > d.txt", Shell: true, Background: "bg1"}}},
		{"copilot read_bash finds it finished", Copilot,
			`{"hook_event_name":"PostToolUse","session_id":"s","timestamp":"t","tool_use_id":"toolu_7","tool_name":"read_bash","tool_input":{"shellId":"bg1","delay":5},"tool_result":{"result_type":"success","text_result_for_llm":"\n<shellId: bg1 completed with exit code 0>"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "read_bash", UseID: "toolu_7", Status: StatusOK, Awaited: "bg1"}}},
		{"copilot read_bash, still running", Copilot,
			`{"hook_event_name":"PostToolUse","session_id":"s","timestamp":"t","tool_use_id":"toolu_8","tool_name":"read_bash","tool_input":{"shellId":"bg1"},"tool_result":{"result_type":"success","text_result_for_llm":"partial output"}}`,
			Event{Name: "PostToolUse", SessionID: "s", Tool: Tool{Name: "read_bash", UseID: "toolu_8", Status: StatusOK}}},
		{"copilot subagent's prompt: its parent's session, not a person's", Copilot,
			`{"hook_event_name":"UserPromptSubmit","session_id":"sub","timestamp":"t","agent_id":"sub","agent_type":"general-purpose","parent_agent_id":"s","prompt":"create e.txt"}`,
			Event{Name: "UserPromptSubmit", SessionID: "s", Prompt: "create e.txt", Injected: true}},
		{"copilot subagent's turn end doesn't end its parent's", Copilot,
			`{"hook_event_name":"Stop","session_id":"sub","timestamp":"t","agent_id":"sub","parent_agent_id":"s","transcript_path":"/h/.copilot/session-state/s/events.jsonl","stop_reason":"end_turn"}`,
			Event{Name: "SubagentStop", SessionID: "s", TranscriptPath: "/h/.copilot/session-state/s/events.jsonl"}},
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
	want = []string{"/r/pkg/.github/hooks/swarmia.json", "/r/pkg/.github/copilot/settings.local.json"}
	if got := Copilot.ProjectHooksFiles("/r/pkg", "/r"); !reflect.DeepEqual(got, want) {
		t.Errorf("Copilot: %q", got)
	}
	if got := Codex.ProjectHooksFiles("/r/pkg", ""); got != nil {
		t.Errorf("Codex outside a repository: %q", got)
	}
}

func TestHooksFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/me/.claude")
	t.Setenv("CODEX_HOME", "/home/me/.codex")
	t.Setenv("COPILOT_HOME", "/home/me/.copilot")
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
		{Copilot, ScopeProject, "/r/.github/hooks/swarmia.json"},
		{Copilot, ScopeLocal, "/r/.github/copilot/settings.local.json"},
		{Copilot, ScopeUser, "/home/me/.copilot/hooks/swarmia.json"},
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

func TestIsCopilotPayload(t *testing.T) {
	for payload, want := range map[string]bool{
		`{"hook_event_name":"PreToolUse","session_id":"s","timestamp":"2026-11-15T19:26:30.073Z","tool_name":"Edit"}`: true,
		`{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Edit","tool_use_id":"toolu_1"}`:                false,
		`{"hook_event_name":"PreToolUse","timestamp":1794770790}`:                                                     false,
		`{"timestamp":"2026-11-15T19:26:30.073Z"}`:                                                                    false,
		`not json`: false,
	} {
		if got := IsCopilotPayload([]byte(payload)); got != want {
			t.Errorf("IsCopilotPayload(%s) = %v", payload, got)
		}
	}
}
