package attribution

import "strings"

// DiffHunks returns the lines added going from before to after, as hunks with
// surrounding context (see diffHunks).
func DiffHunks(before, after string) []Hunk {
	return diffHunks(before, after)
}

// AuthoredOnly narrows hunks from a change the agent caused indirectly — a shell
// command — to the lines it actually wrote. A command's diff also contains
// lines it merely reformatted (prettier --write) or restored (git stash, git
// checkout, git pull). So an added line only counts while its normalized text
// occurs more often in after than in any reference version of the file (the
// file before the command, and HEAD before and after it); the rest become
// context. Hunks left without added lines are dropped.
func AuthoredOnly(hunks []Hunk, after string, refs ...string) []Hunk {
	// surplus = occurrences in after beyond the most any reference has.
	most := map[string]int{}
	for _, ref := range refs {
		for norm, n := range countNormalized(ref) {
			most[norm] = max(most[norm], n)
		}
	}
	surplus := countNormalized(after)
	for norm, n := range surplus {
		surplus[norm] = max(n-most[norm], 0)
	}
	var out []Hunk
	for _, h := range hunks {
		kept := Hunk{Lines: make([]HunkLine, len(h.Lines))}
		anyAdded := false
		for i, l := range h.Lines {
			kept.Lines[i] = HunkLine{Text: l.Text}
			if !l.Added {
				continue
			}
			if norm := normalize(l.Text); surplus[norm] > 0 {
				surplus[norm]--
				kept.Lines[i].Added = true
				anyAdded = anyAdded || strings.TrimSpace(l.Text) != ""
			}
		}
		if anyAdded {
			out = append(out, kept)
		}
	}
	return out
}

func countNormalized(text string) map[string]int {
	counts := map[string]int{}
	for _, l := range strings.Split(text, "\n") {
		counts[normalize(l)]++
	}
	return counts
}

// Encode renders a hunk as diff-style lines: "+" for added, " " for context.
func (h Hunk) Encode() []string {
	out := make([]string, len(h.Lines))
	for i, l := range h.Lines {
		if l.Added {
			out[i] = "+" + l.Text
		} else {
			out[i] = " " + l.Text
		}
	}
	return out
}

// DecodeHunk parses lines produced by Encode.
func DecodeHunk(lines []string) Hunk {
	h := Hunk{Lines: make([]HunkLine, 0, len(lines))}
	for _, l := range lines {
		if l == "" {
			h.Lines = append(h.Lines, HunkLine{})
			continue
		}
		h.Lines = append(h.Lines, HunkLine{Text: l[1:], Added: l[0] == '+'})
	}
	return h
}
