package attribution

import (
	"reflect"
	"testing"
)

// addedLines lists each edit's file and the lines its hunks add.
func addedLines(edits []Edit) map[string][]string {
	out := map[string][]string{}
	for _, e := range edits {
		for _, h := range e.Hunks {
			for _, l := range h.Lines {
				if l.Added {
					out[e.Path] = append(out[e.Path], l.Text)
				}
			}
		}
	}
	return out
}

// The payloads are as Copilot CLI 1.0.112 sent them, under PascalCase hooks.
func TestCopilotEdits(t *testing.T) {
	locate := Within("/r")
	for _, tc := range []struct {
		name    string
		payload string
		want    map[string][]string
		after   string // the Write's file, whole
	}{
		{"edit: old_str to new_str",
			`{"hook_event_name":"PostToolUse","cwd":"/r","tool_use_id":"toolu_1","tool_name":"Edit","tool_input":{"path":"/r/a.txt","old_str":"line2","new_str":"LINE TWO"},"tool_result":{"result_type":"success","text_result_for_llm":"File /r/a.txt updated with changes."}}`,
			map[string][]string{"a.txt": {"LINE TWO"}}, ""},
		{"create: a new file, whole",
			`{"hook_event_name":"PostToolUse","cwd":"/r","tool_name":"Write","tool_input":{"path":"/r/b.txt","file_text":"hello\nworld\n"},"tool_result":{"result_type":"success"}}`,
			map[string][]string{"b.txt": {"hello", "world"}}, "hello\nworld\n"},
		{"apply_patch: a bare string, paths absolute or relative to cwd",
			`{"hook_event_name":"PostToolUse","cwd":"/r","tool_name":"Edit","tool_input":"*** Begin Patch\n*** Update File: /r/a.txt\n@@\n line1\n-line2\n+LINE TWO\n line3\n*** Add File: sub/e.txt\n+sub\n*** End Patch\n","tool_result":{"result_type":"success"}}`,
			map[string][]string{"a.txt": {"LINE TWO"}, "sub/e.txt": {"sub"}}, ""},
		{"failed call",
			`{"hook_event_name":"PostToolUseFailure","cwd":"/r","tool_name":"Write","tool_input":{"path":"/r/a.txt","file_text":"x"},"error":"Path already exists"}`,
			map[string][]string{}, ""},
		{"outside the worktree",
			`{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"path":"/elsewhere/a.txt","file_text":"x"}}`,
			map[string][]string{}, ""},
		{"not an edit tool",
			`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"path":"/r/a.txt"}}`,
			map[string][]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edits := CopilotEdits([]byte(tc.payload), locate)
			if got := addedLines(edits); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("added lines = %q, want %q", got, tc.want)
			}
			if tc.after != "" && (len(edits) != 1 || !edits[0].AfterKnown || edits[0].After != tc.after) {
				t.Errorf("edits = %+v, want the whole file after", edits)
			}
			for _, e := range edits {
				if e.Root != "/r" {
					t.Errorf("root = %q", e.Root)
				}
			}
		})
	}
}
