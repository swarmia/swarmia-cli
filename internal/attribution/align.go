package attribution

import (
	"sort"
	"strings"
)

// Hunk is one contiguous piece of an agent's edit: the new side of the change,
// in order. Added lines are the ones the agent wrote; context lines are the
// unchanged lines around them, which help locate the hunk but are never
// attributed.
type Hunk struct {
	Lines []HunkLine
	// Replaced are the lines the edit removed. A committed line equal to one
	// of them is the version from before the edit, so it is never attributed
	// as the agent's line modified — e.g. in a commit rebased after the edit.
	Replaced []string
}

type HunkLine struct {
	Text  string
	Added bool
}

// Class says how a committed line relates to the agent's line it came from.
type Class string

const (
	Exact      Class = "exact"      // identical, apart from trailing whitespace
	Normalized Class = "normalized" // equal once formatting is ignored (see normalize)
	Modified   Class = "modified"   // the agent's line, edited by a human
)

// LineMatch attributes one line of the committed file (1-based) to the agent.
type LineMatch struct {
	Line  int
	Class Class
}

// Match is the outcome of locating a hunk in a committed file.
type Match struct {
	Lines      []LineMatch // committed lines derived from the hunk's added lines, ascending
	AddedLines int         // non-blank lines the agent added in the hunk
	Survived   int         // added lines found exact or normalized
}

// Survival is the share of the agent's added lines that survived into the file
// unchanged or only reformatted.
func (m Match) Survival() float64 {
	if m.AddedLines == 0 {
		return 0
	}
	return float64(m.Survived) / float64(m.AddedLines)
}

// Scores for the local alignment. A substantive line matching is strong
// evidence; trivial lines (`}`) and lines a human edited only extend an
// alignment. Penalties are mild so a few human-inserted or rewritten lines
// don't break one, while a long unrelated stretch still does.
const (
	scoreExact      = 4
	scoreNormalized = 3
	scoreTrivial    = 1
	scoreModified   = 2
	scoreMismatch   = -1
	scoreGap        = -1
	// minEvidence is how many substantive lines (added or context) must match
	// exactly or normalized for an alignment to count.
	minEvidence = 2
	// maxMerge is how many lines one line may be wrapped into (or joined from)
	// by a formatter and still match as normalized.
	maxMerge = 4
	// maxIterations bounds how many separate pieces of one hunk are located, for
	// hunks a human split apart or partly moved.
	maxIterations = 8
	// maxCells caps the alignment matrix (~5 bytes per cell); larger inputs are
	// first narrowed to a window around the hunk's likely position.
	maxCells = 4_000_000
)

type line struct {
	raw     string // trailing whitespace trimmed
	norm    string
	loose   string // norm without any , or ; — for comparing wrapped/joined lines
	toks    map[string]struct{}
	trivial bool
	added   bool // hunk side only
	num     int  // 1-based line number in the original text
}

func newLine(text string, num int, added bool) line {
	raw := strings.TrimRight(text, " \t\r")
	norm := normalize(raw)
	loose := strings.NewReplacer(",", "", ";", "").Replace(norm)
	return line{raw: raw, norm: norm, loose: loose, toks: tokens(raw), trivial: isTrivial(norm), added: added, num: num}
}

func hunkLines(h Hunk) []line {
	var out []line
	for i, hl := range h.Lines {
		if strings.TrimSpace(hl.Text) != "" {
			out = append(out, newLine(hl.Text, i+1, hl.Added))
		}
	}
	return out
}

func fileLines(content string) []line {
	var out []line
	for i, text := range strings.Split(content, "\n") {
		if strings.TrimSpace(text) != "" {
			out = append(out, newLine(text, i+1, false))
		}
	}
	return out
}

// MatchHunk locates the hunk in the committed file content and attributes the
// lines that came from the agent's added lines. Blank lines are ignored on both
// sides. It may find several pieces, if a human split the hunk up or moved part
// of it; see accept for what counts as a piece.
func MatchHunk(h Hunk, content string) Match {
	H := hunkLines(h)
	F := fileLines(content)
	var m Match
	for _, l := range H {
		if l.added {
			m.AddedLines++
		}
	}
	if m.AddedLines == 0 || len(F) == 0 {
		return m
	}
	F = narrow(H, F)

	substantive := 0
	for _, l := range H {
		if !l.trivial {
			substantive++
		}
	}
	needEvidence := min(minEvidence, substantive)

	usedH := make([]bool, len(H))
	usedF := make([]bool, len(F))
	classes := map[int]Class{}
	replaced := map[string]bool{}
	for _, r := range h.Replaced {
		if n := normalize(r); n != "" {
			replaced[n] = true
		}
	}
	for range maxIterations {
		pairs := align(H, F, usedH, usedF)
		if len(replaced) > 0 {
			pairs = withoutPreEditLines(pairs, F, replaced)
		}
		if len(pairs) == 0 || !accept(H, pairs, needEvidence) {
			break
		}
		for _, p := range pairs {
			for _, i := range p.h {
				usedH[i] = true
				if H[i].added && p.class != Modified {
					m.Survived++
				}
			}
			for _, j := range p.f {
				usedF[j] = true
			}
			if !anyAdded(H, p.h) {
				continue // context: locates the hunk, but isn't the agent's
			}
			for _, j := range p.f {
				classes[F[j].num] = p.class
			}
		}
	}
	for num, c := range classes {
		m.Lines = append(m.Lines, LineMatch{Line: num, Class: c})
	}
	sort.Slice(m.Lines, func(a, b int) bool { return m.Lines[a].Line < m.Lines[b].Line })
	return m
}

// withoutPreEditLines drops `modified` pairs whose file line is a line the
// edit replaced: the file holds the pre-edit version there, not an edit of
// the agent's line.
func withoutPreEditLines(pairs []pair, F []line, replaced map[string]bool) []pair {
	kept := pairs[:0:0]
	for _, p := range pairs {
		if p.class == Modified && len(p.f) == 1 && replaced[F[p.f[0]].norm] {
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// pair links hunk lines to file lines: usually one to one, or one to several
// when a formatter wrapped or joined lines.
type pair struct {
	h, f  []int
	class Class
}

func anyAdded(H []line, idx []int) bool {
	for _, i := range idx {
		if H[i].added {
			return true
		}
	}
	return false
}

// accept decides whether an alignment is evidence of the agent's work. It needs
// enough substantive matching lines to rule out coincidence, and among them an
// added line that survived — or, failing that, an added line a human modified,
// pinned in place by matching context (a one-line edit someone then tweaked).
// Without the added-line rule, an alignment held up only by context (the
// agent's lines were deleted) would attribute lines the agent didn't write.
func accept(H []line, pairs []pair, needEvidence int) bool {
	evidence, survivedAdded, modifiedAdded := 0, false, false
	for _, p := range pairs {
		if p.class == Modified {
			modifiedAdded = modifiedAdded || anyAdded(H, p.h)
			continue
		}
		if len(p.h) > 1 || len(p.f) > 1 {
			evidence += 2 // a wrapped/joined match is long, specific text
			survivedAdded = survivedAdded || anyAdded(H, p.h)
			continue
		}
		if l := H[p.h[0]]; !l.trivial {
			evidence++
			survivedAdded = survivedAdded || l.added
		}
	}
	if evidence < needEvidence {
		return false
	}
	return survivedAdded || modifiedAdded
}

// Traceback moves. Merges encode their width: moveMergeF+k means one hunk line
// matched k file lines; moveMergeH+k the reverse.
const (
	moveStop byte = iota
	moveDiag
	moveSkipH
	moveSkipF
	moveMergeF = 10
	moveMergeH = 20
)

// align runs one Smith-Waterman local alignment of the unused hunk lines
// against the unused file lines and returns the best-scoring alignment.
func align(H, F []line, usedH, usedF []bool) []pair {
	n, m := len(H), len(F)
	w := m + 1
	score := make([]int32, (n+1)*w)
	move := make([]byte, (n+1)*w)
	fLen := prefixLens(F)
	hLen := prefixLens(H)

	best, bi, bj := int32(0), 0, 0
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			s, mv := int32(0), moveStop
			try := func(v int32, how byte) {
				if v > s {
					s, mv = v, how
				}
			}
			if !usedH[i-1] && !usedF[j-1] {
				if v, ok := pairScore(H[i-1], F[j-1]); ok {
					try(score[(i-1)*w+j-1]+v, moveDiag)
				} else {
					try(score[(i-1)*w+j-1]+scoreMismatch, moveDiag)
				}
			}
			try(score[(i-1)*w+j]+scoreGap, moveSkipH)
			try(score[i*w+j-1]+scoreGap, moveSkipF)
			for k := 2; k <= maxMerge; k++ {
				if j >= k && joins(H[i-1].loose, F, j-k, j, fLen) && free(usedF, j-k, j) && !usedH[i-1] {
					try(score[(i-1)*w+j-k]+int32(scoreNormalized*k), moveMergeF+byte(k))
				}
				if i >= k && joins(F[j-1].loose, H, i-k, i, hLen) && free(usedH, i-k, i) && !usedF[j-1] {
					try(score[(i-k)*w+j-1]+int32(scoreNormalized*k), moveMergeH+byte(k))
				}
			}
			score[i*w+j], move[i*w+j] = s, mv
			if s > best {
				best, bi, bj = s, i, j
			}
		}
	}

	var pairs []pair
	for i, j := bi, bj; i > 0 && j > 0 && score[i*w+j] > 0; {
		switch mv := move[i*w+j]; {
		case mv == moveDiag:
			if _, ok := pairScore(H[i-1], F[j-1]); ok {
				pairs = append(pairs, pair{h: []int{i - 1}, f: []int{j - 1}, class: classify(H[i-1], F[j-1])})
			}
			i, j = i-1, j-1
		case mv == moveSkipH:
			i--
		case mv == moveSkipF:
			j--
		case mv > moveMergeH:
			k := int(mv - moveMergeH)
			pairs = append(pairs, pair{h: span(i-k, i), f: []int{j - 1}, class: Normalized})
			i, j = i-k, j-1
		case mv > moveMergeF:
			k := int(mv - moveMergeF)
			pairs = append(pairs, pair{h: []int{i - 1}, f: span(j-k, j), class: Normalized})
			i, j = i-1, j-k
		default:
			i = 0 // moveStop
		}
	}
	return pairs
}

// pairScore scores aligning hunk line h with file line f; ok is false for a
// mismatch.
func pairScore(h, f line) (int32, bool) {
	switch {
	case h.norm != f.norm:
		if !h.trivial && !f.trivial && similar(h.toks, f.toks) {
			return scoreModified, true
		}
		return 0, false
	case h.trivial:
		return scoreTrivial, true
	case h.raw == f.raw:
		return scoreExact, true
	default:
		return scoreNormalized, true
	}
}

func classify(h, f line) Class {
	switch {
	case h.norm != f.norm:
		return Modified
	case h.raw == f.raw:
		return Exact
	default:
		return Normalized
	}
}

// joins reports whether target equals the concatenated loose text of
// lines[from:to]. The length check (via prefix sums) rejects almost every
// candidate before any string comparison.
func joins(target string, lines []line, from, to int, prefix []int) bool {
	if prefix[to]-prefix[from] != len(target) || isTrivial(target) {
		return false
	}
	off := 0
	for _, l := range lines[from:to] {
		if !strings.HasPrefix(target[off:], l.loose) {
			return false
		}
		off += len(l.loose)
	}
	return true
}

func prefixLens(ls []line) []int {
	p := make([]int, len(ls)+1)
	for i, l := range ls {
		p[i+1] = p[i] + len(l.loose)
	}
	return p
}

func free(used []bool, from, to int) bool {
	for _, u := range used[from:to] {
		if u {
			return false
		}
	}
	return true
}

func span(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// narrow keeps the alignment matrix bounded for very large inputs by cutting the
// file down to a window around where the hunk's substantive lines occur most
// densely. Typical hunks and files never hit this.
func narrow(H, F []line) []line {
	if len(H)*len(F) <= maxCells {
		return F
	}
	want := map[string]bool{}
	for _, l := range H {
		if !l.trivial {
			want[l.norm] = true
		}
	}
	var hits []int
	for j, l := range F {
		if want[l.norm] {
			hits = append(hits, j)
		}
	}
	size := max(maxCells/max(len(H), 1), 1)
	if len(hits) == 0 || size >= len(F) {
		return F[:min(size, len(F))]
	}
	// Slide a window of `size` lines over the hits and keep the densest.
	bestStart, bestCount := hits[0], 0
	for a, b := 0, 0; a < len(hits); a++ {
		for b < len(hits) && hits[b] < hits[a]+size {
			b++
		}
		if b-a > bestCount {
			bestStart, bestCount = hits[a], b-a
		}
	}
	start := max(min(bestStart-size/4, len(F)-size), 0)
	return F[start : start+size]
}
