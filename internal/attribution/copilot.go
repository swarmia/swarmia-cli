package attribution

import (
	"encoding/json"
	"path/filepath"
)

// IsCopilotEditTool reports whether a tool name in a GitHub Copilot hook
// payload is one of its file-editing tools. Copilot reports them under Claude
// Code's names: Edit for edit, str_replace_editor and apply_patch, Write for
// create.
func IsCopilotEditTool(name string) bool {
	return name == "Edit" || name == "Write"
}

// copilotToolEvent is the part of a Copilot PostToolUse payload that describes
// a file edit. Its tool_input is either an object, for create and edit, or an
// apply_patch patch as a bare string (as GPT models send it).
type copilotToolEvent struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

type copilotEditInput struct {
	Path     string  `json:"path"`
	FileText *string `json:"file_text"` // create
	OldStr   *string `json:"old_str"`   // edit
	NewStr   *string `json:"new_str"`
}

// CopilotEdits extracts the edits in a Copilot PostToolUse payload:
//
//   - create (Write) writes a new file, file_text; it refuses to overwrite one,
//     so the whole text is new;
//   - edit (Edit) replaces old_str with new_str, diffed on its own, as the
//     payload carries nothing else of the file;
//   - apply_patch (Edit), a patch in Codex's format, whose paths may be
//     relative to the working directory (see CodexEdits).
//
// Failed calls arrive as PostToolUseFailure, and are skipped.
func CopilotEdits(payload []byte, locate Locator) []Edit {
	var ev copilotToolEvent
	if json.Unmarshal(payload, &ev) != nil || ev.HookEventName != "PostToolUse" || !IsCopilotEditTool(ev.ToolName) {
		return nil
	}
	var patch string
	if json.Unmarshal(ev.ToolInput, &patch) == nil {
		return CodexEdits(payload, locate)
	}
	var in copilotEditInput
	if json.Unmarshal(ev.ToolInput, &in) != nil || !filepath.IsAbs(in.Path) {
		return nil
	}
	root, path, ok := locate(filepath.Clean(in.Path))
	if !ok {
		return nil
	}
	e := Edit{Root: root, Path: path}
	switch {
	case in.FileText != nil:
		e.Hunks = diffHunks("", *in.FileText)
		e.After, e.AfterKnown = *in.FileText, true
	case in.NewStr != nil:
		old := ""
		if in.OldStr != nil {
			old = *in.OldStr
		}
		e.Hunks = diffHunks(old, *in.NewStr)
	}
	if len(e.Hunks) == 0 {
		return nil
	}
	return []Edit{e}
}
