//go:build e2e

package e2e

import (
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
