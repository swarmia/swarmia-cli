package attribution

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestPiEdits(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.go")
	for _, tc := range []struct {
		name, tool, hook string
		original         any
		input            map[string]any
		want             string
		count            int
	}{
		{"multiple edits use original", "edit", "PostToolUse", "alpha beta gamma\n", map[string]any{"edits": []map[string]string{{"oldText": "alpha", "newText": "beta"}, {"oldText": "beta", "newText": "delta"}}}, "beta delta gamma\n", 1},
		{"bom and crlf", "edit", "PostToolUse", "\ufeffhello\r\nworld\r\n", map[string]any{"edits": []map[string]string{{"oldText": "hello\nworld", "newText": "hello\nPi"}}}, "\ufeffhello\r\nPi\r\n", 1},
		{"write existing", "write", "PostToolUse", "before\n", map[string]any{"content": "after\n"}, "after\n", 1},
		{"new file", "write", "PostToolUse", "", map[string]any{"content": "new\n"}, "new\n", 1},
		{"unknown before", "write", "PostToolUse", nil, map[string]any{"content": "new\n"}, "", 0},
		{"failed", "write", "PostToolUseFailure", "old", map[string]any{"content": "new"}, "", 0},
		{"ambiguous", "edit", "PostToolUse", "a a", map[string]any{"edits": []map[string]string{{"oldText": "a", "newText": "b"}}}, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.input["file_path"] = path
			data, _ := json.Marshal(map[string]any{"hook_event_name": tc.hook, "tool_name": tc.tool, "original_file": tc.original, "after_file": tc.want, "tool_input": tc.input})
			edits := PiEdits(data, Within(root))
			if len(edits) != tc.count {
				t.Fatalf("got %d edits, want %d", len(edits), tc.count)
			}
			if len(edits) > 0 && (!edits[0].AfterKnown || edits[0].After != tc.want) {
				t.Fatalf("after = %q, want %q", edits[0].After, tc.want)
			}
		})
	}
	stale, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "write", "original_file": "old", "after_file": "concurrent", "tool_input": map[string]any{"file_path": path, "content": "new"}})
	if got := PiEdits(stale, Within(root)); len(got) != 0 {
		t.Fatal("stale post-image was attributed")
	}
}
