package attribution

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden/*/want.txt from current results")

// TestGolden runs real Claude Code edits (testdata/golden/<case>/event.json)
// against the file as it was later committed (committed) and compares the
// attribution with want.txt. Cases come from sessions recorded while building
// this repo; see testdata/golden/README.md. Review diffs to want.txt by hand
// before accepting them with -update.
func TestGolden(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "golden", "*", "event.json"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no golden cases found: %v", err)
	}
	for _, eventPath := range dirs {
		dir := filepath.Dir(eventPath)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			payload, err := os.ReadFile(eventPath)
			if err != nil {
				t.Fatal(err)
			}
			committed, err := os.ReadFile(filepath.Join(dir, "committed"))
			if err != nil {
				t.Fatal(err)
			}
			edit, ok := ClaudeEdit(payload, "/repo")
			if !ok {
				t.Fatal("ClaudeEdit: no edit extracted")
			}
			got := renderGolden(edit, string(committed))

			wantPath := filepath.Join(dir, "want.txt")
			if *update {
				if err := os.WriteFile(wantPath, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(wantPath)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if got != string(want) {
				t.Errorf("attribution changed; got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// renderGolden prints each hunk's survival and every attributed committed line
// with its class and text, so want.txt reads as a reviewable annotated diff.
func renderGolden(edit Edit, committed string) string {
	lines := strings.Split(committed, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", edit.Path)
	for i, h := range edit.Hunks {
		m := MatchHunk(h, committed)
		fmt.Fprintf(&b, "\nhunk %d: %d/%d added lines survived\n", i+1, m.Survived, m.AddedLines)
		for _, l := range m.Lines {
			fmt.Fprintf(&b, "  %4d %-10s | %s\n", l.Line, l.Class, lines[l.Line-1])
		}
	}
	return b.String()
}
