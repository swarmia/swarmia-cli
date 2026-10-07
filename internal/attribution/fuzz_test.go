package attribution

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// The attribution fuzzer simulates what happens to an agent's edit before it
// is committed and measures how well MatchHunk recovers the truth.
//
// Each trial takes a real file from this repository, has the "agent" insert or
// replace a block of real code from another file (or write a new file), then
// applies a random mix of human and tool changes — tweaks, rewrites, inserted
// lines, deletions, moved blocks, formatter runs, line wrapping and joining —
// while tracking every line's true origin. The matcher's classes are then
// compared line by line with that truth.
//
// CI runs two profiles on a fixed seed — "light" (up to 6 changes per trial)
// and "heavy" (up to 30) — and enforces minimum precision and recall for each.
// Explore further locally with e.g.
//
//	go test ./internal/attribution -run TestAttributionFuzzer -v -fuzz.trials=5000 -fuzz.seed=7 -fuzz.maxops=60

var (
	fuzzTrials = flag.Int("fuzz.trials", 400, "attribution fuzzer: number of trials")
	fuzzSeed   = flag.Int64("fuzz.seed", 1, "attribution fuzzer: random seed")
	fuzzMaxOps = flag.Int("fuzz.maxops", 0, "attribution fuzzer: most post-edit changes per trial (0 = run the CI profiles)")
)

// origin is a line's true provenance.
type origin int

const (
	fromOriginal     origin = iota // in the file before the agent's edit
	fromHuman                      // typed by a human after the edit
	fromAIExact                    // the agent's line, untouched
	fromAINormalized               // the agent's line, changed only by a formatter
	fromAIModified                 // the agent's line, edited by a human
)

func (o origin) String() string {
	return [...]string{"original", "human", "ai-exact", "ai-normalized", "ai-modified"}[o]
}

func (o origin) isAI() bool { return o >= fromAIExact }

type tline struct {
	text   string
	origin origin
	aiText string // for agent lines: the text the agent wrote
}

// truth settles a line's origin from its final text: formatter changes that
// cancelled out (reindenting back and forth) leave the agent's exact text.
func (l tline) truth() origin {
	if l.origin == fromAINormalized && strings.TrimRight(l.text, " \t") == strings.TrimRight(l.aiText, " \t") {
		return fromAIExact
	}
	return l.origin
}

type fuzzer struct {
	rng    *rand.Rand
	corpus [][]string
}

// loadCorpus reads this repository's own source and docs: real code in several
// languages, so the fuzzer sees realistic lines, indentation and repetition.
func loadCorpus(t *testing.T) [][]string {
	t.Helper()
	var paths []string
	for _, pattern := range []string{
		"../../cmd/*.go", "../../internal/*/*.go", "../../README.md", "../../docs/*.md",
		"../../Makefile", "../../npm/cli/bin/swarmia-cli", "../../npm/cli/package.json",
		"../../.github/workflows/*.yml",
	} {
		m, _ := filepath.Glob(pattern)
		paths = append(paths, m...)
	}
	sort.Strings(paths)
	var corpus [][]string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n"); len(lines) >= 20 {
			corpus = append(corpus, lines)
		}
	}
	if len(corpus) < 5 {
		t.Skip("corpus not found (run from the repository)")
	}
	return corpus
}

// block returns n..m contiguous lines from a random corpus file other than
// exclude (-1 for any), trimmed of surrounding blank lines, and the file's
// index.
func (f *fuzzer) blockFrom(exclude, minLen, maxLen int) ([]string, int) {
	for {
		fi := f.rng.Intn(len(f.corpus))
		if fi == exclude {
			continue
		}
		file := f.corpus[fi]
		n := minLen + f.rng.Intn(maxLen-minLen+1)
		if n > len(file) {
			continue
		}
		start := f.rng.Intn(len(file) - n + 1)
		b := append([]string(nil), file[start:start+n]...)
		for len(b) > 0 && strings.TrimSpace(b[0]) == "" {
			b = b[1:]
		}
		for len(b) > 0 && strings.TrimSpace(b[len(b)-1]) == "" {
			b = b[:len(b)-1]
		}
		if len(b) >= minLen {
			return b, fi
		}
	}
}

func (f *fuzzer) block(minLen, maxLen int) []string {
	b, _ := f.blockFrom(-1, minLen, maxLen)
	return b
}

// trial runs one scenario and returns the final lines (with truth) and the
// matcher's classes per final line number.
func (f *fuzzer) trial(maxOps int) ([]tline, map[int]Class, string) {
	// The agent's code comes from a different file than the one it edits, so
	// it doesn't duplicate the file's own lines verbatim.
	base, baseFile := f.blockFrom(-1, 20, 200)
	agent := func(minLen, maxLen int) []string { b, _ := f.blockFrom(baseFile, minLen, maxLen); return b }
	var after []string
	var kind string
	switch r := f.rng.Intn(10); {
	case r < 2: // a new file
		kind = "new file"
		after = agent(5, 120)
		base = nil
	case r < 6: // insert a block
		kind = "insert"
		at := f.rng.Intn(len(base) + 1)
		after = append(append(append([]string{}, base[:at]...), agent(1, 40)...), base[at:]...)
	default: // replace a region
		kind = "replace"
		at := f.rng.Intn(len(base))
		n := 1 + f.rng.Intn(min(10, len(base)-at))
		after = append(append(append([]string{}, base[:at]...), agent(1, 40)...), base[at+n:]...)
	}

	before, afterText := strings.Join(base, "\n")+"\n", strings.Join(after, "\n")+"\n"
	if base == nil {
		before = ""
	}
	hunks := diffHunks(before, afterText)
	lines, added := addedFlags(before, afterText)
	doc := make([]tline, len(lines))
	for i, l := range lines {
		doc[i] = tline{text: l, origin: fromOriginal}
		if added[i] {
			doc[i] = tline{text: l, origin: fromAIExact, aiText: l}
		}
	}

	ops := f.rng.Intn(maxOps + 1)
	var applied []string
	for range ops {
		var name string
		doc, name = f.mutate(doc)
		applied = append(applied, name)
	}

	final := make([]string, len(doc))
	for i, l := range doc {
		final[i] = l.text
	}
	content := strings.Join(final, "\n") + "\n"
	best := map[int]Class{}
	for _, h := range hunks {
		for _, lm := range MatchHunk(h, content).Lines {
			if rank(lm.Class) > rank(best[lm.Line]) {
				best[lm.Line] = lm.Class
			}
		}
	}
	return doc, best, kind + " + " + strings.Join(applied, ", ")
}

func rank(c Class) int {
	return map[Class]int{Exact: 3, Normalized: 2, Modified: 1}[c]
}

// mutate applies one random post-edit change and keeps origins up to date.
func (f *fuzzer) mutate(doc []tline) ([]tline, string) {
	ai := func() []int {
		var idx []int
		for i, l := range doc {
			if l.origin.isAI() && strings.TrimSpace(l.text) != "" {
				idx = append(idx, i)
			}
		}
		return idx
	}
	pick := func(idx []int) int { return idx[f.rng.Intn(len(idx))] }

	switch f.rng.Intn(9) {
	case 0: // a human tweaks one identifier in an agent line
		var cands []int
		for _, i := range ai() {
			if len(tokens(doc[i].text)) >= 3 {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return doc, "noop"
		}
		i := pick(cands)
		if t, ok := tweak(doc[i].text, f.rng); ok {
			doc[i] = tline{text: t, origin: fromAIModified, aiText: doc[i].aiText}
		}
		return doc, "tweak"
	case 1: // a human rewrites an agent line entirely
		idx := ai()
		if len(idx) == 0 {
			return doc, "noop"
		}
		i := pick(idx)
		doc[i] = tline{text: f.block(1, 1)[0], origin: fromHuman}
		return doc, "rewrite"
	case 2: // a human inserts a few lines
		at := f.rng.Intn(len(doc) + 1)
		var ins []tline
		for _, l := range f.block(1, 5) {
			ins = append(ins, tline{text: l, origin: fromHuman})
		}
		return append(doc[:at:at], append(ins, doc[at:]...)...), "insert-human"
	case 3: // a human deletes a few lines
		if len(doc) < 2 {
			return doc, "noop"
		}
		at := f.rng.Intn(len(doc))
		n := min(1+f.rng.Intn(3), len(doc)-at)
		return append(doc[:at:at], doc[at+n:]...), "delete"
	case 4: // a human moves a block of the agent's code
		idx := ai()
		if len(idx) < 3 {
			return doc, "noop"
		}
		start := pick(idx)
		n := min(3+f.rng.Intn(12), len(doc)-start)
		moved := append([]tline(nil), doc[start:start+n]...)
		rest := append(doc[:start:start], doc[start+n:]...)
		at := f.rng.Intn(len(rest) + 1)
		return append(rest[:at:at], append(moved, rest[at:]...)...), "move"
	case 5: // a formatter reindents the file
		return reformat(doc, func(s string) string {
			trimmed := strings.TrimLeft(s, " \t")
			indent := s[:len(s)-len(trimmed)]
			if strings.Contains(indent, "\t") {
				return strings.ReplaceAll(indent, "\t", "    ") + trimmed
			}
			return strings.ReplaceAll(indent, "    ", "\t") + trimmed
		}), "reindent"
	case 6: // a formatter changes quotes and statement terminators
		return reformat(doc, func(s string) string {
			s = strings.ReplaceAll(s, `"`, `'`)
			if t := strings.TrimRight(s, " "); strings.HasSuffix(t, ")") {
				return t + ";"
			}
			return strings.TrimSuffix(s, ";")
		}), "quotes+semicolons"
	case 7: // a formatter wraps a long agent line at its commas
		var cands []int
		for _, i := range ai() {
			if len(doc[i].text) > 40 && strings.Count(doc[i].text, ", ") >= 1 {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return doc, "noop"
		}
		i := pick(cands)
		parts := strings.SplitAfter(doc[i].text, ", ")
		if len(parts) > maxMerge {
			parts = append(parts[:maxMerge-1], strings.Join(parts[maxMerge-1:], ""))
		}
		var wrapped []tline
		for j, p := range parts {
			if j > 0 {
				p = "    " + strings.TrimSpace(p)
			}
			wrapped = append(wrapped, tline{text: strings.TrimRight(p, " "), origin: fromAINormalized})
		}
		return append(doc[:i:i], append(wrapped, doc[i+1:]...)...), "wrap"
	default: // a formatter joins two short consecutive agent lines
		for _, i := range ai() {
			if i+1 < len(doc) && doc[i+1].origin == fromAIExact && doc[i].origin == fromAIExact &&
				strings.HasSuffix(strings.TrimSpace(doc[i].text), ",") && len(doc[i].text)+len(doc[i+1].text) < 80 {
				joined := tline{text: strings.TrimRight(doc[i].text, " ") + " " + strings.TrimSpace(doc[i+1].text), origin: fromAINormalized}
				return append(doc[:i:i], append([]tline{joined}, doc[i+2:]...)...), "join"
			}
		}
		return doc, "noop"
	}
}

// reformat applies a formatting change to every line; agent lines whose text
// changed become normalized (other lines keep their origin).
func reformat(doc []tline, fn func(string) string) []tline {
	for i, l := range doc {
		t := fn(l.text)
		if t != l.text && (l.origin == fromAIExact || l.origin == fromAINormalized) {
			doc[i].origin = fromAINormalized // truth() undoes this if changes cancel out
		}
		doc[i].text = t
	}
	return doc
}

// tweak renames one identifier in the line, as a human editing the agent's
// code would.
func tweak(line string, rng *rand.Rand) (string, bool) {
	var spans [][2]int
	start := -1
	for i, r := range line + " " {
		word := r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
		switch {
		case word && start < 0:
			start = i
		case !word && start >= 0:
			if i-start >= 3 {
				spans = append(spans, [2]int{start, i})
			}
			start = -1
		}
	}
	if len(spans) == 0 {
		return line, false
	}
	s := spans[rng.Intn(len(spans))]
	return line[:s[0]] + "renamed" + line[s[0]:s[1]] + line[s[1]:], true
}

// fuzzStats accumulates the comparison of truth and prediction.
type fuzzStats struct {
	confusion map[origin]map[Class]int // Class "" = not attributed
	trivial   map[origin]map[Class]int
}

func (s *fuzzStats) add(doc []tline, pred map[int]Class) {
	for i, l := range doc {
		if strings.TrimSpace(l.text) == "" {
			continue
		}
		m := s.confusion
		if isTrivial(normalize(l.text)) {
			m = s.trivial
		}
		o := l.truth()
		if m[o] == nil {
			m[o] = map[Class]int{}
		}
		m[o][pred[i+1]]++
	}
}

type fuzzMetrics struct {
	precision, recall, falsePositiveRate float64
	recallBy                             map[origin]float64 // share attributed at all
	agreementBy                          map[origin]float64 // share attributed with the matching class
}

func metrics(m map[origin]map[Class]int) fuzzMetrics {
	out := fuzzMetrics{recallBy: map[origin]float64{}, agreementBy: map[origin]float64{}}
	var tp, fp, fn, nonAI int
	for o, preds := range m {
		total, attributed := 0, 0
		for c, n := range preds {
			total += n
			if c != "" {
				attributed += n
			}
		}
		if o.isAI() {
			tp += attributed
			fn += total - attributed
			out.recallBy[o] = ratio(attributed, total)
			want := map[origin]Class{fromAIExact: Exact, fromAINormalized: Normalized, fromAIModified: Modified}[o]
			out.agreementBy[o] = ratio(preds[want], total)
		} else {
			fp += attributed
			nonAI += total
		}
	}
	out.precision = ratio(tp, tp+fp)
	out.recall = ratio(tp, tp+fn)
	out.falsePositiveRate = ratio(fp, nonAI)
	return out
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}

func (s *fuzzStats) table(m map[origin]map[Class]int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-14s %8s %8s %8s %8s\n", "truth \\ pred", "exact", "normal.", "modif.", "none")
	for o := fromOriginal; o <= fromAIModified; o++ {
		p := m[o]
		fmt.Fprintf(&b, "  %-14s %8d %8d %8d %8d\n", o, p[Exact], p[Normalized], p[Modified], p[""])
	}
	return b.String()
}

// fuzzProfiles are the CI configurations, with minimum quality on
// substantive lines at the default seed, set a little below measured values
// (2026-10-06: light 1.000 / 0.993, heavy 0.998 / 0.921). Raise them as the
// matcher improves; a drop means a regression. The corpus is this repository's
// own source, so it drifts as the code changes: across seeds 1–8 heavy recall
// spans 0.900–0.924 (2026-10-07), hence the margin.
var fuzzProfiles = []struct {
	name                    string
	maxOps                  int
	minPrecision, minRecall float64
}{
	{"light", 6, 0.995, 0.98},
	{"heavy", 30, 0.99, 0.88},
}

func TestAttributionFuzzer(t *testing.T) {
	corpus := loadCorpus(t)
	if *fuzzMaxOps > 0 { // exploring: report only
		runFuzzer(t, corpus, *fuzzMaxOps)
		return
	}
	for _, p := range fuzzProfiles {
		t.Run(p.name, func(t *testing.T) {
			m := runFuzzer(t, corpus, p.maxOps)
			if m.precision < p.minPrecision {
				t.Errorf("precision %.3f below %.3f", m.precision, p.minPrecision)
			}
			if m.recall < p.minRecall {
				t.Errorf("recall %.3f below %.3f", m.recall, p.minRecall)
			}
		})
	}
}

func runFuzzer(t *testing.T, corpus [][]string, maxOps int) fuzzMetrics {
	f := &fuzzer{rng: rand.New(rand.NewSource(*fuzzSeed)), corpus: corpus}
	stats := &fuzzStats{confusion: map[origin]map[Class]int{}, trivial: map[origin]map[Class]int{}}
	var worst []string
	for i := range *fuzzTrials {
		doc, pred, desc := f.trial(maxOps)
		stats.add(doc, pred)
		for j, l := range doc {
			if c := pred[j+1]; c != "" && !l.truth().isAI() && !isTrivial(normalize(l.text)) && len(worst) < 5 {
				worst = append(worst, fmt.Sprintf("trial %d (%s): %s line %q attributed as %s", i, desc, l.truth(), l.text, c))
			}
		}
	}

	m := metrics(stats.confusion)
	t.Logf("seed %d, %d trials, up to %d changes each — substantive lines:\n%s", *fuzzSeed, *fuzzTrials, maxOps, stats.table(stats.confusion))
	t.Logf("precision %.3f, recall %.3f, false positives on non-AI lines %.3f", m.precision, m.recall, m.falsePositiveRate)
	for o := fromAIExact; o <= fromAIModified; o++ {
		t.Logf("  %-14s attributed %.3f, with matching class %.3f", o, m.recallBy[o], m.agreementBy[o])
	}
	tm := metrics(stats.trivial)
	t.Logf("trivial lines (e.g. `}`): precision %.3f, recall %.3f\n%s", tm.precision, tm.recall, stats.table(stats.trivial))
	for _, w := range worst {
		t.Logf("false positive: %s", w)
	}
	return m
}

// FuzzMatchHunk checks invariants with Go's native fuzzing: no panics, and
// attributed lines are valid, ascending and unique, with survival bounded by
// the added lines. Run with: go test ./internal/attribution -fuzz FuzzMatchHunk
func FuzzMatchHunk(f *testing.F) {
	f.Add("a\nb\n", "a\nx := compute(1, 2)\nb\n", "a\nx := compute(1, 2)\nb\n")
	f.Add("", "func F() {\n\treturn nil\n}\n", "func F() {\n    return nil\n}\n")
	f.Add("x\n", "x\ny, z\n", "")
	f.Fuzz(func(t *testing.T, before, after, final string) {
		n := len(strings.Split(final, "\n"))
		for _, h := range diffHunks(before, after) {
			m := MatchHunk(h, final)
			prev := 0
			for _, l := range m.Lines {
				if l.Line <= prev || l.Line > n {
					t.Fatalf("invalid or unsorted line %d (prev %d, %d lines)", l.Line, prev, n)
				}
				prev = l.Line
			}
			if m.Survived > m.AddedLines {
				t.Fatalf("survived %d > added %d", m.Survived, m.AddedLines)
			}
		}
	})
}
