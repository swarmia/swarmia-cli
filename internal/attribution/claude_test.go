package attribution

import (
	"encoding/json"
	"reflect"
	"testing"
)

// payload builds a Claude Code PostToolUse payload.
func payload(t *testing.T, tool string, input map[string]any, originalFile any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse",
		"tool_name":       tool,
		"tool_input":      input,
		"tool_response":   map[string]any{"originalFile": originalFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// render flattens hunks to strings: "+" for added lines, " " for context.
func render(hunks []Hunk) [][]string {
	var out [][]string
	for _, h := range hunks {
		var lines []string
		for _, l := range h.Lines {
			prefix := " "
			if l.Added {
				prefix = "+"
			}
			lines = append(lines, prefix+l.Text)
		}
		out = append(out, lines)
	}
	return out
}

func TestClaudeEditRebuildsFromOriginalFile(t *testing.T) {
	orig := "a\nb\nc\nd\ne\nf\ng\nh\n"
	p := payload(t, "Edit", map[string]any{
		"file_path": "/repo/src/x.txt", "old_string": "d\n", "new_string": "D1\nD2\n",
	}, orig)
	edit, ok := ClaudeEdit(p, Within("/repo"))
	if !ok || edit.Path != "src/x.txt" {
		t.Fatalf("got %+v, %v", edit, ok)
	}
	want := [][]string{{" a", " b", " c", "+D1", "+D2", " e", " f", " g"}}
	if got := render(edit.Hunks); !reflect.DeepEqual(got, want) {
		t.Errorf("hunks = %q, want %q", got, want)
	}
}

func TestClaudeEditMultiEditReplaysInOrder(t *testing.T) {
	p := payload(t, "MultiEdit", map[string]any{
		"file_path": "/repo/x.go",
		"edits": []map[string]any{
			{"old_string": "one", "new_string": "uno"},
			{"old_string": "uno two", "new_string": "uno dos"}, // depends on the first edit
		},
	}, "one two\n")
	edit, ok := ClaudeEdit(p, Within("/repo"))
	if want := [][]string{{"+uno dos"}}; !ok || !reflect.DeepEqual(render(edit.Hunks), want) {
		t.Errorf("got %q, %v; want %q", render(edit.Hunks), ok, want)
	}
}

// Edit's replace_all replaces every occurrence, so every changed line is added.
func TestClaudeEditReplaceAll(t *testing.T) {
	p := payload(t, "Edit", map[string]any{
		"file_path": "/repo/x.go", "old_string": "foo", "new_string": "bar", "replace_all": true,
	}, "foo()\nkeep\nfoo()\n")
	edit, _ := ClaudeEdit(p, Within("/repo"))
	if want := [][]string{{"+bar()", " keep", "+bar()"}}; !reflect.DeepEqual(render(edit.Hunks), want) {
		t.Errorf("got %q, want %q", render(edit.Hunks), want)
	}
}

func TestClaudeEditWriteCreateIsAllAdded(t *testing.T) {
	p := payload(t, "Write", map[string]any{"file_path": "/repo/new.go", "content": "package x\n\nfunc F() {}\n"}, nil)
	edit, _ := ClaudeEdit(p, Within("/repo"))
	if want := [][]string{{"+package x", "+", "+func F() {}"}}; !reflect.DeepEqual(render(edit.Hunks), want) {
		t.Errorf("got %q, want %q", render(edit.Hunks), want)
	}
}

// Without a usable originalFile (or when old_string isn't in it), the input is
// diffed on its own.
func TestClaudeEditFallsBackToInputDiff(t *testing.T) {
	in := map[string]any{"file_path": "/repo/x.go", "old_string": "a\nb\nc", "new_string": "a\nB\nc"}
	want := [][]string{{" a", "+B", " c"}}
	for name, orig := range map[string]any{"no originalFile": nil, "stale originalFile": "zzz\n"} {
		edit, ok := ClaudeEdit(payload(t, "Edit", in, orig), Within("/repo"))
		if !ok || !reflect.DeepEqual(render(edit.Hunks), want) {
			t.Errorf("%s: got %q, %v; want %q", name, render(edit.Hunks), ok, want)
		}
	}
}

func TestClaudeEditIgnoresIrrelevantPayloads(t *testing.T) {
	for name, p := range map[string][]byte{
		"outside root": payload(t, "Write", map[string]any{"file_path": "/home/me/.claude/plans/p.md", "content": "x"}, nil),
		"sibling dir":  payload(t, "Write", map[string]any{"file_path": "/repository/x.go", "content": "x"}, nil),
		"other tool":   payload(t, "Bash", map[string]any{"command": "ls"}, nil),
		"pre tool use": []byte(`{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"/repo/x","content":"x"}}`),
		"not json":     []byte(`nope`),
	} {
		if edit, ok := ClaudeEdit(p, Within("/repo")); ok {
			t.Errorf("%s: extracted %+v", name, edit)
		}
	}
}

// Changes far apart become separate hunks; nearby ones share one, as in a
// unified diff with three lines of context.
func TestDiffHunksGrouping(t *testing.T) {
	before := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\n20\n"
	after := "1\nTWO\n3\n4\n5\n6\n7\nEIGHT\n9\n10\n11\n12\n13\n14\n15\n16\n17\n18\nNINETEEN\n20\n"
	want := [][]string{
		{" 1", "+TWO", " 3", " 4", " 5", " 6", " 7", "+EIGHT", " 9", " 10", " 11"},
		{" 16", " 17", " 18", "+NINETEEN", " 20"},
	}
	if got := render(diffHunks(before, after)); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := diffHunks(before, before); got != nil {
		t.Errorf("identical texts gave %q", render(got))
	}
}

// An edit is placed where the locator says, and the locator only ever sees
// absolute, cleaned paths: a relative file_path isn't an edit.
func TestClaudeEditUsesLocator(t *testing.T) {
	var asked []string
	locate := func(path string) (string, string, bool) {
		asked = append(asked, path)
		return "/work/lib", "pkg/x.go", true
	}
	p := payload(t, "Write", map[string]any{"file_path": "/work/app/../lib/pkg/x.go", "content": "x\n"}, nil)
	edit, ok := ClaudeEdit(p, locate)
	if !ok || edit.Root != "/work/lib" || edit.Path != "pkg/x.go" {
		t.Errorf("edit = %+v, %v", edit, ok)
	}
	if want := []string{"/work/lib/pkg/x.go"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("locator asked %q, want %q", asked, want)
	}
	if _, ok := ClaudeEdit(payload(t, "Write", map[string]any{"file_path": "pkg/x.go", "content": "x\n"}, nil), locate); ok {
		t.Error("relative file_path made an edit")
	}
}
