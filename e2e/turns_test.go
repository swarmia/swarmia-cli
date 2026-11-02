//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Checks of how agents report turns: what their hooks send around a turn's
// prompt and end that the main scenario doesn't exercise. Each logs what the
// agent actually sent (hook-events.txt in the artifacts), so a failure shows
// how the agent behaves, not just that swarmia-cli disagrees.

// TestCodexSubagentTurns has Codex delegate to a subagent. A subagent's own
// prompt and end must not show up as the developer's: the session still has
// one prompt, and one turn the agent ended with its reply. It also checks
// that Codex's Stop hook sends last_assistant_message.
func TestCodexSubagentTurns(t *testing.T) {
	ag := codex{}
	e := setupTurns(t, ag)
	run := e.agentStep(&agentWork{raw: map[string]bool{}, norm: map[string]bool{}}, "1-subagent", "",
		// The session runs a shell command itself, so it worked in the
		// repository and is sent whatever the subagent's events say.
		"First run `ls` yourself. Then use the spawn_agent tool to start exactly one subagent. Ask it to count the TypeScript files in the src "+
			"directory with a single shell command and to report the number. Wait for its answer, then reply with "+
			"one sentence giving the number. Do not edit any files, and do not run git commands.")
	evs := e.hookEvents()
	if run.Transcript == "" || !fileContains(t, run.Transcript, `"spawn_agent"`) {
		t.Skipf("inconclusive: the agent didn't call spawn_agent (result %q, %d turns)", run.Result, run.NumTurns)
	}
	if len(run.HookErrors) > 0 {
		t.Errorf("hooks reported errors: %v", run.HookErrors)
	}

	// What Codex sent: from the subagent, events carrying agent_id, or
	// events of sessions other than the one we started.
	var fromSubagent, otherSessions int
	replies := 0
	for _, ev := range evs {
		switch {
		case ev.SessionID != run.SessionID:
			otherSessions++
		case ev.AgentID != "":
			fromSubagent++
		case ev.Name == "Stop" && ev.LastMessage != "":
			replies++
		}
	}
	t.Logf("observed: %d hook event(s) in the session with agent_id set, %d in other sessions", fromSubagent, otherSessions)
	if replies == 0 {
		t.Error("no Stop event in the session carried last_assistant_message")
	}

	payloads := e.payloads()
	p := findPayload(t, payloads, run.SessionID)
	checkOneTurn(t, p)
	for _, o := range payloads {
		if o.SessionID != run.SessionID {
			t.Errorf("the subagent was reported as a session of its own (%s, %d turn(s)); it's part of %s",
				o.SessionID, len(o.Turns), run.SessionID)
		}
	}
}

// setupTurns prepares a run of a turns check against ag: the fixture with
// our hooks installed, and the agent's credentials, or it skips the test.
func setupTurns(t *testing.T, ag agent) *env {
	t.Helper()
	if envOr("E2E_AGENT", "claude-code") != ag.name() {
		t.Skipf("E2E_AGENT selects %s", envOr("E2E_AGENT", "claude-code"))
	}
	e := newEnv(t, ag)
	e.buildSwarmia()
	ag.install(e, envOr(ag.versionVar(), "latest"))
	e.setupFixture()
	e.installHooks()
	if e.auth.kind == authNone {
		if os.Getenv("E2E_REQUIRE_AUTH") != "" {
			t.Fatal("no credentials for the agent, and E2E_REQUIRE_AUTH is set (see docs/e2e.md)")
		}
		t.Skip("setup OK; no credentials for the agent (see docs/e2e.md), skipping the agent steps")
	}
	return e
}

// checkOneTurn checks that a session the developer prompted once has one
// prompt, in a turn the agent ended with a reply, and that any automatic
// turn was ended too.
func checkOneTurn(t *testing.T, p payload) {
	t.Helper()
	prompts := 0
	for _, turn := range p.Turns {
		if !turn.Automatic {
			prompts++
		}
		if turn.EndedAt == nil || (!turn.Automatic && turn.Response == "") {
			t.Errorf("turn started %s (automatic %v) has no end or reply: ended %v, %d-char reply",
				turn.StartedAt, turn.Automatic, turn.EndedAt != nil, len(turn.Response))
		}
	}
	if prompts != 1 {
		t.Errorf("%d prompts in the session, want 1: %+v", prompts, p.Turns)
	}
}

// payloads processes the logged events and returns what delivery would send.
func (e *env) payloads() []payload {
	e.t.Helper()
	e.run(e.workspace, nil, e.swarmia, "process")
	e.keepBytes("report.txt", []byte(e.run(e.workspace, nil, e.swarmia, "report", "--limit", "0")))
	raw := e.run(e.workspace, nil, e.swarmia, "report", "--json", "--limit", "0")
	e.keepBytes("payloads.json", []byte(raw))
	var ps []payload
	must(e.t, json.Unmarshal([]byte(raw), &ps))
	return ps
}

// hookEvent is one hook event as the agent sent it to swarmia-cli.
type hookEvent struct {
	LoggedAt  string // to the second
	Name      string
	SessionID string
	AgentID   string // Codex: the subagent that sent it
	Prompt    string // UserPromptSubmit
	// LastMessage is Stop's last_assistant_message.
	LastMessage string
	ToolName    string
}

// hookEvents reads every event swarmia-cli logged, in the order logged, and
// keeps a summary of them as hook-events.txt.
func (e *env) hookEvents() []hookEvent {
	e.t.Helper()
	files, err := filepath.Glob(filepath.Join(e.root, "state", "repos", "*", "events.jsonl"))
	must(e.t, err)
	var evs []hookEvent
	for _, f := range files {
		data, err := os.ReadFile(f)
		must(e.t, err)
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			var rec struct {
				LoggedAt string `json:"logged_at"`
				Event    struct {
					Name        string `json:"hook_event_name"`
					SessionID   string `json:"session_id"`
					AgentID     string `json:"agent_id"`
					Prompt      string `json:"prompt"`
					LastMessage string `json:"last_assistant_message"`
					ToolName    string `json:"tool_name"`
				} `json:"event"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			ev := rec.Event
			evs = append(evs, hookEvent{LoggedAt: rec.LoggedAt, Name: ev.Name, SessionID: ev.SessionID,
				AgentID: ev.AgentID, Prompt: ev.Prompt, LastMessage: ev.LastMessage, ToolName: ev.ToolName})
		}
	}
	var b strings.Builder
	for _, ev := range evs {
		fmt.Fprintf(&b, "%s  %-18s session=%s", ev.LoggedAt, ev.Name, ev.SessionID)
		if ev.AgentID != "" {
			fmt.Fprintf(&b, " agent_id=%s", ev.AgentID)
		}
		if ev.ToolName != "" {
			fmt.Fprintf(&b, " tool=%s", ev.ToolName)
		}
		if ev.Prompt != "" {
			fmt.Fprintf(&b, " prompt=%q", truncate(ev.Prompt, 120))
		}
		if ev.LastMessage != "" {
			fmt.Fprintf(&b, " reply=%q", truncate(ev.LastMessage, 120))
		}
		b.WriteString("\n")
	}
	e.keepBytes("hook-events.txt", []byte(b.String()))
	e.t.Logf("hook events:\n%s", b.String())
	return evs
}
