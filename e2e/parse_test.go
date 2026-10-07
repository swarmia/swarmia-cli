//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stream parser's inputs follow Claude Code's documented message shapes.
func TestParseStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s","model":"claude-sonnet-5"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Edit","input":{}}]}}`,
		`{"type":"hook","subtype":"response","hook_event":"PostToolUse","hook_name":"x","exit_code":0,"outcome":"success"}`,
		`{"type":"hook","subtype":"response","hook_event":"Stop","hook_name":"bad","exit_code":1,"outcome":"failure","stderr":"boom"}`,
		`{"type":"system","subtype":"hook_response","hook_event":"SessionEnd","exit_code":2}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.12,"session_id":"s"}`,
	}, "\n")
	r := parseStream([]byte(stream))
	if strings.Join(r.ToolUses, ",") != "Read,Edit" || r.Result != "success" || r.NumTurns != 3 || r.CostUSD != 0.12 {
		t.Errorf("parsed %+v", r)
	}
	if len(r.HookErrors) != 2 {
		t.Errorf("hook errors = %v, want the Stop and SessionEnd failures", r.HookErrors)
	}
}

func TestOracleHelpers(t *testing.T) {
	if normalizeLine(`  return x === "https:";`) != normalizeLine(`return x === 'https:'`) {
		t.Error("formatting differences should normalize away")
	}
	if normalizeLine("});") != "" || normalizeLine("  }") != "" {
		t.Error("trivial lines should normalize to empty")
	}
	if id := newSessionID(); len(id) != 36 || id[14] != '4' {
		t.Errorf("session id %q is not a UUIDv4", id)
	}
}

func TestScrub(t *testing.T) {
	e := &env{}
	e.addSecret("short")
	e.addSecret("sk-proj-abcdefghijkl")
	out, found := e.scrub([]byte(`{"env":"OPENAI_API_KEY=sk-proj-abcdefghijkl","n":"short"}`))
	if !found || string(out) != `{"env":"OPENAI_API_KEY=[E2E-SCRUBBED]","n":"short"}` {
		t.Errorf("scrub = %s, %v", out, found)
	}
	if _, found := e.scrub([]byte("nothing here")); found {
		t.Error("found a secret in clean text")
	}
}

func TestParseCodexStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"0199a-thread"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"ls","exit_code":0}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"file_change","changes":[{"path":"a.ts","kind":"update"}]}}`,
		`{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"Done."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}`,
	}, "\n")
	r := parseCodexStream([]byte(stream), []byte("warning: hook SessionStart failed: exit 1\n"))
	if r.SessionID != "0199a-thread" || !r.ReachedModel || r.Result != "success" || r.Message != "Done." {
		t.Errorf("run = %+v", r)
	}
	if strings.Join(r.ToolUses, ",") != "Bash,apply_patch" || len(r.HookErrors) != 1 {
		t.Errorf("tools %v, hook errors %v", r.ToolUses, r.HookErrors)
	}
	failed := parseCodexStream([]byte(`{"type":"thread.started","thread_id":"x"}`+"\n"+`{"type":"turn.failed","error":{"message":"401 Unauthorized"}}`), nil)
	if failed.ReachedModel || !failed.IsError || failed.Message != "401 Unauthorized" {
		t.Errorf("failed run = %+v", failed)
	}
}

func TestCodexWrites(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: src/a.ts\n@@\n keep\n-old\n+export const x = 1;\n*** End Patch"
	args, _ := json.Marshal(map[string]any{"command": []string{"apply_patch", patch}})
	lines := []string{
		mustJSON(t, map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "apply_patch", "input": patch}}),
		mustJSON(t, map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "shell", "arguments": string(args)}}),
		mustJSON(t, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "content": patch}}),
	}
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
	got := codex{}.writes(t, path)
	if strings.Join(got, "|") != "export const x = 1;|export const x = 1;" {
		t.Errorf("writes = %q", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
