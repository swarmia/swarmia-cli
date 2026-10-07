package attribution

import (
	"encoding/json"
	"reflect"
	"testing"
)

const samplePatch = `*** Begin Patch
*** Add File: src/new.ts
+export const answer = 42;
+
+export function double(n: number) {
+  return n * 2;
+}
*** Update File: src/utils.ts
@@ export function hasProtocol(
 const a = 1;
-const b = 2;
+const b = 3;
+const c = 4;
 const d = 5;

@@
 function tail() {
-  return 0;
+  return 1;
 }
*** End of File
*** Update File: old/name.ts
*** Move to: new/name.ts
@@
 keep();
+added();
*** Delete File: src/gone.ts
*** End Patch`

func TestParsePatch(t *testing.T) {
	files := ParsePatch(samplePatch)
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if want := []string{"src/new.ts", "src/utils.ts", "new/name.ts"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if got := files[0].Chunks; len(got) != 1 || len(got[0].Before) != 0 || len(got[0].After) != 5 {
		t.Errorf("added file chunks = %#v", got)
	}
	u := files[1].Chunks
	if len(u) != 2 {
		t.Fatalf("update chunks = %#v", u)
	}
	if want := []string{"const a = 1;", "const b = 2;", "const d = 5;", ""}; !reflect.DeepEqual(u[0].Before, want) {
		t.Errorf("chunk 1 before = %q", u[0].Before)
	}
	if want := []string{"const a = 1;", "const b = 3;", "const c = 4;", "const d = 5;", ""}; !reflect.DeepEqual(u[0].After, want) {
		t.Errorf("chunk 1 after = %q", u[0].After)
	}
	if want := []string{"function tail() {", "  return 1;", "}"}; !reflect.DeepEqual(u[1].After, want) {
		t.Errorf("chunk 2 after = %q", u[1].After)
	}
}

func TestCodexEdits(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse",
		"tool_name":       "apply_patch",
		"cwd":             "/repo/pkg",
		"tool_input":      map[string]any{"command": samplePatch},
	})
	edits := CodexEdits(payload, "/repo")
	if len(edits) != 3 {
		t.Fatalf("edits = %#v", edits)
	}
	if edits[0].Path != "pkg/src/new.ts" || edits[2].Path != "pkg/new/name.ts" {
		t.Errorf("paths %q, %q", edits[0].Path, edits[2].Path)
	}
	var added []string
	var replaced []string
	for _, h := range edits[1].Hunks {
		for _, l := range h.Lines {
			if l.Added {
				added = append(added, l.Text)
			}
		}
		replaced = append(replaced, h.Replaced...)
	}
	if want := []string{"const b = 3;", "const c = 4;", "  return 1;"}; !reflect.DeepEqual(added, want) {
		t.Errorf("added = %q, want %q", added, want)
	}
	if want := []string{"const b = 2;", "  return 0;"}; !reflect.DeepEqual(replaced, want) {
		t.Errorf("replaced = %q, want %q", replaced, want)
	}

	// Outside the repository, other tools, other agents: nothing.
	outside, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "apply_patch",
		"cwd": "/elsewhere", "tool_input": map[string]any{"command": samplePatch}})
	shell, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash",
		"cwd": "/repo", "tool_input": map[string]any{"command": "ls"}})
	for name, p := range map[string][]byte{"outside": outside, "shell": shell} {
		if e := CodexEdits(p, "/repo"); len(e) != 0 {
			t.Errorf("%s: %#v", name, e)
		}
	}
	if e, ok := ClaudeEdit(payload, "/repo"); ok {
		t.Errorf("a Codex payload read as Claude Code's: %#v", e)
	}
}

func TestCodexPatchInputShapes(t *testing.T) {
	for _, in := range []string{
		`{"command":"*** Begin Patch\nx"}`,
		`{"command":["apply_patch","*** Begin Patch\nx"]}`,
		`{"patch":"*** Begin Patch\nx"}`,
		`"*** Begin Patch\nx"`,
	} {
		if got := CodexPatch(json.RawMessage(in)); got != "*** Begin Patch\nx" {
			t.Errorf("%s: %q", in, got)
		}
	}
}
