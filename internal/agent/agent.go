// Package agent describes the coding agents swarmia-cli supports: how each is
// named in the event log, where a project configures its hooks, and how its
// hook payloads map onto the one shape the rest of swarmia-cli reads.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/codex"
	"github.com/swarmia/swarmia-cli/internal/usage"
)

// Agent is one supported coding agent.
type Agent struct {
	// Name identifies the agent in the event log, in session payloads and in
	// log-hook's --agent flag.
	Name string
	// Title is its name for people.
	Title string
	// ConfigDir is the agent's directory in a project, e.g. ".claude".
	ConfigDir string
	// hooksFiles names the file that configures hooks in each scope the agent
	// has: in ConfigDir for project and local scope, in userDir for user scope.
	hooksFiles map[Scope]string
	userDir    func() (string, error)
	// Transcript is the format of the agent's session transcripts, which
	// hold its token usage.
	Transcript usage.Format
	// hooksAtRepoRoot is set for agents that run hooks in the session's working
	// directory without saying where the project is: their project is the
	// enclosing repository.
	hooksAtRepoRoot bool
	// reportsFailures is set for agents that send failed tool calls as
	// PostToolUseFailure, so a PostToolUse is a success. Other agents send both
	// as PostToolUse.
	reportsFailures bool
	// injectedPrompt matches a prompt the agent submitted itself, such as a
	// background task's notification, which its prompt hook also sees.
	injectedPrompt *regexp.Regexp
	isEditTool     func(name string) bool
	isShellTool    func(name string) bool
	edits          func(payload []byte, root string) []attribution.Edit
}

var ClaudeCode = &Agent{
	Name:            "claude-code",
	Title:           "Claude Code",
	ConfigDir:       ".claude",
	hooksFiles:      map[Scope]string{ScopeProject: "settings.json", ScopeLocal: "settings.local.json", ScopeUser: "settings.json"},
	userDir:         claudeConfigDir,
	Transcript:      usage.ClaudeTranscript,
	reportsFailures: true,
	injectedPrompt:  claudeInjectedPrompt,
	isEditTool:      attribution.IsClaudeEditTool,
	isShellTool:     func(name string) bool { return name == "Bash" },
	edits: func(payload []byte, root string) []attribution.Edit {
		if e, ok := attribution.ClaudeEdit(payload, root); ok {
			return []attribution.Edit{e}
		}
		return nil
	},
}

var Codex = &Agent{
	Name:            "codex",
	Title:           "Codex",
	ConfigDir:       ".codex",
	hooksFiles:      map[Scope]string{ScopeProject: codex.HooksFile, ScopeUser: codex.HooksFile},
	userDir:         codex.Home,
	Transcript:      usage.CodexRollout,
	hooksAtRepoRoot: true,
	isEditTool:      attribution.IsCodexPatchTool,
	isShellTool:     func(name string) bool { return name == "Bash" },
	edits:           attribution.CodexEdits,
}

// claudeInjectedPrompt matches a prompt made only of the blocks Claude Code
// submits for a background task (<task-notification>) or a subagent reporting
// back (<agent-message>).
var claudeInjectedPrompt = regexp.MustCompile(`^\s*(?:<(?:task-notification|agent-message)\b[^>]*>(?s:.*?)</(?:task-notification|agent-message)>\s*)+$`)

// claudeConfigDir is Claude Code's user configuration directory.
func claudeConfigDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// All lists the supported agents.
var All = []*Agent{ClaudeCode, Codex}

// Lookup returns the agent with the given name, or nil.
func Lookup(name string) *Agent {
	for _, a := range All {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// OfRecord returns the agent that sent an event log record, from its agent
// field: empty in records that predate the field, all from Claude Code. It is
// nil for an agent this version doesn't know.
func OfRecord(name string) *Agent {
	if name == "" {
		return ClaudeCode
	}
	return Lookup(name)
}

// Event is a hook payload, in the fields swarmia-cli reads.
type Event struct {
	// Name is the hook event: SessionStart, UserPromptSubmit, PreToolUse,
	// PostToolUse, PostToolUseFailure, Stop, SessionEnd.
	Name           string
	SessionID      string
	TranscriptPath string
	Model          string
	Prompt         string
	// Injected is set for a prompt the agent submitted itself, not the person.
	Injected bool
	// Tool is the tool call a tool event is about.
	Tool Tool
}

// Tool is a tool call.
type Tool struct {
	Name  string
	UseID string
	// File is the file the call works on, when its input names one.
	File string
	// Command is the shell command it runs, if any.
	Command string
	// Edits is set for the agent's file-editing tools, whose payloads carry
	// the edits (see Agent.Edits).
	Edits bool
	// Shell is set for shell tools, whose changes are found by snapshotting
	// the worktree around the call.
	Shell bool
	// Status is how a finished call ended: StatusOK, StatusFailed or
	// StatusInterrupted, or empty if the agent doesn't say.
	Status string
	// ExitCode is a failed shell command's exit status, when the agent
	// reports it. Successful calls have none: an agent may count some
	// nonzero exits (grep finding nothing) as success.
	ExitCode *int
}

// Tool call statuses.
const (
	StatusOK          = "ok"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// exitCodeRE matches the exit status Claude Code puts at the start of a failed
// shell command's error.
var exitCodeRE = regexp.MustCompile(`^Exit code (-?\d+)`)

// Parse reads a hook payload. ok is false if it isn't a JSON object.
func (a *Agent) Parse(payload []byte) (ev Event, ok bool) {
	var p struct {
		HookEventName  string `json:"hook_event_name"`
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		ToolName       string `json:"tool_name"`
		ToolUseID      string `json:"tool_use_id"`
		ToolInput      struct {
			FilePath string     `json:"file_path"`
			Command  textOrNone `json:"command"`
		} `json:"tool_input"`
		ToolResponse json.RawMessage `json:"tool_response"`
		Error        string          `json:"error"`
		IsInterrupt  bool            `json:"is_interrupt"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return Event{}, false
	}
	ev = Event{Name: p.HookEventName, SessionID: p.SessionID, TranscriptPath: p.TranscriptPath,
		Model: p.Model, Prompt: p.Prompt}
	ev.Injected = a.injectedPrompt != nil && a.injectedPrompt.MatchString(p.Prompt)
	if p.ToolName != "" {
		ev.Tool = Tool{Name: p.ToolName, UseID: p.ToolUseID, File: p.ToolInput.FilePath,
			Edits: a.isEditTool(p.ToolName), Shell: a.isShellTool(p.ToolName)}
		if !ev.Tool.Edits { // an edit tool's command, if any, is its patch
			ev.Tool.Command = string(p.ToolInput.Command)
		}
		if a.reportsFailures {
			ev.Tool.Status, ev.Tool.ExitCode = toolStatus(p.HookEventName, p.ToolResponse, p.Error, p.IsInterrupt, ev.Tool.Shell)
		}
	}
	return ev, true
}

// toolStatus reads how a finished tool call ended from an agent that reports
// failures separately (see Agent.reportsFailures).
func toolStatus(hook string, response json.RawMessage, errText string, interrupt, shell bool) (string, *int) {
	switch hook {
	case "PostToolUse":
		var r struct {
			Interrupted bool `json:"interrupted"`
		}
		if json.Unmarshal(response, &r) == nil && r.Interrupted {
			return StatusInterrupted, nil
		}
		return StatusOK, nil
	case "PostToolUseFailure":
		var code *int
		if m := exitCodeRE.FindStringSubmatch(errText); shell && m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				code = &n
			}
		}
		if interrupt {
			return StatusInterrupted, code
		}
		return StatusFailed, code
	}
	return "", nil
}

// textOrNone is a string field that agents may also send as something else
// (an argv array, say), which reads as empty instead of failing the payload.
type textOrNone string

func (t *textOrNone) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*t = textOrNone(s)
	}
	return nil
}

// Edits extracts the file edits in a PostToolUse payload: the files the
// agent's edit tool changed inside root, and the hunks it wrote. Shell
// commands aren't covered here: their changes come from worktree snapshots.
func (a *Agent) Edits(payload []byte, root string) []attribution.Edit {
	return a.edits(payload, root)
}

// Scope says which of an agent's settings files configures hooks.
type Scope string

const (
	// ScopeProject is the repo's shared, committed settings: hooks for everyone.
	ScopeProject Scope = "project"
	// ScopeLocal is the repo's personal, gitignored settings.
	ScopeLocal Scope = "local"
	// ScopeUser is the user's settings: every project on this machine, including
	// worktrees that lack the repo's untracked local settings.
	ScopeUser Scope = "user"
)

// Scopes lists every scope.
var Scopes = []Scope{ScopeProject, ScopeLocal, ScopeUser}

// NoScopeError is HooksFile's error for a scope the agent doesn't have.
type NoScopeError struct {
	Agent *Agent
	Scope Scope
}

func (e *NoScopeError) Error() string {
	kind := map[Scope]string{ScopeProject: "shared per-project", ScopeLocal: "personal per-project", ScopeUser: "per-user"}[e.Scope]
	return fmt.Sprintf("no %s scope (%s has no %s hooks file)", e.Scope, e.Agent.Title, kind)
}

// HooksFile is the file that configures the agent's hooks in a scope, for the
// project in dir. It fails for a scope the agent doesn't have.
func (a *Agent) HooksFile(dir string, scope Scope) (string, error) {
	name, ok := a.hooksFiles[scope]
	if !ok {
		return "", &NoScopeError{a, scope}
	}
	if scope == ScopeUser {
		home, err := a.userDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, name), nil
	}
	return filepath.Join(dir, a.ConfigDir, name), nil
}

// ProjectHooksFiles are the files where the project a hook runs in may
// configure the agent's hooks: its project and local scope. projectDir is the
// directory the agent passes to hooks (Claude Code's project directory), else
// where the hook runs; repoRoot is the enclosing repository's root, if any.
func (a *Agent) ProjectHooksFiles(projectDir, repoRoot string) []string {
	dir := projectDir
	if a.hooksAtRepoRoot {
		if repoRoot == "" {
			return nil
		}
		dir = repoRoot
	}
	var files []string
	for _, scope := range []Scope{ScopeProject, ScopeLocal} {
		if path, err := a.HooksFile(dir, scope); err == nil {
			files = append(files, path)
		}
	}
	return files
}
