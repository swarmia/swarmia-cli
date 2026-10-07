// Package agent describes the coding agents swarmia-cli supports: how each is
// named in the event log, where a project configures its hooks, and how its
// hook payloads map onto the one shape the rest of swarmia-cli reads.
package agent

import (
	"encoding/json"
	"path/filepath"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/gitrepo"
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
	// projectHooks are the files in ConfigDir that may configure hooks.
	projectHooks []string
	// hooksAtRepoRoot is set for agents that run hooks in the session's working
	// directory without saying where the project is: their project is the
	// enclosing repository.
	hooksAtRepoRoot bool
	isEditTool      func(name string) bool
	isShellTool     func(name string) bool
	edits           func(payload []byte, root string) []attribution.Edit
}

var ClaudeCode = &Agent{
	Name:         "claude-code",
	Title:        "Claude Code",
	ConfigDir:    ".claude",
	projectHooks: []string{"settings.json", "settings.local.json"},
	isEditTool:   attribution.IsClaudeEditTool,
	isShellTool:  func(name string) bool { return name == "Bash" },
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
	projectHooks:    []string{"hooks.json"},
	hooksAtRepoRoot: true,
	isEditTool:      attribution.IsCodexPatchTool,
	isShellTool:     func(name string) bool { return name == "Bash" },
	edits:           attribution.CodexEdits,
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
}

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
	}
	if json.Unmarshal(payload, &p) != nil {
		return Event{}, false
	}
	ev = Event{Name: p.HookEventName, SessionID: p.SessionID, TranscriptPath: p.TranscriptPath,
		Model: p.Model, Prompt: p.Prompt}
	if p.ToolName != "" {
		ev.Tool = Tool{Name: p.ToolName, UseID: p.ToolUseID, File: p.ToolInput.FilePath,
			Edits: a.isEditTool(p.ToolName), Shell: a.isShellTool(p.ToolName)}
		if !ev.Tool.Edits { // an edit tool's command, if any, is its patch
			ev.Tool.Command = string(p.ToolInput.Command)
		}
	}
	return ev, true
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

// ProjectHooksFiles are the files where the project a hook runs in (in dir)
// may configure the agent's hooks. For Claude Code, dir is the project
// directory it passes to hooks.
func (a *Agent) ProjectHooksFiles(dir string) []string {
	if a.hooksAtRepoRoot {
		repo, err := gitrepo.Find(dir)
		if err != nil {
			return nil
		}
		dir = repo.Root
	}
	var files []string
	for _, name := range a.projectHooks {
		files = append(files, filepath.Join(dir, a.ConfigDir, name))
	}
	return files
}
