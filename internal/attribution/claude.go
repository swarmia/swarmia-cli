package attribution

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Edit is a change an agent made to one file: the file's path relative to the
// repository root (slash-separated) and the hunks it wrote.
type Edit struct {
	Path  string
	Hunks []Hunk
	// After is the whole file once the edit was applied, when the payload
	// carries enough to rebuild it (AfterKnown).
	After      string
	AfterKnown bool
}

type claudeReplacement struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// claudeToolEvent is the part of a Claude Code PostToolUse payload that
// describes a file edit.
type claudeToolEvent struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		FilePath string  `json:"file_path"`
		Content  *string `json:"content"` // Write
		claudeReplacement
		Edits []claudeReplacement `json:"edits"` // MultiEdit
	} `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

type claudeToolResponse struct {
	OriginalFile *string `json:"originalFile"`
}

// ClaudeEdit extracts the edit from a Claude Code hook payload. ok is false for
// anything that isn't an Edit, MultiEdit or Write of a file inside root.
//
// It rebuilds the file before and after the tool call from the response's
// originalFile and the tool input, and diffs the two. (The response also has a
// structuredPatch, but it renders tabs as spaces, so it isn't verbatim.)
// Without originalFile it falls back to the input alone: a Write's whole
// content, or each old_string → new_string diffed on its own.
func ClaudeEdit(payload []byte, root string) (Edit, bool) {
	var ev claudeToolEvent
	if err := json.Unmarshal(payload, &ev); err != nil || ev.HookEventName != "PostToolUse" {
		return Edit{}, false
	}
	in := ev.ToolInput
	var edits []claudeReplacement
	switch {
	case ev.ToolName == "Write" && in.Content != nil:
	case ev.ToolName == "Edit":
		edits = []claudeReplacement{in.claudeReplacement}
	case ev.ToolName == "MultiEdit":
		edits = in.Edits
	default:
		return Edit{}, false
	}
	path, ok := relPath(root, in.FilePath)
	if !ok {
		return Edit{}, false
	}

	var resp claudeToolResponse
	_ = json.Unmarshal(ev.ToolResponse, &resp) // a failed tool's response is a plain string
	edit := Edit{Path: path}
	switch {
	case in.Content != nil:
		before := ""
		if resp.OriginalFile != nil {
			before = *resp.OriginalFile
		}
		edit.Hunks = diffHunks(before, *in.Content)
		edit.After, edit.AfterKnown = *in.Content, true
	case resp.OriginalFile != nil:
		if after, ok := applyReplacements(*resp.OriginalFile, edits); ok {
			edit.Hunks = diffHunks(*resp.OriginalFile, after)
			edit.After, edit.AfterKnown = after, true
			break
		}
		fallthrough
	default:
		for _, e := range edits {
			edit.Hunks = append(edit.Hunks, diffHunks(e.OldString, e.NewString)...)
		}
	}
	return edit, len(edit.Hunks) > 0
}

// applyReplacements replays Edit/MultiEdit replacements on the original file.
// It fails if any old_string isn't present, i.e. the payload doesn't describe
// this file state.
func applyReplacements(text string, edits []claudeReplacement) (string, bool) {
	for _, e := range edits {
		if e.OldString == "" || !strings.Contains(text, e.OldString) {
			return "", false
		}
		n := 1
		if e.ReplaceAll {
			n = -1
		}
		text = strings.Replace(text, e.OldString, e.NewString, n)
	}
	return text, true
}

// relPath returns path relative to root, slash-separated, if it lies inside.
func relPath(root, path string) (string, bool) {
	if root == "" || path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// diffContext is how many unchanged lines surround each change in a hunk, as in
// a unified diff. They anchor short changes made of generic lines.
const diffContext = 3

// maxDiffCells caps the LCS table; beyond it, the changed middle of the file is
// treated as entirely rewritten.
const maxDiffCells = 4_000_000

// diffHunks diffs two versions of a text by line and returns the new side as
// hunks: each run of added lines with up to diffContext unchanged lines around
// it, runs closer than 2*diffContext merged, as in a unified diff.
func diffHunks(before, after string) []Hunk {
	a, b := splitLines(before), splitLines(after)

	// Edits are local: trim the common prefix and suffix, diff only the middle.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	added := make([]bool, len(b))
	for j, isAdded := range middleAdded(a[pre:len(a)-suf], b[pre:len(b)-suf]) {
		added[pre+j] = isAdded
	}

	var hunks []Hunk
	for j := 0; j < len(b); j++ {
		if !added[j] {
			continue
		}
		// Extend the run while the next added line is close enough that the two
		// hunks' context would touch.
		last := j
		for k := j + 1; k < len(b) && k-last <= 2*diffContext; k++ {
			if added[k] {
				last = k
			}
		}
		start, end := max(j-diffContext, 0), min(last+1+diffContext, len(b))
		var h Hunk
		for k := start; k < end; k++ {
			h.Lines = append(h.Lines, HunkLine{Text: b[k], Added: added[k]})
		}
		hunks = append(hunks, h)
		j = end - 1
	}
	return hunks
}

// middleAdded marks each line of b that isn't part of the longest common
// subsequence with a.
func middleAdded(a, b []string) []bool {
	added := make([]bool, len(b))
	if len(a) == 0 || (len(a)+1)*(len(b)+1) > maxDiffCells {
		for j := range added {
			added[j] = true
		}
		return added
	}
	// lcs[i*w+j] = LCS length of a[i:] and b[j:].
	w := len(b) + 1
	lcs := make([]int32, (len(a)+1)*w)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}
	for i, j := 0, 0; j < len(b); {
		switch {
		case i < len(a) && a[i] == b[j]:
			i, j = i+1, j+1
		case i < len(a) && lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			i++
		default:
			added[j] = true
			j++
		}
	}
	return added
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
