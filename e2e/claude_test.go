//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agent is a coding agent the scenario runs: how to install, authenticate and
// run it headless, and the independent oracles over its transcripts.
type agent interface {
	name() string       // as in payloads: claude-code, codex
	projectDir() string // its project settings directory: .claude, .codex
	versionVar() string // the variable choosing its version or dist-tag
	install(e *env, version string)
	installFlags() []string // extra flags for swarmia-cli install
	prepare(e *env)         // after install, before the first run
	detectAuth() auth
	// run runs the agent headless in the fixture: a new session if sessionID
	// is "", else a further turn of that session.
	run(e *env, step, sessionID, prompt string) agentRun
	// writes lists every line the agent put into files with its edit tools,
	// from a transcript.
	writes(t *testing.T, transcript string) []string
	// outputTokens counts a session's output tokens over its transcripts,
	// without internal/usage, to check it.
	outputTokens(t *testing.T, transcripts []string) int64
}

// agentRun is what one headless run did, from its output stream.
type agentRun struct {
	SessionID    string
	ToolUses     []string // tool names, in order
	Result       string   // how the run ended, e.g. "success"
	Message      string   // the result text (the agent's answer, or the error)
	IsError      bool
	ReachedModel bool // any model response at all (else: no credentials, an outage)
	CostUSD      float64
	NumTurns     int
	HookErrors   []string
	Transcript   string // path to the session transcript
}

// --- credentials -------------------------------------------------------------------

type authKind int

const (
	authNone authKind = iota
	authFederation
	authAPIKey
	authOAuthToken
)

type auth struct {
	kind authKind
	vars []string // passed to every agent run (federation adds a fresh token per run)
}

// agentEnv returns the credential variables for one agent run. Federation
// mints a fresh GitHub OIDC token each time: a token's jti can be exchanged
// only once, and every claude process exchanges its own.
func (e *env) agentEnv() []string {
	e.t.Helper()
	vars := append([]string{}, e.auth.vars...)
	if e.auth.kind != authFederation {
		return vars
	}
	audience := envOr("ANTHROPIC_OIDC_AUDIENCE", "https://api.anthropic.com")
	req, err := http.NewRequest(http.MethodGet, os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")+"&audience="+audience, nil)
	must(e.t, err)
	req.Header.Set("Authorization", "bearer "+os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	must(e.t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tok struct {
		Value string `json:"value"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &tok) != nil || tok.Value == "" {
		e.t.Fatalf("minting a GitHub OIDC token failed: HTTP %d", resp.StatusCode)
	}
	e.addSecret(tok.Value)
	path := filepath.Join(e.root, fmt.Sprintf("oidc-%d.jwt", time.Now().UnixNano()))
	must(e.t, os.WriteFile(path, []byte(tok.Value), 0o600))
	return append(vars, "ANTHROPIC_IDENTITY_TOKEN_FILE="+path)
}

// --- Claude Code ---------------------------------------------------------------------

type claudeCode struct{}

func (claudeCode) name() string           { return "claude-code" }
func (claudeCode) projectDir() string     { return ".claude" }
func (claudeCode) versionVar() string     { return "E2E_CLAUDE_VERSION" }
func (claudeCode) installFlags() []string { return nil }
func (claudeCode) prepare(*env)           {}

// install installs Claude Code at the requested version or dist-tag.
func (claudeCode) install(e *env, version string) {
	e.t.Helper()
	prefix := filepath.Join(e.root, "claude")
	e.run(e.root, nil, "npm", "install", "--prefix", prefix, "--no-audit", "--no-fund", "--loglevel=error",
		"@anthropic-ai/claude-code@"+version)
	e.agentBin = filepath.Join(prefix, "node_modules", ".bin", "claude")
	e.version = strings.TrimSpace(e.run(e.root, nil, e.agentBin, "--version"))
	e.t.Logf("Claude Code %s (requested %q)", e.version, version)
}

// detectAuth picks the credentials to give Claude Code, first match wins:
// workload identity federation (as in CI), an API key, or an OAuth token.
func (claudeCode) detectAuth() auth {
	fed := []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID"}
	have := true
	for _, k := range fed {
		have = have && os.Getenv(k) != ""
	}
	if have && os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL") != "" {
		a := auth{kind: authFederation}
		for _, k := range append(fed, "ANTHROPIC_WORKSPACE_ID") {
			if v := os.Getenv(k); v != "" {
				a.vars = append(a.vars, k+"="+v)
			}
		}
		return a
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return auth{kind: authAPIKey, vars: []string{"ANTHROPIC_API_KEY=" + k}}
	}
	if k := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); k != "" {
		return auth{kind: authOAuthToken, vars: []string{"CLAUDE_CODE_OAUTH_TOKEN=" + k}}
	}
	return auth{}
}

// newSessionID returns a random UUIDv4, for --session-id.
func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// run runs `claude -p`, with a session id we choose so sessions map
// one-to-one to swarmia-cli's.
func (claudeCode) run(e *env, step, sessionID, prompt string) agentRun {
	e.t.Helper()
	args := []string{"-p", prompt,
		"--output-format", "stream-json", "--verbose", "--include-hook-events",
		"--permission-mode", "bypassPermissions",
		"--setting-sources", "project",
		"--max-turns", envOr("E2E_MAX_TURNS", "40"),
		"--model", envOr("E2E_MODEL", "sonnet"),
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	} else {
		sessionID = newSessionID()
		args = append(args, "--session-id", sessionID)
	}
	cmd := exec.Command(e.agentBin, args...)
	cmd.Dir = e.workspace
	cmd.Env = append(append([]string{}, e.base...), e.agentEnv()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	e.t.Logf("agent step %q took %s", step, time.Since(start).Round(time.Second))
	e.keepBytes(step+".stream.jsonl", stdout.Bytes())
	e.keepBytes(step+".stderr.txt", stderr.Bytes())

	r := parseStream(stdout.Bytes())
	r.SessionID = sessionID
	if err != nil && r.Result == "" {
		e.t.Fatalf("claude failed before producing a result: %v\nstderr:\n%s", err, truncate(stderr.String(), 4000))
	}
	// An error without any model call cost means the agent never reached the
	// model (failed token exchange, API outage).
	r.ReachedModel = !(r.IsError && r.CostUSD == 0)
	matches, _ := filepath.Glob(filepath.Join(e.root, "claude-config", "projects", "*", sessionID+".jsonl"))
	if len(matches) > 0 {
		r.Transcript = matches[0]
		e.keep(r.Transcript, step+".transcript.jsonl")
	}
	return r
}

// parseStream reads the stream-json output: tool calls, hook failures and the
// final result.
func parseStream(data []byte) agentRun {
	var r agentRun
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m["type"] {
		case "assistant":
			msg, _ := m["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, c := range content {
				if cm, _ := c.(map[string]any); cm["type"] == "tool_use" {
					name, _ := cm["name"].(string)
					r.ToolUses = append(r.ToolUses, name)
				}
			}
		case "hook", "system":
			// Hook lifecycle events: documented both as type "hook" with subtype
			// "response", and as type "system" with subtype "hook_response".
			// Responses only appear for some hooks (e.g. background ones), so
			// this is a bonus check; capture completeness is the real one.
			sub, _ := m["subtype"].(string)
			if m["type"] == "system" && !strings.HasPrefix(sub, "hook") {
				continue
			}
			code, hasCode := m["exit_code"].(float64)
			outcome, _ := m["outcome"].(string)
			if (hasCode && code != 0) || outcome == "failure" || outcome == "error" {
				r.HookErrors = append(r.HookErrors, fmt.Sprintf("%v %v (exit %v, %s): %v", m["hook_event"], m["hook_name"], m["exit_code"], outcome, m["stderr"]))
			}
		case "result":
			r.Result, _ = m["subtype"].(string)
			r.Message, _ = m["result"].(string)
			r.IsError, _ = m["is_error"].(bool)
			r.CostUSD, _ = m["total_cost_usd"].(float64)
			if n, ok := m["num_turns"].(float64); ok {
				r.NumTurns = int(n)
			}
		}
	}
	return r
}

// outputTokens sums output tokens over the transcripts, counting each API
// response once.
func (claudeCode) outputTokens(t *testing.T, transcripts []string) int64 {
	t.Helper()
	seen := map[string]bool{}
	var total int64
	for _, transcript := range transcripts {
		f, err := os.Open(transcript)
		must(t, err)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			var l struct {
				Type      string `json:"type"`
				RequestID string `json:"requestId"`
				Message   struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage *struct {
						OutputTokens int64 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "assistant" || l.Message.Usage == nil || strings.HasPrefix(l.Message.Model, "<") {
				continue
			}
			key := l.Message.ID + ":" + l.RequestID
			if !seen[key] {
				seen[key] = true
				total += l.Message.Usage.OutputTokens
			}
		}
		f.Close()
	}
	return total
}

// writes returns every line the agent put into files through its edit tools,
// from the transcript's tool inputs.
func (claudeCode) writes(t *testing.T, transcript string) []string {
	t.Helper()
	f, err := os.Open(transcript)
	must(t, err)
	defer f.Close()
	var lines []string
	add := func(s string) { lines = append(lines, strings.Split(s, "\n")...) }
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var l struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					Input struct {
						Content   string `json:"content"`
						NewString string `json:"new_string"`
						Edits     []struct {
							NewString string `json:"new_string"`
						} `json:"edits"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "assistant" {
			continue
		}
		for _, c := range l.Message.Content {
			if c.Type != "tool_use" {
				continue
			}
			add(c.Input.Content)
			add(c.Input.NewString)
			for _, ed := range c.Input.Edits {
				add(ed.NewString)
			}
		}
	}
	return lines
}
