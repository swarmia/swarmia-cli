//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copilot is GitHub Copilot CLI, run with `copilot -p`.
type copilot struct{}

func (copilot) name() string { return "copilot" }

// projectDir is where its project hooks live; install detects Copilot by it.
func (copilot) projectDir() string     { return ".github/hooks" }
func (copilot) versionVar() string     { return "E2E_COPILOT_VERSION" }
func (copilot) model() string          { return os.Getenv("E2E_COPILOT_MODEL") }
func (copilot) installFlags() []string { return nil }
func (copilot) prepare(*env)           {}

// install installs Copilot CLI at the requested version or dist-tag (latest is
// the stable channel, prerelease the development one).
func (copilot) install(e *env, version string) {
	e.t.Helper()
	prefix := filepath.Join(e.root, "copilot")
	e.run(e.root, nil, "npm", "install", "--prefix", prefix, "--no-audit", "--no-fund", "--loglevel=error",
		"@github/copilot@"+version)
	e.agentBin = filepath.Join(prefix, "node_modules", ".bin", "copilot")
	// "GitHub Copilot CLI 1.0.112.", then a hint to update.
	out := strings.Split(strings.TrimSpace(e.run(e.root, nil, e.agentBin, "--version")), "\n")
	e.version = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(out[0]), "GitHub Copilot CLI "), ".")
	e.t.Logf("Copilot CLI %s (requested %q)", e.version, version)
}

// detectAuth: a GitHub token for an account with Copilot (`gh auth token`
// works), given to Copilot as COPILOT_GITHUB_TOKEN, which it reads first.
func (copilot) detectAuth() auth {
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return auth{kind: authAPIKey, vars: []string{"COPILOT_GITHUB_TOKEN=" + v}}
		}
	}
	return auth{}
}

// run runs `copilot -p --output-format json` with an explicit --session-id, or
// --resume to continue a session. COPILOT_ALLOW_ALL trusts the fixture's
// folder (so its hooks run) and allows every tool.
func (copilot) run(e *env, step, sessionID, prompt string) agentRun {
	e.t.Helper()
	args := []string{"-p", prompt, "--output-format", "json", "--no-auto-update"}
	if sessionID == "" {
		sessionID = newSessionID()
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume="+sessionID)
	}
	if m := os.Getenv("E2E_COPILOT_MODEL"); m != "" {
		args = append(args, "--model", m)
	}
	cmd := exec.Command(e.agentBin, args...)
	cmd.Dir = e.workspace
	cmd.Env = append(append([]string{}, e.base...), append(e.agentEnv(), "COPILOT_ALLOW_ALL=true")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	e.t.Logf("agent step %q took %s", step, time.Since(start).Round(time.Second))
	e.keepBytes(step+".stream.jsonl", stdout.Bytes())
	e.keepBytes(step+".stderr.txt", stderr.Bytes())

	r := parseCopilotStream(stdout.Bytes())
	if r.SessionID == "" {
		r.SessionID = sessionID
	}
	if !r.ReachedModel && r.Message == "" {
		r.Message = truncate(strings.TrimSpace(stderr.String()+" "+errString(err)), 4000)
	}
	transcript := filepath.Join(e.home, ".copilot", "session-state", r.SessionID, "events.jsonl")
	if fileExists(transcript) {
		r.Transcript = transcript
		e.keep(transcript, step+".transcript.jsonl")
		r.HookErrors = copilotHookErrors(transcript)
	}
	return r
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// copilotToolNames maps Copilot's own tool names, in its output stream, to
// the names its hooks report.
var copilotToolNames = map[string]string{
	"bash": "Bash", "powershell": "Bash",
	"edit": "Edit", "str_replace_editor": "Edit", "apply_patch": "Edit",
	"create": "Write", "view": "Read", "grep": "Grep", "rg": "Grep", "glob": "Glob", "task": "Agent",
}

// parseCopilotStream reads `copilot -p --output-format json`: the session id,
// tool calls, the final reply and how the run ended.
func parseCopilotStream(stdout []byte) agentRun {
	var r agentRun
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var m struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			ExitCode  *int   `json:"exitCode"`
			AgentID   string `json:"agentId"`
			Data      struct {
				SessionID string `json:"sessionId"`
				ToolName  string `json:"toolName"`
				Content   string `json:"content"`
				Message   string `json:"message"`
				ErrorType string `json:"errorType"`
			} `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "session.start":
			r.SessionID = m.Data.SessionID
		case "tool.execution_start":
			name := m.Data.ToolName
			if n, ok := copilotToolNames[name]; ok {
				name = n
			}
			r.ToolUses = append(r.ToolUses, name)
		case "assistant.message":
			r.ReachedModel = true
			if m.AgentID == "" && strings.TrimSpace(m.Data.Content) != "" {
				r.Message = m.Data.Content
			}
		case "assistant.turn_end":
			r.NumTurns++
		case "session.error":
			r.IsError, r.Message = true, m.Data.Message
		case "result":
			if m.SessionID != "" {
				r.SessionID = m.SessionID
			}
			r.Result = "success"
			if m.ExitCode != nil && *m.ExitCode != 0 {
				r.IsError, r.Result = true, "failed"
			}
		}
	}
	return r
}

// copilotHookErrors lists the hooks a session's events record as failed.
func copilotHookErrors(transcript string) []string {
	data, err := os.ReadFile(transcript)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		var l struct {
			Type string `json:"type"`
			Data struct {
				HookType string `json:"hookType"`
				Success  *bool  `json:"success"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &l) == nil && l.Type == "hook.end" && l.Data.Success != nil && !*l.Data.Success {
			out = append(out, string(line))
		}
	}
	return out
}

// copilotUsageRecords visits each request's usage in a session's events.
func copilotUsageRecords(t *testing.T, transcript string, visit func(output int64)) {
	t.Helper()
	data, err := os.ReadFile(transcript)
	must(t, err)
	for _, line := range bytes.Split(data, []byte("\n")) {
		var l struct {
			Type string `json:"type"`
			Data struct {
				Usage struct {
					OutputTokens int64 `json:"outputTokens"`
				} `json:"usage"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &l) == nil && l.Type == "session.usage_record" {
			visit(l.Data.Usage.OutputTokens)
		}
	}
}

// outputTokens sums the output tokens of every request in the session's
// events, its subagents' included (a resumed session continues its file).
func (copilot) outputTokens(t *testing.T, transcripts []string) int64 {
	t.Helper()
	var sum int64
	for _, transcript := range transcripts {
		copilotUsageRecords(t, transcript, func(out int64) { sum += out })
	}
	return sum
}

// writes returns the lines the agent wrote with its edit tools, from the tool
// calls in the session's events: create's file_text, edit's new_str, and the
// "+" lines of apply_patch patches.
func (copilot) writes(t *testing.T, transcript string) []string {
	t.Helper()
	data, err := os.ReadFile(transcript)
	must(t, err)
	var lines []string
	split := func(s string) {
		lines = append(lines, strings.Split(strings.TrimSuffix(s, "\n"), "\n")...)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var l struct {
			Type string `json:"type"`
			Data struct {
				ToolName  string          `json:"toolName"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "tool.execution_start" {
			continue
		}
		var patch string
		if json.Unmarshal(l.Data.Arguments, &patch) == nil {
			for _, p := range strings.Split(patch, "\n") {
				if strings.HasPrefix(p, "+") {
					lines = append(lines, p[1:])
				}
			}
			continue
		}
		var in struct {
			FileText *string `json:"file_text"`
			NewStr   *string `json:"new_str"`
			Input    string  `json:"input"`
			Patch    string  `json:"patch"`
		}
		json.Unmarshal(l.Data.Arguments, &in)
		switch {
		case in.FileText != nil:
			split(*in.FileText)
		case in.NewStr != nil:
			split(*in.NewStr)
		}
		for _, p := range strings.Split(in.Input+"\n"+in.Patch, "\n") {
			if strings.HasPrefix(p, "+") {
				lines = append(lines, p[1:])
			}
		}
	}
	return lines
}
