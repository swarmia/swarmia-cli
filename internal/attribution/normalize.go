// Package attribution decides which lines of a committed file were written by a
// coding agent. It compares the hunks an agent wrote (see Hunk) with the file as
// committed and classifies each surviving line as an exact copy, a copy that
// only a formatter touched, or a line a human modified. It never sees anything
// but the two texts: choosing candidate commits is the caller's job.
package attribution

import (
	"slices"
	"strings"
	"unicode"
)

// Version identifies the matching rules. It travels with every result so the
// server can discount output from versions later found to be wrong; bump it on
// any change that alters results.
const Version = "0.4.0"

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

// tokenSet is a line's distinct identifier/number tokens, for judging whether
// a changed line is still recognisably the agent's. Alignment compares every
// hunk line with every file line, so the set is laid out for that: sorted, to
// intersect two sets in one merge pass, with a bloom-style mask that rules out
// most unrelated pairs before any string is compared.
type tokenSet struct {
	toks []string // sorted, distinct
	mask uint64   // bit hash(t)%64 set for each token t
}

func newTokenSet(s string) tokenSet {
	toks := tokens(s)
	var mask uint64
	for _, t := range toks {
		mask |= 1 << (tokenHash(t) % 64)
	}
	return tokenSet{toks: toks, mask: mask}
}

// tokens splits a line into its distinct identifier/number tokens, sorted.
func tokens(s string) []string {
	toks := strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	slices.Sort(toks)
	return slices.Compact(toks)
}

// tokenHash is FNV-1a, inlined to avoid an allocation per token.
func tokenHash(t string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(t); i++ {
		h ^= uint64(t[i])
		h *= 1099511628211
	}
	return h
}

// similar reports whether two token sets overlap enough (Jaccard ≥ 0.5) that
// one line is plausibly an edited version of the other.
func similar(a, b tokenSet) bool {
	la, lb := len(a.toks), len(b.toks)
	if la == 0 || lb == 0 {
		return false
	}
	// Jaccard ≥ 0.5 means 3·|a∩b| ≥ |a|+|b|. As |a∩b| ≤ min(|a|,|b|), sets
	// whose sizes differ by more than 2× can't qualify, and sets sharing no
	// mask bit share no token.
	if 3*min(la, lb) < la+lb || a.mask&b.mask == 0 {
		return false
	}
	inter := 0
	for i, j := 0, 0; i < la && j < lb; {
		switch c := strings.Compare(a.toks[i], b.toks[j]); {
		case c < 0:
			i++
		case c > 0:
			j++
		default:
			inter++
			i++
			j++
		}
	}
	return 2*inter >= la+lb-inter
}
