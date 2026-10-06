package attribution

import (
	"reflect"
	"strings"
	"testing"
)

func added(hunks []Hunk) []string {
	var out []string
	for _, h := range hunks {
		for _, l := range h.Lines {
			if l.Added && strings.TrimSpace(l.Text) != "" { // blank lines are never attributed
				out = append(out, l.Text)
			}
		}
	}
	return out
}

// Code a command generates counts; generic lines count when they're new too.
func TestAuthoredOnlyKeepsGeneratedCode(t *testing.T) {
	before := "package api\n\nfunc existing() {\n}\n"
	after := before + "\nfunc Generated() error {\n\treturn nil\n}\n"
	got := added(AuthoredOnly(DiffHunks(before, after), after, before, before))
	want := []string{"func Generated() error {", "\treturn nil", "}"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("added = %q, want %q", got, want)
	}
}

// A formatter run over human code changes lines without authoring them.
func TestAuthoredOnlyDropsReformattedLines(t *testing.T) {
	before := "const x = {a:1,b:2}\nfoo( x )\n"
	after := "const x = { a: 1, b: 2 };\nfoo(x);\n"
	if got := AuthoredOnly(DiffHunks(before, after), after, before, before); got != nil {
		t.Errorf("reformat produced authored lines: %q", added(got))
	}
}

// git stash / git checkout restore HEAD's content: nothing is authored.
func TestAuthoredOnlyDropsRestoredLines(t *testing.T) {
	head := "func a() {}\nfunc b() {}\n"
	before := "func a() {}\n" // the human had deleted b
	after := head             // the command restored it
	if got := AuthoredOnly(DiffHunks(before, after), after, before, head); got != nil {
		t.Errorf("restore produced authored lines: %q", added(got))
	}
}

// git pull brings in a teammate's code via the new HEAD: not authored either.
func TestAuthoredOnlyDropsPulledLines(t *testing.T) {
	oldHead := "func a() {}\n"
	newHead := "func a() {}\nfunc teammate() {}\n"
	if got := AuthoredOnly(DiffHunks(oldHead, newHead), newHead, oldHead, oldHead, newHead); got != nil {
		t.Errorf("pull produced authored lines: %q", added(got))
	}
}

// Mixed: a command both reformats a line and adds a new one.
func TestAuthoredOnlyMixed(t *testing.T) {
	before := "foo( x )\n"
	after := "foo(x);\nbar(y);\n"
	got := added(AuthoredOnly(DiffHunks(before, after), after, before))
	if !reflect.DeepEqual(got, []string{"bar(y);"}) {
		t.Errorf("added = %q, want [bar(y);]", got)
	}
}

func TestHunkEncodeRoundTrip(t *testing.T) {
	h := Hunk{Lines: []HunkLine{{Text: "ctx"}, {Text: "new", Added: true}, {}, {Text: "+literal plus", Added: true}}}
	if got := DecodeHunk(h.Encode()); !reflect.DeepEqual(got, h) {
		t.Errorf("round trip = %+v, want %+v", got, h)
	}
}
