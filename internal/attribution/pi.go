package attribution

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// PiEdits extracts a successful Pi edit or write when the extension captured
// the file before the call. Unknown pre-images are skipped rather than treating
// an overwritten file as entirely AI-authored.
func PiEdits(payload []byte, locate Locator) []Edit {
	var ev struct {
		Name  string `json:"hook_event_name"`
		Tool  string `json:"tool_name"`
		Input struct {
			File    string  `json:"file_path"`
			Content *string `json:"content"`
			Edits   []struct {
				Old string `json:"oldText"`
				New string `json:"newText"`
			} `json:"edits"`
		} `json:"tool_input"`
		Original *string `json:"original_file"`
		Observed *string `json:"after_file"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Name != "PostToolUse" || ev.Original == nil || ev.Observed == nil || !filepath.IsAbs(ev.Input.File) {
		return nil
	}
	root, path, ok := locate(filepath.Clean(ev.Input.File))
	if !ok {
		return nil
	}
	before := *ev.Original
	var after string
	switch ev.Tool {
	case "write":
		if ev.Input.Content == nil {
			return nil
		}
		after = *ev.Input.Content
	case "edit":
		// Pi matches all replacements against the original, not each other.
		// Normalize line endings as Pi does before applying replacements.
		bom := ""
		text := before
		if strings.HasPrefix(text, "\ufeff") {
			bom, text = "\ufeff", strings.TrimPrefix(text, "\ufeff")
		}
		normalize := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n") }
		text = normalize(text)
		type replacement struct {
			start, end int
			text       string
		}
		var replacements []replacement
		for _, edit := range ev.Input.Edits {
			old := normalize(edit.Old)
			if old == "" || strings.Count(text, old) != 1 {
				return nil
			}
			start := strings.Index(text, old)
			replacements = append(replacements, replacement{start, start + len(old), normalize(edit.New)})
		}
		if len(replacements) == 0 {
			return nil
		}
		sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
		var b strings.Builder
		pos := 0
		for _, r := range replacements {
			if r.start < pos {
				return nil
			}
			b.WriteString(text[pos:r.start])
			b.WriteString(r.text)
			pos = r.end
		}
		b.WriteString(text[pos:])
		after = b.String()
		if i := strings.Index(before, "\n"); i > 0 && before[i-1] == '\r' {
			after = strings.ReplaceAll(after, "\n", "\r\n")
		}
		after = bom + after
	default:
		return nil
	}
	// A changed input, stale pre-image, fuzzy match, or concurrent filesystem
	// mutation can make reconstruction unreliable. Never attribute a guess.
	if after != *ev.Observed {
		return nil
	}
	hunks := diffHunks(before, after)
	if len(hunks) == 0 {
		return nil
	}
	return []Edit{{Root: root, Path: path, Hunks: hunks, After: after, AfterKnown: true}}
}
