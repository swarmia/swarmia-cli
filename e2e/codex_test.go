//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codex is OpenAI's Codex CLI, run with `codex exec`.
type codex struct{}

func (codex) name() string       { return "codex" }
func (codex) projectDir() string { return ".codex" }
func (codex) versionVar() string { return "E2E_CODEX_VERSION" }

// installFlags: Codex runs only hooks the user trusts, so the suite trusts
// them the way users can, through swarmia-cli (in the sandbox's CODEX_HOME).
func (codex) installFlags() []string { return []string{"--trust"} }

// install installs Codex at the requested version or dist-tag (latest is the
// stable channel, alpha the development one).
func (codex) install(e *env, version string) {
	e.t.Helper()
	prefix := filepath.Join(e.root, "codex")
	e.run(e.root, nil, "npm", "install", "--prefix", prefix, "--no-audit", "--no-fund", "--loglevel=error",
		"@openai/codex@"+version)
	e.agentBin = filepath.Join(prefix, "node_modules", ".bin", "codex")
	e.version = strings.TrimSpace(e.run(e.root, nil, e.agentBin, "--version"))
	e.t.Logf("Codex %s (requested %q)", e.version, version)
}

// prepare trusts the fixture as a project: Codex reads a project's .codex/
// (our hooks) only once its folder is trusted, which users confirm on first
// use.
func (codex) prepare(e *env) {
	e.t.Helper()
	f, err := os.OpenFile(filepath.Join(e.root, "codex-home", "config.toml"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	must(e.t, err)
	fmt.Fprintf(f, "\n[projects.%q]\ntrust_level = \"trusted\"\n", e.repo)
	f.Close()

	// Codex skips untrusted hooks silently, so check up front, without a
	// model, that it loads ours and trusts them as installed.
	hooks := codexListHooks(e)
	trusted := 0
	for _, h := range hooks {
		if strings.Contains(h.Command, "swarmia-cli") {
			if h.TrustStatus != "trusted" {
				e.t.Errorf("Codex doesn't trust %s (%s): %s", h.Key, h.TrustStatus, h.CurrentHash)
			}
			trusted++
		}
	}
	if trusted != 6 {
		e.t.Fatalf("Codex lists %d swarmia-cli hooks, want 6: %+v", trusted, hooks)
	}
}

type codexHook struct {
	Key         string `json:"key"`
	Command     string `json:"command"`
	CurrentHash string `json:"currentHash"`
	TrustStatus string `json:"trustStatus"`
}

// codexListHooks asks Codex's app server which hooks apply in the fixture.
func codexListHooks(e *env) []codexHook {
	e.t.Helper()
	cmd := exec.Command(e.agentBin, "app-server")
	cmd.Dir = e.repo
	cmd.Env = e.base
	stdin, err := cmd.StdinPipe()
	must(e.t, err)
	stdout, err := cmd.StdoutPipe()
	must(e.t, err)
	must(e.t, cmd.Start())
	defer func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() }()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	await := func(id int) json.RawMessage {
		for sc.Scan() {
			var m struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &m) == nil && m.ID != nil && *m.ID == id {
				if m.Error != nil {
					e.t.Fatalf("codex app-server: %s", m.Error)
				}
				return m.Result
			}
		}
		e.t.Fatalf("codex app-server closed without answering request %d", id)
		return nil
	}
	fmt.Fprintln(stdin, `{"method":"initialize","id":1,"params":{"clientInfo":{"name":"swarmia_cli_e2e","title":"swarmia-cli e2e","version":"1.0.0"}}}`)
	await(1)
	fmt.Fprintln(stdin, `{"method":"initialized"}`)
	req, _ := json.Marshal(map[string]any{"method": "hooks/list", "id": 2, "params": map[string]any{"cwds": []string{e.repo}}})
	fmt.Fprintln(stdin, string(req))
	var res struct {
		Data []struct {
			Hooks  []codexHook       `json:"hooks"`
			Errors []json.RawMessage `json:"errors"`
		} `json:"data"`
	}
	must(e.t, json.Unmarshal(await(2), &res))
	var out []codexHook
	for _, d := range res.Data {
		if len(d.Errors) > 0 {
			e.t.Errorf("Codex reports hook errors: %s", d.Errors)
		}
		out = append(out, d.Hooks...)
	}
	return out
}

// detectAuth: an OpenAI API key. codex exec reads it from CODEX_API_KEY, which
// only the agent process gets.
func (codex) detectAuth() auth {
	for _, k := range []string{"CODEX_API_KEY", "OPENAI_API_KEY"} {
		if v := os.Getenv(k); v != "" {
			return auth{kind: authAPIKey, vars: []string{"CODEX_API_KEY=" + v}}
		}
	}
	return auth{}
}

// run runs `codex exec --json`; Codex picks the session id (its thread id),
// which the output stream starts with.
func (codex) run(e *env, step, sessionID, prompt string) agentRun {
	e.t.Helper()
	args := []string{"exec"}
	if sessionID != "" {
		args = append(args, "resume", sessionID)
	}
	args = append(args, "--json", "--dangerously-bypass-approvals-and-sandbox")
	if m := os.Getenv("E2E_CODEX_MODEL"); m != "" {
		args = append(args, "--model", m)
	}
	args = append(args, prompt)
	cmd := exec.Command(e.agentBin, args...)
	cmd.Dir = e.repo
	cmd.Env = append(append([]string{}, e.base...), e.agentEnv()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	e.t.Logf("agent step %q took %s", step, time.Since(start).Round(time.Second))
	e.keepBytes(step+".stream.jsonl", stdout.Bytes())
	e.keepBytes(step+".stderr.txt", stderr.Bytes())

	r := parseCodexStream(stdout.Bytes(), stderr.Bytes())
	if sessionID != "" && r.SessionID == "" {
		r.SessionID = sessionID
	}
	if r.SessionID == "" {
		e.t.Fatalf("codex exec started no session: %v\nstderr:\n%s", err, truncate(stderr.String(), 4000))
	}
	matches, _ := filepath.Glob(filepath.Join(e.root, "codex-home", "sessions", "*", "*", "*", "rollout-*"+r.SessionID+".jsonl"))
	if len(matches) > 0 {
		r.Transcript = matches[len(matches)-1]
		e.keep(r.Transcript, step+".transcript.jsonl")
	}
	return r
}

// parseCodexStream reads codex exec's JSON lines: the thread id, completed
// tool items, and how the turn ended. Hook failures only show on stderr.
func parseCodexStream(stdout, stderr []byte) agentRun {
	var r agentRun
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var m struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "thread.started":
			r.SessionID = m.ThreadID
		case "item.completed":
			switch m.Item.Type {
			case "command_execution":
				r.ToolUses = append(r.ToolUses, "Bash")
			case "file_change":
				r.ToolUses = append(r.ToolUses, "apply_patch")
			case "mcp_tool_call", "web_search":
				r.ToolUses = append(r.ToolUses, m.Item.Type)
			case "agent_message":
				r.Message = m.Item.Text
			}
		case "turn.completed":
			r.NumTurns++
			r.ReachedModel = true
			r.Result = "success"
		case "turn.failed":
			r.IsError, r.Result, r.Message = true, "failed", m.Error.Message
		case "error":
			if r.Message == "" {
				r.Message = m.Message
			}
		}
	}
	for _, l := range strings.Split(string(stderr), "\n") {
		if low := strings.ToLower(l); strings.Contains(low, "hook") && (strings.Contains(low, "fail") || strings.Contains(low, "error")) {
			r.HookErrors = append(r.HookErrors, l)
		}
	}
	return r
}

// outputTokens is the session's last cumulative total in its rollouts (a
// resumed session continues its own).
func (codex) outputTokens(t *testing.T, transcripts []string) int64 {
	t.Helper()
	var most int64
	for _, transcript := range transcripts {
		data, err := os.ReadFile(transcript)
		must(t, err)
		for _, line := range bytes.Split(data, []byte("\n")) {
			var l struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
					Info *struct {
						Total struct {
							OutputTokens int64 `json:"output_tokens"`
						} `json:"total_token_usage"`
					} `json:"info"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &l) == nil && l.Type == "event_msg" && l.Payload.Type == "token_count" && l.Payload.Info != nil {
				most = max(most, l.Payload.Info.Total.OutputTokens)
			}
		}
	}
	return most
}

// writes returns the lines the agent added with apply_patch: the "+" lines of
// every patch in the rollout's tool calls, wherever the call carries it (as
// the input of a custom tool call, or inside a function call's JSON
// arguments).
func (codex) writes(t *testing.T, transcript string) []string {
	t.Helper()
	data, err := os.ReadFile(transcript)
	must(t, err)
	var lines []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		case string:
			if !strings.Contains(x, "*** Begin Patch") {
				return
			}
			var inner any
			if t := strings.TrimSpace(x); (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Unmarshal([]byte(t), &inner) == nil {
				walk(inner)
				return
			}
			for _, l := range strings.Split(x, "\n") {
				if strings.HasPrefix(l, "+") {
					lines = append(lines, l[1:])
				}
			}
		}
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var l struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "response_item" || !strings.HasSuffix(l.Payload.Type, "call") {
			continue
		}
		var v any
		json.Unmarshal(line, &v)
		walk(v)
	}
	return lines
}
