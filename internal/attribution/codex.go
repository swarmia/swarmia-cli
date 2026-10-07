package attribution

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// IsCodexPatchTool reports whether a Codex tool name is its file-editing
// tool. apply_patch is the canonical name; Write and Edit are aliases Codex
// accepts in hook matchers, and are taken as such should it report them.
func IsCodexPatchTool(name string) bool {
	switch name {
	case "apply_patch", "functions.apply_patch", "Write", "Edit":
		return true
	}
	return false
}

// codexToolEvent is the part of a Codex PostToolUse payload that describes a
// patch. The patch is text in apply_patch's own format, under "command".
type codexToolEvent struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	CWD           string          `json:"cwd"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

// CodexPatch returns the patch text in a Codex apply_patch tool input:
// {"command": "<patch>"}, or {"patch"|"input": …}, or the patch as a bare
// JSON string.
func CodexPatch(toolInput json.RawMessage) string {
	var s string
	if json.Unmarshal(toolInput, &s) == nil {
		return s
	}
	var in struct {
		Command json.RawMessage `json:"command"`
		Patch   string          `json:"patch"`
		Input   string          `json:"input"`
	}
	if json.Unmarshal(toolInput, &in) != nil {
		return ""
	}
	if json.Unmarshal(in.Command, &s) == nil && s != "" {
		return s
	}
	// An argv, as in ["apply_patch", "<patch>"].
	var argv []string
	if json.Unmarshal(in.Command, &argv) == nil {
		for _, a := range argv {
			if strings.Contains(a, "*** Begin Patch") {
				return a
			}
		}
	}
	if in.Patch != "" {
		return in.Patch
	}
	return in.Input
}

// CodexEdits extracts the edits from a Codex apply_patch payload, one per file
// the patch adds or updates inside root. Paths in the patch are relative to
// the session's working directory (cwd in the payload) unless absolute.
//
// Each update chunk carries its context, removed and added lines, which give
// the chunk's text before and after; those are diffed like an Edit tool's
// old_string and new_string. Codex applies chunks with some fuzz (trailing
// whitespace and the like), which doesn't matter for matching lines later.
func CodexEdits(payload []byte, root string) []Edit {
	var ev codexToolEvent
	if json.Unmarshal(payload, &ev) != nil || ev.HookEventName != "PostToolUse" || !IsCodexPatchTool(ev.ToolName) {
		return nil
	}
	var out []Edit
	for _, f := range ParsePatch(CodexPatch(ev.ToolInput)) {
		path := f.Path
		if !filepath.IsAbs(path) {
			if ev.CWD == "" {
				continue
			}
			path = filepath.Join(ev.CWD, path)
		}
		rel, ok := relPath(root, filepath.Clean(path))
		if !ok {
			continue
		}
		e := Edit{Path: rel}
		for _, c := range f.Chunks {
			e.Hunks = append(e.Hunks, diffHunks(strings.Join(c.Before, "\n"), strings.Join(c.After, "\n"))...)
		}
		if len(e.Hunks) > 0 {
			out = append(out, e)
		}
	}
	return out
}

// PatchFile is one file an apply_patch patch adds or updates. A new file is a
// single chunk with nothing before.
type PatchFile struct {
	Path   string // where the file ends up (a move's destination)
	Chunks []PatchChunk
}

// PatchChunk is one @@ section: the lines it covers before and after.
type PatchChunk struct {
	Before, After []string
}

// ParsePatch reads apply_patch's format:
//
//	*** Begin Patch
//	*** Add File: <path>          followed by +lines
//	*** Update File: <path>       optionally *** Move to: <path>, then chunks:
//	@@ [context header]           " " context, "-" removed, "+" added lines
//	*** Delete File: <path>
//	*** End Patch
//
// Deleted files are left out: they hold no lines anyone wrote.
func ParsePatch(patch string) []PatchFile {
	var files []PatchFile
	var cur *PatchFile
	var chunk *PatchChunk
	flushChunk := func() {
		if cur != nil && chunk != nil && (len(chunk.Before) > 0 || len(chunk.After) > 0) {
			cur.Chunks = append(cur.Chunks, *chunk)
		}
		chunk = nil
	}
	flushFile := func() {
		flushChunk()
		if cur != nil && len(cur.Chunks) > 0 {
			files = append(files, *cur)
		}
		cur = nil
	}
	adding := false
	for _, line := range strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			flushFile()
			cur, chunk, adding = &PatchFile{Path: strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))}, &PatchChunk{}, true
		case strings.HasPrefix(line, "*** Update File: "):
			flushFile()
			cur, adding = &PatchFile{Path: strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))}, false
		case strings.HasPrefix(line, "*** Delete File: "):
			flushFile()
		case strings.HasPrefix(line, "*** Move to: "):
			if cur != nil {
				cur.Path = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to: "))
			}
		case strings.HasPrefix(line, "***"): // Begin/End Patch, End of File
			if strings.HasPrefix(line, "*** End Patch") {
				flushFile()
			}
		case cur == nil:
		case adding:
			if strings.HasPrefix(line, "+") {
				chunk.After = append(chunk.After, line[1:])
			}
		case strings.HasPrefix(line, "@@"):
			flushChunk()
			chunk = &PatchChunk{}
		default:
			if chunk == nil { // the first chunk may come without @@
				chunk = &PatchChunk{}
			}
			switch {
			case strings.HasPrefix(line, "+"):
				chunk.After = append(chunk.After, line[1:])
			case strings.HasPrefix(line, "-"):
				chunk.Before = append(chunk.Before, line[1:])
			case strings.HasPrefix(line, " "):
				chunk.Before = append(chunk.Before, line[1:])
				chunk.After = append(chunk.After, line[1:])
			case line == "": // a blank context line, its space stripped
				chunk.Before = append(chunk.Before, "")
				chunk.After = append(chunk.After, "")
			}
		}
	}
	flushFile()
	return files
}
