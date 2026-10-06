// Package attribution decides which lines of a committed file were written by a
// coding agent. It compares the hunks an agent wrote (see Hunk) with the file as
// committed and classifies each surviving line as an exact copy, a copy that
// only a formatter touched, or a line a human modified. It never sees anything
// but the two texts: choosing candidate commits is the caller's job.
package attribution

import (
	"strings"
	"unicode"
)

// Version identifies the matching rules. It travels with every result so the
// server can discount output from versions later found to be wrong; bump it on
// any change that alters results.
const Version = "0.1.0"

// normalize maps a line to the form formatters can't change: no whitespace at
// all, no trailing commas or semicolons (also before a closing bracket), and a
// single quote style. Removing all whitespace (not just collapsing it) is what
// makes `foo(a,b)` equal `foo(a, b)`; across a whole line, collisions from it
// are negligible.
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
		case r == '"' || r == '`':
			b.WriteByte('\'')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for _, closer := range []string{")", "]", "}"} {
		out = strings.ReplaceAll(out, ","+closer, closer)
	}
	return strings.TrimRight(out, ",;")
}

// trivialChars is the minimum number of identifier characters for a line to
// count as evidence on its own. Lines like `}`, `});` or `fi` occur everywhere,
// so matching them says nothing about where a hunk landed.
const trivialChars = 3

func isTrivial(norm string) bool {
	n := 0
	for _, r := range norm {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			if n++; n >= trivialChars {
				return false
			}
		}
	}
	return true
}

// tokens splits a line into its identifier/number tokens, for judging whether a
// changed line is still recognisably the agent's.
func tokens(s string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
	}) {
		set[f] = struct{}{}
	}
	return set
}

// similar reports whether two token sets overlap enough (Jaccard ≥ 0.5) that
// one line is plausibly an edited version of the other.
func similar(a, b map[string]struct{}) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	inter := 0
	for t := range a {
		if _, ok := b[t]; ok {
			inter++
		}
	}
	return 2*inter >= len(a)+len(b)-inter
}
