//go:build e2e

// Package e2e runs a real coding agent against a real repository with
// swarmia-cli's hooks installed, and checks what swarmia-cli captured,
// attributed and would send. See docs/e2e.md. Run with `make e2e`.
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
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	fixtureURL  = "https://github.com/unjs/ufo.git"
	fixtureSHA  = "f06c800d0c59f2a4a1b9ba65eb6cb61a84419be6" // 2026-04-29
	fixturePNPM = "pnpm@10.33.2"                             // the fixture's packageManager

	devName  = "E2E Developer"
	devEmail = "dev@e2e.example.com"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// env is one isolated end-to-end run: its own HOME, Claude Code config,
// git config and swarmia-cli state, so nothing touches the developer's own.
type env struct {
	t         *testing.T
	root      string // everything lives under here
	home      string
	repo      string // the developer's clone of the fixture
	origin    string // bare repository standing in for GitHub
	team      string // a teammate's clone
	swarmia   string // swarmia-cli binary
	claude    string // Claude Code binary
	version   string // Claude Code version actually installed
	artifacts string // where to keep logs (empty: don't)
	auth      auth
	base      []string // environment for every command
	secrets   []string // credential values that must never reach the artifacts
	leaked    bool     // a secret was found (and scrubbed) in an artifact
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	e := &env{t: t, root: root, home: filepath.Join(root, "home"), repo: filepath.Join(root, "repo"),
		origin: filepath.Join(root, "origin.git"), team: filepath.Join(root, "team"),
		artifacts: os.Getenv("E2E_ARTIFACTS"), auth: detectAuth()}
	for _, d := range []string{e.home, filepath.Join(root, "claude-config"), filepath.Join(root, "state"), filepath.Join(root, "bin")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	if e.artifacts != "" {
		must(t, os.MkdirAll(e.artifacts, 0o755))
	}
	for _, k := range secretVars {
		e.addSecret(os.Getenv(k))
	}
	t.Cleanup(func() {
		if e.leaked {
			t.Error("a credential reached the artifacts (scrubbed before upload); find out how")
		}
	})
	gitconfig := filepath.Join(root, "gitconfig")
	must(t, os.WriteFile(gitconfig, []byte(fmt.Sprintf(
		"[user]\n\tname = %s\n\temail = %s\n[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n", devName, devEmail)), 0o644))

	// Build the environment from scratch: only what the tools need, plus the
	// chosen credentials. Nothing else from the caller's environment (their
	// Claude Code session, their swarmia-cli settings) leaks in.
	e.base = []string{
		"PATH=" + filepath.Join(root, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + e.home,
		"TMPDIR=" + os.TempDir(),
		"LANG=en_US.UTF-8",
		"CLAUDE_CONFIG_DIR=" + filepath.Join(root, "claude-config"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "xdg-config"),
		"SWARMIA_STATE_DIR=" + filepath.Join(root, "state"),
		"GIT_CONFIG_GLOBAL=" + gitconfig,
		"GIT_CONFIG_NOSYSTEM=1",
		"DISABLE_AUTOUPDATER=1",
		"CI=1",
	}
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// run runs a command in dir with the isolated environment (plus extra
// variables), failing the test on error. Output is returned and, when
// artifacts are kept, appended to commands.log.
func (e *env) run(dir string, extra []string, name string, args ...string) string {
	e.t.Helper()
	out, err := e.try(dir, extra, name, args...)
	if err != nil {
		e.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) try(dir string, extra []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, e.base...), extra...)
	out, err := cmd.CombinedOutput()
	e.logCommand(dir, name, args, out, err)
	return string(out), err
}

func (e *env) logCommand(dir, name string, args []string, out []byte, err error) {
	e.writeArtifact("commands.log", []byte(fmt.Sprintf("$ (cd %s && %s %s)  # err=%v\n%s\n",
		dir, name, strings.Join(args, " "), err, truncate(string(out), 4000))), true)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n…"
	}
	return s
}

// keep copies a file into the artifacts directory, if one is set.
func (e *env) keep(src, name string) {
	if e.artifacts == "" {
		return
	}
	if data, err := os.ReadFile(src); err == nil {
		e.writeArtifact(name, data, false)
	}
}

func (e *env) keepBytes(name string, data []byte) {
	e.writeArtifact(name, data, false)
}

// secretVars are the caller's variables holding credentials. Their values
// (and every OIDC token the suite mints) are scrubbed from artifacts: CI
// uploads those publicly, unmasked, and an agent's transcript or output could
// echo its environment.
var secretVars = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY",
	"ACTIONS_ID_TOKEN_REQUEST_TOKEN"}

func (e *env) addSecret(v string) {
	if len(v) >= 8 { // shorter is no credential, and would scrub innocent text
		e.secrets = append(e.secrets, v)
	}
}

// scrub replaces every known secret in data, and reports whether it found one.
func (e *env) scrub(data []byte) ([]byte, bool) {
	found := false
	for _, s := range e.secrets {
		if bytes.Contains(data, []byte(s)) {
			data, found = bytes.ReplaceAll(data, []byte(s), []byte("[E2E-SCRUBBED]")), true
		}
	}
	return data, found
}

// writeArtifact is the one way into the artifacts directory: it scrubs
// secrets at write time, so even a run killed by a timeout (which skips
// cleanups) leaves nothing to leak.
func (e *env) writeArtifact(name string, data []byte, appendTo bool) {
	if e.artifacts == "" {
		return
	}
	data, found := e.scrub(data)
	if found {
		e.leaked = true
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(filepath.Join(e.artifacts, name), flags, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
}

// --- setup ---------------------------------------------------------------------

// buildSwarmia compiles swarmia-cli from this working tree. It uses the
// caller's own Go environment (module and build caches), not the sandbox:
// compiling our code needs none of the isolation, and Go's read-only module
// cache inside the sandbox would break the temp directory's cleanup.
func (e *env) buildSwarmia() {
	e.t.Helper()
	moduleRoot, err := filepath.Abs("..")
	must(e.t, err)
	e.swarmia = filepath.Join(e.root, "bin", "swarmia-cli")
	cmd := exec.Command("go", "build", "-ldflags", "-X github.com/swarmia/swarmia-cli/cmd.version=e2e", "-o", e.swarmia, ".")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("building swarmia-cli: %v\n%s", err, out)
	}
}

// installClaude installs Claude Code at the requested version or dist-tag.
func (e *env) installClaude(version string) {
	e.t.Helper()
	prefix := filepath.Join(e.root, "claude")
	e.run(e.root, nil, "npm", "install", "--prefix", prefix, "--no-audit", "--no-fund", "--loglevel=error",
		"@anthropic-ai/claude-code@"+version)
	e.claude = filepath.Join(prefix, "node_modules", ".bin", "claude")
	e.version = strings.TrimSpace(e.run(e.root, nil, e.claude, "--version"))
	e.t.Logf("Claude Code %s (requested %q)", e.version, version)
}

// setupFixture fetches the fixture at its pinned commit, creates the bare
// origin and the teammate's clone, installs dependencies and our hooks.
func (e *env) setupFixture() {
	e.t.Helper()
	must(e.t, os.MkdirAll(e.repo, 0o755))
	e.run(e.repo, nil, "git", "init", "-q")
	e.run(e.repo, nil, "git", "remote", "add", "upstream", fixtureURL)
	e.run(e.repo, nil, "git", "fetch", "-q", "--depth", "1", "upstream", fixtureSHA)
	e.run(e.repo, nil, "git", "checkout", "-q", "-b", "main", "FETCH_HEAD")

	e.run(e.root, nil, "git", "clone", "-q", "--bare", e.repo, e.origin)
	e.run(e.repo, nil, "git", "remote", "add", "origin", e.origin)
	e.run(e.repo, nil, "git", "fetch", "-q", "origin")
	e.run(e.repo, nil, "git", "branch", "-q", "--set-upstream-to=origin/main", "main")
	e.run(e.root, nil, "git", "clone", "-q", e.origin, e.team)

	e.run(e.repo, nil, "npx", "-y", fixturePNPM, "install", "--frozen-lockfile", "--reporter=silent")

	// Our hooks go into the project's settings, which Claude Code reads with
	// --setting-sources project; .claude/ is excluded so it's never committed.
	must(e.t, os.MkdirAll(filepath.Join(e.repo, ".claude"), 0o755))
	exclude := filepath.Join(e.repo, ".git", "info", "exclude")
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	must(e.t, err)
	f.WriteString("\n.claude/\n")
	f.Close()
	out := e.run(e.repo, nil, e.swarmia, "install", "--scope", "project", "--dir", e.repo, "--binary", e.swarmia)
	e.t.Logf("swarmia-cli install:\n%s", out)
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

// detectAuth picks the credentials to give Claude Code, first match wins:
// workload identity federation (as in CI), an API key, or an OAuth token.
func detectAuth() auth {
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

// --- the agent -----------------------------------------------------------------------

// agentRun is what one `claude -p` run did, from its output stream.
type agentRun struct {
	SessionID  string
	ToolUses   []string // tool names, in order
	Result     string   // the result subtype, e.g. "success"
	Message    string   // the result text (the agent's answer, or the error)
	IsError    bool
	CostUSD    float64
	NumTurns   int
	HookErrors []string
	Transcript string // path to the session transcript
}

// newSessionID returns a random UUIDv4, for --session-id.
func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// runAgent runs Claude Code headless in the fixture: a new session with the
// given id, or (resume) a further turn of an existing one.
func (e *env) runAgent(step, sessionID, prompt string, resume bool) agentRun {
	e.t.Helper()
	args := []string{"-p", prompt,
		"--output-format", "stream-json", "--verbose", "--include-hook-events",
		"--permission-mode", "bypassPermissions",
		"--setting-sources", "project",
		"--max-turns", envOr("E2E_MAX_TURNS", "40"),
		"--model", envOr("E2E_MODEL", "sonnet"),
	}
	if resume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	cmd := exec.Command(e.claude, args...)
	cmd.Dir = e.repo
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

func init() {
	if runtime.GOOS == "windows" {
		panic("the e2e suite runs on macOS and Linux")
	}
}
