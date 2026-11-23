package attribution

import (
	"fmt"
	"strings"
	"testing"
)

// benchSource builds a code-like file of n lines whose identifiers recur, so
// that many line pairs share tokens and similarity is computed for real.
func benchSource(n int) []string {
	verbs := []string{"load", "save", "parse", "render", "match", "build", "check", "send"}
	nouns := []string{"user", "session", "commit", "hunk", "line", "file", "report", "token"}
	out := make([]string, 0, n)
	for i := range n {
		v, o := verbs[i%len(verbs)], nouns[(i/len(verbs))%len(nouns)]
		switch i % 6 {
		case 0:
			out = append(out, fmt.Sprintf("func %s%s%d(ctx context.Context, %s *%s) error {", v, strings.ToUpper(o[:1])+o[1:], i, o, o))
		case 1:
			out = append(out, fmt.Sprintf("\tif err := %s.%s(ctx, %d); err != nil {", o, v, i))
		case 2:
			out = append(out, fmt.Sprintf("\t\treturn fmt.Errorf(\"%s %s: %%w\", err)", v, o))
		case 3:
			out = append(out, "\t}")
		case 4:
			out = append(out, fmt.Sprintf("\treturn %s.%s(%s, %d)", o, v, nouns[i%len(nouns)], i))
		default:
			out = append(out, "}")
		}
	}
	return out
}

// benchCase is a 60-line hunk taken from the middle of a 3,000-line file, with
// every fifth committed line edited by a human.
func benchCase() (Hunk, string) {
	src := benchSource(3000)
	var h Hunk
	for i, l := range src[1500:1560] {
		h.Lines = append(h.Lines, HunkLine{Text: l, Added: i%10 != 0})
	}
	committed := append([]string(nil), src...)
	for i := 1500; i < 1560; i += 5 {
		committed[i] += " // reviewed"
	}
	return h, strings.Join(committed, "\n") + "\n"
}

func BenchmarkMatchHunk(b *testing.B) {
	h, content := benchCase()
	b.ReportAllocs()
	for b.Loop() {
		MatchHunk(h, content)
	}
}

// BenchmarkFileMatch matches against a file parsed once, as report does for
// all the hunks matched against one committed file.
func BenchmarkFileMatch(b *testing.B) {
	h, content := benchCase()
	file := ParseFile(content)
	b.ReportAllocs()
	for b.Loop() {
		file.Match(h)
	}
}
