package report

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/events"
)

// The pipeline fuzzer runs whole scenarios through real git repositories and
// the full attribution pipeline (Build): an agent edits files (through its edit
// tool and through shell commands), a human edits and reformats, and the
// developer commits, amends, squashes, and rebases onto a teammate's fetched
// commits. Every line's true origin is tracked in a model of the working tree,
// and each commit's snapshot of it is kept, so the truth for every reachable
// commit is known: the lines git says the commit introduced, and where each
// came from. That truth is compared with what Build attributes.
//
// It complements the matcher fuzzer in internal/attribution by exercising
// commit selection (author and reflog), reachability after rewrites, the
// introduced-lines rule, and combining hunks across edits. Explore with e.g.
//
//	go test ./internal/report -run TestPipelineFuzzer -v -pfuzz.trials=200 -pfuzz.steps=30

var (
	pfuzzTrials = flag.Int("pfuzz.trials", 10, "pipeline fuzzer: number of trials")
	pfuzzSeed   = flag.Int64("pfuzz.seed", 1, "pipeline fuzzer: random seed")
	pfuzzSteps  = flag.Int("pfuzz.steps", 12, "pipeline fuzzer: steps per trial")
)

type porigin int

const (
	pOriginal porigin = iota
	pHuman
	pTeammate
	pAIExact
	pAINormalized
	pAIModified
)

func (o porigin) isAI() bool { return o >= pAIExact }

func (o porigin) String() string {
	return [...]string{"original", "human", "teammate", "ai-exact", "ai-normalized", "ai-modified"}[o]
}

type pline struct {
	text   string
	origin porigin
	aiText string
}

func (l pline) truth() porigin {
	if l.origin == pAINormalized && strings.TrimRight(l.text, " \t") == strings.TrimRight(l.aiText, " \t") {
		return pAIExact
	}
	return l.origin
}

type snapshot map[string][]pline // path → lines

func (s snapshot) clone() snapshot {
	c := snapshot{}
	for p, ls := range s {
		c[p] = append([]pline(nil), ls...)
	}
	return c
}

type pfuzzer struct {
	t      *testing.T
	rng    *rand.Rand
	corpus [][]string
	f      *fixture
	team   *fixture
	now    time.Time
	model  snapshot            // the working tree
	source map[string]int      // path → corpus file its original content came from
	snaps  map[string]snapshot // commit subject → files as committed
	nextC  int
	log    []string
}

const (
	me       = "me@example.com"
	teammate = "teammate@example.com"
)

func loadPipelineCorpus(t *testing.T) [][]string {
	t.Helper()
	var paths []string
	for _, pattern := range []string{"../../cmd/*.go", "../../internal/*/*.go", "../../README.md", "../../docs/*.md", "../../Makefile"} {
		m, _ := filepath.Glob(pattern)
		paths = append(paths, m...)
	}
	sort.Strings(paths)
	var corpus [][]string
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			if lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n"); len(lines) >= 20 {
				corpus = append(corpus, lines)
			}
		}
	}
	if len(corpus) < 5 {
		t.Skip("corpus not found (run from the repository)")
	}
	return corpus
}

func (p *pfuzzer) block(minLen, maxLen int) []string {
	b, _ := p.blockFrom(-1, minLen, maxLen)
	return b
}

// blockFrom returns lines from a random corpus file other than exclude, and
// that file's index.
func (p *pfuzzer) blockFrom(exclude, minLen, maxLen int) ([]string, int) {
	for {
		fi := p.rng.Intn(len(p.corpus))
		if fi == exclude {
			continue
		}
		file := p.corpus[fi]
		n := minLen + p.rng.Intn(maxLen-minLen+1)
		if n > len(file) {
			continue
		}
		start := p.rng.Intn(len(file) - n + 1)
		b := file[start : start+n]
		for len(b) > 0 && strings.TrimSpace(b[0]) == "" {
			b = b[1:]
		}
		for len(b) > 0 && strings.TrimSpace(b[len(b)-1]) == "" {
			b = b[:len(b)-1]
		}
		if len(b) >= minLen {
			return append([]string(nil), b...), fi
		}
	}
}

func (p *pfuzzer) tick() time.Time {
	p.now = p.now.Add(time.Minute)
	return p.now
}

func text(lines []pline) string {
	if len(lines) == 0 {
		return ""
	}
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.text
	}
	return strings.Join(parts, "\n") + "\n"
}

func (p *pfuzzer) writeModel() {
	for path, lines := range p.model {
		p.f.write(path, text(lines))
	}
}

func (p *pfuzzer) paths() []string {
	var ps []string
	for path := range p.model {
		ps = append(ps, path)
	}
	sort.Strings(ps)
	return ps
}

// --- the agent ---------------------------------------------------------------

// agentEdit has the agent insert or replace a block in a file (or create one),
// logged either as a Write tool call or as a shell command's snapshot changes.
func (p *pfuzzer) agentEdit() {
	var path string
	if p.rng.Intn(5) == 0 || len(p.model) == 0 {
		path = fmt.Sprintf("src/new%d.go", len(p.model))
	} else {
		ps := p.paths()
		path = ps[p.rng.Intn(len(ps))]
	}
	old := p.model[path]
	beforeText := text(old)
	var newLines []string
	for _, l := range old {
		newLines = append(newLines, l.text)
	}
	// The agent's code comes from a different file than the one the edited
	// file's content came from, so it doesn't duplicate its lines verbatim.
	src, ok := p.source[path]
	if !ok {
		src = -1
	}
	block, bsrc := p.blockFrom(src, 1, 30)
	if !ok {
		p.source[path] = bsrc
	}
	at := 0
	if len(newLines) > 0 {
		at = p.rng.Intn(len(newLines) + 1)
	}
	cut := 0
	if p.rng.Intn(2) == 0 && at < len(newLines) {
		cut = 1 + p.rng.Intn(min(5, len(newLines)-at))
	}
	newLines = append(append(append([]string{}, newLines[:at]...), block...), newLines[at+cut:]...)
	afterText := strings.Join(newLines, "\n") + "\n"

	// Lines the diff calls added are the agent's; the others are the old lines,
	// in order, keeping their origins.
	added := attribution.AddedFlags(beforeText, afterText)
	updated := make([]pline, len(newLines))
	oi := 0
	for i, l := range newLines {
		if added[i] {
			updated[i] = pline{text: l, origin: pAIExact, aiText: l}
			continue
		}
		for oi < len(old) && old[oi].text != l {
			oi++
		}
		if oi >= len(old) {
			p.t.Fatalf("kept line %q not found in the old version", l)
		}
		updated[i] = old[oi]
		oi++
	}
	p.model[path] = updated

	at2 := p.tick()
	if p.rng.Intn(3) == 0 {
		// A shell command (codegen, sed): recorded as snapshot changes.
		hunks := attribution.AuthoredOnly(attribution.DiffHunks(beforeText, afterText), afterText, beforeText)
		var enc [][]string
		for _, h := range hunks {
			enc = append(enc, h.Encode())
		}
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": "sess-1", "tool_name": "Bash",
			"tool_input": map[string]any{"command": "./gen.sh"}})
		p.f.recs = append(p.f.recs, events.Record{LoggedAt: at2.Format(time.RFC3339), Git: &events.Git{Root: p.f.root}, Event: raw,
			Changes: []events.FileChange{{Path: path, Hunks: enc}}})
		p.log = append(p.log, "shell-edit "+path)
	} else {
		var original any
		if len(old) > 0 {
			original = beforeText
		}
		p.f.agentWrite(at2, path, afterText, original)
		p.log = append(p.log, "agent-write "+path)
	}
	p.writeModel()
}

// --- the human -----------------------------------------------------------------

func (p *pfuzzer) humanEdit() {
	ps := p.paths()
	if len(ps) == 0 {
		return
	}
	path := ps[p.rng.Intn(len(ps))]
	doc := p.model[path]
	if len(doc) == 0 {
		return
	}
	i := p.rng.Intn(len(doc))
	switch p.rng.Intn(5) {
	case 0: // tweak an agent line
		for k := 0; k < len(doc); k++ {
			j := (i + k) % len(doc)
			if doc[j].origin.isAI() && len(strings.Fields(doc[j].text)) >= 3 {
				doc[j] = pline{text: doc[j].text + " // renamedValue", origin: pAIModified, aiText: doc[j].aiText}
				break
			}
		}
		p.log = append(p.log, "human-tweak "+path)
	case 1: // rewrite a line
		doc[i] = pline{text: p.block(1, 1)[0], origin: pHuman}
		p.log = append(p.log, "human-rewrite "+path)
	case 2: // insert lines
		var ins []pline
		for _, l := range p.block(1, 4) {
			ins = append(ins, pline{text: l, origin: pHuman})
		}
		doc = append(doc[:i:i], append(ins, doc[i:]...)...)
		p.log = append(p.log, "human-insert "+path)
	case 3: // delete lines
		n := min(1+p.rng.Intn(3), len(doc)-i)
		doc = append(doc[:i:i], doc[i+n:]...)
		p.log = append(p.log, "human-delete "+path)
	default: // a formatter reindents the file
		for j, l := range doc {
			trimmed := strings.TrimLeft(l.text, " \t")
			indent := l.text[:len(l.text)-len(trimmed)]
			var t string
			if strings.Contains(indent, "\t") {
				t = strings.ReplaceAll(indent, "\t", "    ") + trimmed
			} else {
				t = strings.ReplaceAll(indent, "    ", "\t") + trimmed
			}
			if t != l.text && (l.origin == pAIExact || l.origin == pAINormalized) {
				doc[j].origin = pAINormalized
			}
			doc[j].text = t
		}
		p.log = append(p.log, "format "+path)
	}
	p.model[path] = doc
	p.writeModel()
}

// --- git operations ------------------------------------------------------------

func (p *pfuzzer) subject() string {
	p.nextC++
	return fmt.Sprintf("c%d", p.nextC)
}

func (p *pfuzzer) commitAll() {
	if p.f.git(p.now, me, "status", "--porcelain") == "" {
		return
	}
	at := p.tick()
	s := p.subject()
	p.f.git(at, me, "add", "-A")
	p.f.git(at, me, "commit", "-q", "-m", s)
	p.snaps[s] = p.model.clone()
	p.log = append(p.log, "commit "+s)
}

// commitSome commits only one file; the snapshot reflects exactly what was
// committed (that file's model, the rest as in the parent commit).
func (p *pfuzzer) commitSome() {
	status := p.f.git(p.now, me, "status", "--porcelain")
	if status == "" {
		return
	}
	path := strings.TrimSpace(strings.Split(status, "\n")[0][3:])
	if _, ok := p.model[path]; !ok {
		return
	}
	at := p.tick()
	s := p.subject()
	parent := p.headSnapshot()
	parent[path] = append([]pline(nil), p.model[path]...)
	p.f.git(at, me, "add", "--", path)
	p.f.git(at, me, "commit", "-q", "-m", s)
	p.snaps[s] = parent
	p.log = append(p.log, "commit-some "+s)
}

// headSnapshot returns the snapshot of HEAD's tree (by its subject).
func (p *pfuzzer) headSnapshot() snapshot {
	subj := p.f.git(p.now, me, "log", "-1", "--format=%s")
	if s, ok := p.snaps[subj]; ok {
		return s.clone()
	}
	return snapshot{}
}

func (p *pfuzzer) amend() {
	if !strings.HasPrefix(p.f.git(p.now, me, "log", "-1", "--format=%s"), "c") {
		return
	}
	at := p.tick()
	s := p.subject()
	p.f.git(at, me, "add", "-A")
	p.f.git(at, me, "commit", "-q", "--amend", "-m", s)
	p.snaps[s] = p.model.clone()
	p.log = append(p.log, "amend "+s)
}

func (p *pfuzzer) squash() {
	subjects := strings.Split(p.f.git(p.now, me, "log", "-3", "--format=%s"), "\n")
	if len(subjects) < 3 || !strings.HasPrefix(subjects[0], "c") || !strings.HasPrefix(subjects[1], "c") {
		return
	}
	at := p.tick()
	p.f.git(at, me, "reset", "-q", "--soft", "HEAD~2")
	s := p.subject()
	p.f.git(at, me, "commit", "-q", "-m", s)
	p.snaps[s] = p.snaps[subjects[0]].clone() // the tree of the newer one
	p.log = append(p.log, "squash "+s)
}

// rebase has the teammate commit to their own file in their clone; the
// developer fetches and rebases onto it. Commits get new SHAs with the same
// subjects and (for the developer's files) the same contents.
func (p *pfuzzer) rebase() {
	if p.f.git(p.now, me, "status", "--porcelain") != "" {
		p.commitAll() // rebase needs a clean tree
	}
	at := p.tick()
	team := p.team.f()
	lines := p.block(2, 10)
	teamFile := fmt.Sprintf("team/t%d.go", p.nextC)
	p.team.write(teamFile, strings.Join(lines, "\n")+"\n")
	p.team.git(at, teammate, "add", "-A")
	p.team.git(at, teammate, "commit", "-q", "-m", "teammate "+teamFile)
	p.f.git(at, me, "fetch", "-q", team, "main:refs/remotes/teammate/main")
	p.f.git(at, me, "rebase", "-q", "refs/remotes/teammate/main")
	// The teammate's file is now in every rebased commit; record it in the
	// snapshots so the truth knows it isn't the agent's.
	var tl []pline
	for _, l := range lines {
		tl = append(tl, pline{text: l, origin: pTeammate})
	}
	p.model[teamFile] = tl
	for _, s := range p.snaps {
		if _, ok := s[teamFile]; !ok {
			s[teamFile] = tl
		}
	}
	p.log = append(p.log, "rebase onto "+teamFile)
}

func (fx *fixture) f() string { return fx.root }

// --- evaluation ----------------------------------------------------------------

type pkey struct {
	sha, path string
	line      int
}

// truth lists, for each reachable commit the developer made, the introduced
// non-blank lines and their true origin.
func (p *pfuzzer) truth() map[pkey]pline {
	out := map[pkey]pline{}
	r := newRepo(p.f.root)
	for _, rec := range strings.Split(p.f.git(p.now, me, "log", "--no-merges", "--format=%H %s", "HEAD"), "\n") {
		sha, subj, _ := strings.Cut(rec, " ")
		snap, ok := p.snaps[subj]
		if !ok {
			continue // base commit or the teammate's
		}
		for path, lines := range snap {
			for n := range r.addedLines(sha, path) {
				if n-1 < len(lines) && strings.TrimSpace(lines[n-1].text) != "" {
					out[pkey{sha, path, n}] = lines[n-1]
				}
			}
		}
	}
	return out
}

type pstats struct {
	confusion map[porigin]map[attribution.Class]int
	trivial   map[porigin]map[attribution.Class]int
	worst     []string
}

func (p *pfuzzer) evaluate(st *pstats, trial int) {
	sessions, err := Build(p.f.recs, Options{FallbackRoot: p.f.root, AuthorEmail: emailIs(me)})
	if err != nil {
		p.t.Fatal(err)
	}
	pred := map[pkey]attribution.Class{}
	for _, s := range sessions {
		for _, c := range s.Commits() {
			for _, f := range c.Files {
				for _, r := range f.Lines {
					for n := r.Start; n <= r.End; n++ {
						pred[pkey{c.SHA, f.Path, n}] = r.Class
					}
				}
			}
		}
	}
	truth := p.truth()
	for k, l := range truth {
		m := st.confusion
		if isTrivialLine(l.text) {
			m = st.trivial
		}
		o := l.truth()
		if m[o] == nil {
			m[o] = map[attribution.Class]int{}
		}
		m[o][pred[k]]++
		if pred[k] != "" && !o.isAI() && !isTrivialLine(l.text) && len(st.worst) < 5 {
			st.worst = append(st.worst, fmt.Sprintf("trial %d: %s line %q in %s:%d attributed as %s (steps: %s)",
				trial, o, l.text, k.path, k.line, pred[k], strings.Join(p.log, ", ")))
		}
	}
	// Attributions outside the truth (lines a commit didn't introduce, or
	// commits that aren't the developer's) are false positives too.
	for k, c := range pred {
		if _, ok := truth[k]; !ok {
			m := st.confusion
			if st.confusion[pHuman] == nil {
				st.confusion[pHuman] = map[attribution.Class]int{}
			}
			m[pHuman][c]++
			if len(st.worst) < 5 {
				st.worst = append(st.worst, fmt.Sprintf("trial %d: attribution outside the truth %s:%d in %s (%s)", trial, k.path, k.line, k.sha[:7], c))
			}
		}
	}
}

func isTrivialLine(s string) bool {
	n := 0
	for _, r := range s {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			n++
		}
	}
	return n < 3
}

func ratioOf(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}

func (st *pstats) summary(m map[porigin]map[attribution.Class]int) (precision, recall float64, table string) {
	var tp, fp, fn int
	var b strings.Builder
	fmt.Fprintf(&b, "  %-14s %8s %8s %8s %8s\n", "truth \\ pred", "exact", "normal.", "modif.", "none")
	for o := pOriginal; o <= pAIModified; o++ {
		row := m[o]
		attributed := row[attribution.Exact] + row[attribution.Normalized] + row[attribution.Modified]
		if o.isAI() {
			tp += attributed
			fn += row[""]
		} else {
			fp += attributed
		}
		fmt.Fprintf(&b, "  %-14s %8d %8d %8d %8d\n", o, row[attribution.Exact], row[attribution.Normalized], row[attribution.Modified], row[""])
	}
	return ratioOf(tp, tp+fp), ratioOf(tp, tp+fn), b.String()
}

// Minimum quality on substantive lines at the default seed, a little below
// measured values (2026-10-06: 1.000 / 0.999 at the defaults, and at seeds 2–3
// with 40 trials × 30 steps); a drop means a regression.
const (
	pipelineMinPrecision = 0.99
	pipelineMinRecall    = 0.97
)

func TestPipelineFuzzer(t *testing.T) {
	corpus := loadPipelineCorpus(t)
	rng := rand.New(rand.NewSource(*pfuzzSeed))
	st := &pstats{confusion: map[porigin]map[attribution.Class]int{}, trivial: map[porigin]map[attribution.Class]int{}}
	ops := map[string]int{}
	for trial := range *pfuzzTrials {
		p := &pfuzzer{t: t, rng: rng, corpus: corpus, f: newFixture(t), now: t0, model: snapshot{}, source: map[string]int{}, snaps: map[string]snapshot{}}
		// Base commit with a few real files, and a teammate's clone of it.
		for i := range 3 {
			var ls []pline
			block, src := p.blockFrom(-1, 20, 80)
			for _, l := range block {
				ls = append(ls, pline{text: l, origin: pOriginal})
			}
			path := fmt.Sprintf("src/file%d.go", i)
			p.model[path] = ls
			p.source[path] = src
		}
		p.writeModel()
		p.f.git(t0.Add(-30*time.Minute), me, "add", "-A")
		p.f.git(t0.Add(-30*time.Minute), me, "commit", "-q", "-m", "base")
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		p.team = &fixture{t: t, root: filepath.Join(dir, "team")}
		p.f.git(t0, me, "clone", "-q", p.f.root, p.team.root)

		for range *pfuzzSteps {
			switch r := rng.Intn(20); {
			case r < 7:
				p.agentEdit()
			case r < 11:
				p.humanEdit()
			case r < 14:
				p.commitAll()
			case r < 15:
				p.commitSome()
			case r < 17:
				p.amend()
			case r < 18:
				p.squash()
			default:
				p.rebase()
			}
		}
		p.commitAll()
		for _, l := range p.log {
			ops[strings.Fields(l)[0]]++
		}
		p.evaluate(st, trial)
	}

	precision, recall, table := st.summary(st.confusion)
	tp, tr, ttable := st.summary(st.trivial)
	var opList []string
	for op, n := range ops {
		opList = append(opList, fmt.Sprintf("%s %d", op, n))
	}
	sort.Strings(opList)
	t.Logf("seed %d, %d trials × %d steps (%s) — substantive lines:\n%s", *pfuzzSeed, *pfuzzTrials, *pfuzzSteps, strings.Join(opList, ", "), table)
	t.Logf("precision %.3f, recall %.3f", precision, recall)
	t.Logf("trivial lines: precision %.3f, recall %.3f\n%s", tp, tr, ttable)
	for _, w := range st.worst {
		t.Logf("false positive: %s", w)
	}
	if precision < pipelineMinPrecision {
		t.Errorf("precision %.3f below %.2f", precision, pipelineMinPrecision)
	}
	if recall < pipelineMinRecall {
		t.Errorf("recall %.3f below %.2f", recall, pipelineMinRecall)
	}
}
