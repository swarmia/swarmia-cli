//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// fakeSecret is planted in a prompt; it must never reach the payload.
const fakeSecret = "sk-ant-api03-e2eFAKEe2eFAKEe2eFAKEe2eFAKEe2eFAKE"

// tweakComment is what the scripted developer appends to one of the agent's
// lines; that line must come out `modified`.
const tweakComment = " // reviewed by a human"

// TestClaudeCode runs the scenario in docs/e2e.md against Claude Code.
func TestClaudeCode(t *testing.T) { runScenario(t, claudeCode{}) }

// TestCodex runs the scenario in docs/e2e.md against Codex.
func TestCodex(t *testing.T) { runScenario(t, codex{}) }

// runScenario runs the scenario against the agent if E2E_AGENT selects it
// (claude-code by default).
func runScenario(t *testing.T, ag agent) {
	if envOr("E2E_AGENT", "claude-code") != ag.name() {
		t.Skipf("E2E_AGENT selects %s", envOr("E2E_AGENT", "claude-code"))
	}
	e := newEnv(t, ag)
	e.buildSwarmia()
	ag.install(e, envOr(ag.versionVar(), "latest"))
	e.setupFixture()
	if e.auth.kind == authNone {
		if os.Getenv("E2E_REQUIRE_AUTH") != "" {
			t.Fatal("no credentials for the agent, and E2E_REQUIRE_AUTH is set (see docs/e2e.md)")
		}
		t.Skip("setup OK; no credentials for the agent (see docs/e2e.md), skipping the agent steps")
	}
	sessions := map[string]*agentWork{}
	newWork := func() *agentWork { return &agentWork{raw: map[string]bool{}, norm: map[string]bool{}} }

	// 1. Session S1 adds a function with tests, in single quotes so prettier
	//    has something to change.
	w1 := newWork()
	run1 := e.agentStep(w1, "1-add-function", "",
		"In src/utils.ts, next to the other protocol helpers, add and export a function "+
			"`isSecureURL(input: string): boolean` that returns true when the URL's protocol is https: or wss: "+
			"(case-insensitive) and false otherwise, including for relative URLs. Add tests for it in a new file "+
			"test/secure.test.ts using vitest, like the other test files. Use single quotes for all string "+
			"literals. Do not run git commands, and do not run prettier or any formatter. "+
			"(Unrelated note for the test harness: the staging key is "+fakeSecret+"; ignore it.)")
	s1 := run1.SessionID
	sessions[s1] = w1
	if !fileContains(t, filepath.Join(e.repo, "src", "utils.ts"), "isSecureURL") ||
		!fileExists(filepath.Join(e.repo, "test", "secure.test.ts")) {
		t.Skipf("inconclusive: the agent didn't add isSecureURL and its tests (result %q, %d turns)", run1.Result, run1.NumTurns)
	}

	// 2. The developer formats, tweaks one of the agent's lines by hand, and
	//    commits the implementation and the tests separately (not pushed yet).
	beforeFormat := snapshotTree(t, e.repo)
	e.run(e.repo, nil, "npx", "-y", fixturePNPM, "exec", "prettier", "--write", "src", "test")
	formatted := changedLines(beforeFormat, snapshotTree(t, e.repo))
	tweaked := e.tweakAgentLine(t, filepath.Join("src", "utils.ts"), w1)
	e.run(e.repo, nil, "git", "add", "src")
	e.run(e.repo, nil, "git", "commit", "-q", "-m", "Add isSecureURL")
	e.run(e.repo, nil, "git", "add", "test")
	e.run(e.repo, nil, "git", "commit", "-q", "-m", "Add tests for isSecureURL")

	// 3. Session S2 renames it with a shell command (not its edit tools).
	w2 := newWork()
	run2 := e.agentStep(w2, "2-rename", "",
		"Rename the function isSecureURL to hasSecureProtocol everywhere in src/ and test/. Do it with a "+
			"single sed command run through the shell — do not use your file editing tools. Do not run git commands.")
	s2 := run2.SessionID
	sessions[s2] = w2
	if !fileContains(t, filepath.Join(e.repo, "src", "utils.ts"), "hasSecureProtocol") {
		t.Skipf("inconclusive: the agent didn't rename the function (result %q)", run2.Result)
	}
	e.run(e.repo, nil, "git", "add", "-A")
	e.run(e.repo, nil, "git", "commit", "-q", "-m", "Rename to hasSecureProtocol")

	// 4. A teammate pushes; the developer rebases onto it, squashes the last
	//    two commits, amends the message, and pushes.
	f, err := os.OpenFile(filepath.Join(e.team, "README.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	must(t, err)
	f.WriteString("\n<!-- A teammate's note: ufo parses and builds URLs without dependencies. -->\n")
	f.Close()
	e.run(e.team, []string{"GIT_AUTHOR_NAME=Teammate", "GIT_AUTHOR_EMAIL=teammate@e2e.example.com",
		"GIT_COMMITTER_NAME=Teammate", "GIT_COMMITTER_EMAIL=teammate@e2e.example.com"}, "git", "commit", "-q", "-am", "Teammate's README note")
	e.run(e.team, nil, "git", "push", "-q", "origin", "main")
	teammateCommit := strings.TrimSpace(e.run(e.team, nil, "git", "rev-parse", "HEAD"))
	e.run(e.repo, nil, "git", "fetch", "-q", "origin")
	e.run(e.repo, nil, "git", "rebase", "-q", "origin/main")
	e.run(e.repo, nil, "git", "reset", "-q", "--soft", "HEAD~2")
	e.run(e.repo, nil, "git", "commit", "-q", "-m", "Add tests and rename")
	e.run(e.repo, nil, "git", "commit", "-q", "--amend", "-m", "Add tests, and rename isSecureURL to hasSecureProtocol")
	e.run(e.repo, nil, "git", "push", "-q", "origin", "main")

	// 5. S1 is resumed for a follow-up; the developer commits and pushes.
	run3 := e.agentStep(w1, "3-resume", s1,
		"Now add and export a function `isInsecureURL(input: string): boolean` in src/utils.ts that returns "+
			"the opposite of hasSecureProtocol for absolute URLs and false for relative ones, and add a test for it "+
			"in test/secure.test.ts. Use single quotes. Do not run git commands or any formatter.")
	resumed := strings.TrimSpace(e.run(e.repo, nil, "git", "status", "--porcelain")) != ""
	if resumed {
		e.run(e.repo, nil, "git", "add", "-A")
		e.run(e.repo, nil, "git", "commit", "-q", "-m", "Document hasSecureProtocol")
		e.run(e.repo, nil, "git", "push", "-q", "origin", "main")
	}

	// Bring swarmia-cli's store up to date and take the payloads it would send.
	e.run(e.repo, nil, e.swarmia, "process")
	e.keepBytes("report.txt", []byte(e.run(e.repo, nil, e.swarmia, "report", "--limit", "0")))
	raw := e.run(e.repo, nil, e.swarmia, "report", "--json", "--limit", "0")
	e.keepBytes("payloads.json", []byte(raw))
	var payloads []payload
	must(t, json.Unmarshal([]byte(raw), &payloads))
	p1, p2 := findPayload(t, payloads, s1), findPayload(t, payloads, s2)

	m := metrics{Agent: ag.name(), AgentVersion: e.version, Model: envOr("E2E_MODEL", envOr("E2E_CODEX_MODEL", "")),
		CostUSD:        run1.CostUSD + run2.CostUSD + run3.CostUSD,
		AgentToolCalls: len(run1.ToolUses) + len(run2.ToolUses) + len(run3.ToolUses),
		Sessions:       map[string]sessionMetrics{}}
	defer func() { writeMetrics(e, m) }()

	t.Run("capture", func(t *testing.T) {
		for _, c := range []struct {
			name string
			p    payload
			runs []agentRun
		}{{"S1", p1, []agentRun{run1, run3}}, {"S2", p2, []agentRun{run2}}} {
			// Every edit and shell call must be captured: attribution depends on
			// them. Other tools are only reported, since agents have stopped
			// running hooks for some read-only ones (Claude Code 2.1.293 ran
			// none for a Grep).
			streamed, captured := map[string]int{}, map[string]int{}
			for _, r := range c.runs {
				for _, name := range r.ToolUses {
					streamed[name]++
				}
				if len(r.HookErrors) > 0 {
					t.Errorf("%s: hooks reported errors: %v", c.name, r.HookErrors)
				}
			}
			for _, tool := range c.p.Tools {
				captured[tool.Name]++
			}
			m.CapturedToolCalls += len(c.p.Tools)
			if len(streamed) == 0 {
				t.Errorf("%s: the agent's streams show no tool calls", c.name)
			}
			for name, n := range streamed {
				switch {
				case captured[name] >= n:
				case editOrShellTools[name]:
					t.Errorf("%s: captured %d %s calls; the agent's streams show %d", c.name, captured[name], name, n)
				default:
					m.UncapturedToolCalls += n - captured[name]
					t.Logf("%s: captured %d %s calls; the agent's streams show %d (no hooks ran for the rest)", c.name, captured[name], name, n)
				}
			}
			if len(c.p.Prompts) != len(c.runs) {
				t.Errorf("%s: %d prompts captured, want %d", c.name, len(c.p.Prompts), len(c.runs))
			}
			if !c.p.Ended || c.p.Model == "" || c.p.Agent != ag.name() {
				t.Errorf("%s: ended=%v model=%q agent=%q", c.name, c.p.Ended, c.p.Model, c.p.Agent)
			}
		}
		if !hasTool(p2, "Bash") {
			t.Errorf("S2 has no Bash call; the rename should have been a shell command")
		}
	})

	t.Run("usage", func(t *testing.T) {
		for name, c := range map[string]struct {
			p    payload
			runs []agentRun
		}{"S1": {p1, []agentRun{run1, run3}}, "S2": {p2, []agentRun{run2}}} {
			var transcripts []string
			for _, r := range c.runs {
				if r.Transcript != "" && !slices.Contains(transcripts, r.Transcript) {
					transcripts = append(transcripts, r.Transcript)
				}
			}
			if len(transcripts) == 0 {
				t.Errorf("%s: transcript not found", name)
				continue
			}
			want := ag.outputTokens(t, transcripts)
			var got int64
			for _, u := range c.p.Usage {
				got += u.OutputTokens
			}
			m.OutputTokens += got
			if got == 0 || got != want {
				t.Errorf("%s: output tokens: payload %d, independent count over the transcript %d", name, got, want)
			}
		}
	})

	t.Run("attribution", func(t *testing.T) {
		reachable := map[string]bool{}
		for _, sha := range strings.Fields(e.run(e.repo, nil, "git", "rev-list", "origin/main")) {
			reachable[sha] = true
		}
		// The commits made during the scenario (the fixture's own base commit
		// introduced the whole repository, agent-like lines included).
		pushed := strings.Fields(e.run(e.repo, nil, "git", "rev-list", "origin/main", "^"+fixtureSHA))
		introduced := map[string]map[string]map[int]string{} // sha → path → line → text
		for _, p := range []payload{p1, p2} {
			for _, c := range p.Commits {
				if !reachable[c.SHA] {
					t.Errorf("attribution on %s, which isn't on origin/main", c.SHA[:7])
				}
				if c.SHA == teammateCommit {
					t.Errorf("attribution on the teammate's commit")
				}
				if introduced[c.SHA] == nil {
					introduced[c.SHA] = introducedLines(e, c.SHA)
				}
			}
		}
		anyAgent := func(text string) bool {
			for _, w := range sessions {
				if w.owns(text, tweaked) != "" {
					return true
				}
			}
			return false
		}
		sawModified, sawNormalized := false, false
		for name, c := range map[string]struct {
			id string
			p  payload
		}{"S1": {s1, p1}, "S2": {s2, p2}} {
			w := sessions[c.id]
			predicted := map[string]string{} // sha:path:line → class
			for _, cm := range c.p.Commits {
				for _, r := range cm.Lines {
					for n := r.Start; n <= r.End; n++ {
						predicted[fmt.Sprintf("%s:%s:%d", cm.SHA, cm.Path, n)] = r.Class
					}
				}
			}
			var sm sessionMetrics
			// Expected: lines the pushed commits introduced that this session
			// wrote. Every commit with any attribution is examined, plus all of
			// origin/main's own commits for recall.
			for _, sha := range pushed {
				if sha == teammateCommit {
					continue
				}
				if introduced[sha] == nil {
					introduced[sha] = introducedLines(e, sha)
				}
				for path, lines := range introduced[sha] {
					for n, text := range lines {
						if normalizeLine(text) == "" {
							continue // trivial: text can't tell who wrote it
						}
						key := fmt.Sprintf("%s:%s:%d", sha, path, n)
						want := w.owns(text, tweaked)
						got, attributed := predicted[key]
						switch {
						case want != "" && attributed:
							sm.TruePositives++
							if got == want {
								sm.ClassAgreements++
							} else {
								t.Logf("%s: %s %q is %s, expected %s", name, key, text, got, want)
							}
							sawModified = sawModified || (got == "modified" && want == "modified")
							sawNormalized = sawNormalized || (got == "normalized" && want == "normalized")
						case want != "":
							sm.Missed++
							t.Logf("%s: missed %s %q (expected %s)", name, key, text, want)
						case attributed && (!anyAgent(text) || w.preexisting[strings.TrimRight(text, " \t")]):
							// No agent wrote it, or it was there before this
							// session started and the session didn't write it.
							// (Another session's line this one later changed is
							// fine: many-to-many.)
							sm.FalsePositives++
							t.Logf("%s: attributed a line it didn't write: %s %q (%s)", name, key, text, got)
						}
					}
				}
			}
			sm.Precision = ratio(sm.TruePositives, sm.TruePositives+sm.FalsePositives)
			sm.Recall = ratio(sm.TruePositives, sm.TruePositives+sm.Missed)
			sm.ClassAgreement = ratio(sm.ClassAgreements, sm.TruePositives)
			m.Sessions[name] = sm
			t.Logf("%s: %d lines attributed correctly, %d false positives, %d missed — precision %.3f, recall %.3f, class agreement %.3f",
				name, sm.TruePositives, sm.FalsePositives, sm.Missed, sm.Precision, sm.Recall, sm.ClassAgreement)
			if sm.TruePositives == 0 {
				t.Errorf("%s: nothing attributed", name)
			}
			if sm.Precision < 0.95 {
				t.Errorf("%s: precision %.3f below 0.95", name, sm.Precision)
			}
			if sm.Recall < 0.8 {
				t.Errorf("%s: recall %.3f below 0.8", name, sm.Recall)
			}
			if sm.ClassAgreement < 0.8 {
				t.Errorf("%s: class agreement %.3f below 0.8", name, sm.ClassAgreement)
			}
		}
		if tweaked != "" && !sawModified {
			t.Errorf("the hand-tweaked line %q never came out modified", tweaked)
		}
		if len(formatted) > 0 && !sawNormalized {
			t.Errorf("prettier changed %d lines, but none came out normalized", len(formatted))
		}
		m.Resumed = resumed
	})

	t.Run("payload privacy", func(t *testing.T) {
		if strings.Contains(raw, fakeSecret) {
			t.Error("the planted secret reached the payload")
		}
		if len(p1.Prompts) == 0 || !strings.Contains(p1.Prompts[0].Text, "[REDACTED:") {
			t.Errorf("prompt not redacted: %+v", p1.Prompts)
		}
		// The repository and home directories are masked. (The sandbox's own
		// root above them isn't: agents probe parent directories for
		// AGENTS.md, and in real use those are under the home directory.)
		if strings.Contains(raw, e.repo) || strings.Contains(raw, e.home) {
			t.Error("the payload contains local paths")
		}
		for _, p := range []payload{p1, p2} {
			for _, tool := range p.Tools {
				if strings.HasPrefix(tool.File, "/") {
					t.Errorf("absolute tool file path in payload: %q", tool.File)
				}
			}
		}
	})
}

// --- the agent's work, per session ----------------------------------------------

// agentWork is every line a session put into files: from its transcript's
// edit-tool inputs, and from the working tree's changes across its runs
// (which also covers shell edits). Both raw and normalized.
type agentWork struct {
	raw  map[string]bool // trailing-whitespace-trimmed text
	norm map[string]bool // normalizeLine text
	// preexisting holds every line in the working tree before the session's
	// first run: lines the session can't have written unless it wrote them
	// again itself.
	preexisting map[string]bool
}

func (w *agentWork) add(line string) {
	if n := normalizeLine(line); n != "" {
		w.raw[strings.TrimRight(line, " \t")] = true
		w.norm[n] = true
	}
}

// owns returns the class a committed line should have for this session:
// "exact" (text as the agent wrote it), "normalized" (only formatting
// differs), "modified" (the developer's hand-tweaked line), or "".
func (w *agentWork) owns(text, tweaked string) string {
	switch t := strings.TrimRight(text, " \t"); {
	case w.raw[t]:
		return "exact"
	case w.norm[normalizeLine(text)]:
		return "normalized"
	case tweaked != "" && t == strings.TrimRight(tweaked, " \t") && w.norm[normalizeLine(strings.TrimSuffix(t, tweakComment))]:
		return "modified"
	}
	return ""
}

// agentStep runs the agent (a new session, or resuming sessionID) and records
// what it wrote: the working tree's changes during the run, and (after the
// run) its transcript's tool inputs.
func (e *env) agentStep(w *agentWork, step, sessionID, prompt string) agentRun {
	e.t.Helper()
	before := snapshotTree(e.t, e.repo)
	if w.preexisting == nil {
		w.preexisting = map[string]bool{}
		for _, content := range before {
			for _, l := range strings.Split(content, "\n") {
				w.preexisting[strings.TrimRight(l, " \t")] = true
			}
		}
	}
	r := e.agent.run(e, step, sessionID, prompt)
	// An agent that never reached the model (failed token exchange, API
	// outage) is a failure, not an inconclusive run where the agent just
	// didn't do the task.
	if !r.ReachedModel {
		e.t.Fatalf("%s: the agent got no model response: %s", step, r.Message)
	}
	for _, l := range changedLines(before, snapshotTree(e.t, e.repo)) {
		w.add(l)
	}
	if r.Transcript != "" {
		for _, l := range e.agent.writes(e.t, r.Transcript) {
			w.add(l)
		}
	}
	return r
}

// tweakAgentLine appends a comment to the longest line in file that the
// session wrote, as a developer touching up the agent's code would, and
// returns the new line ("" if there was none to tweak).
func (e *env) tweakAgentLine(t *testing.T, file string, w *agentWork) string {
	t.Helper()
	path := filepath.Join(e.repo, file)
	data, err := os.ReadFile(path)
	must(t, err)
	lines := strings.Split(string(data), "\n")
	best := -1
	for i, l := range lines {
		if w.owns(l, "") != "" && !strings.Contains(l, "//") && len(strings.Fields(l)) >= 3 && (best < 0 || len(l) > len(lines[best])) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	lines[best] = strings.TrimRight(lines[best], " \t") + tweakComment
	must(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644))
	return lines[best]
}

// snapshotTree reads the fixture's own files (not dependencies or git data).
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".claude", ".codex":
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() < 1<<20 {
			if data, err := os.ReadFile(path); err == nil {
				rel, _ := filepath.Rel(root, path)
				files[rel] = string(data)
			}
		}
		return nil
	})
	return files
}

// changedLines lists lines present after but not before (per file, as a
// multiset), across all files.
func changedLines(before, after map[string]string) []string {
	var out []string
	paths := make([]string, 0, len(after))
	for p := range after {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if after[p] == before[p] {
			continue
		}
		count := map[string]int{}
		for _, l := range strings.Split(before[p], "\n") {
			count[l]++
		}
		for _, l := range strings.Split(after[p], "\n") {
			if count[l] > 0 {
				count[l]--
				continue
			}
			out = append(out, l)
		}
	}
	return out
}

// editOrShellTools are the tools whose calls attribution needs.
var editOrShellTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "Bash": true, "apply_patch": true}

func hasTool(p payload, name string) bool {
	for _, t := range p.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- payload shape (the parts the checks read) -------------------------------

type payload struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Model     string `json:"model"`
	Ended     bool   `json:"ended"`
	Prompts   []struct {
		Text string `json:"text"`
	} `json:"prompts"`
	Tools []struct {
		Name    string `json:"name"`
		File    string `json:"file"`
		Command string `json:"command"`
	} `json:"tools"`
	Commits []struct {
		SHA   string `json:"sha"`
		Path  string `json:"path"`
		Lines []struct {
			Start int    `json:"start"`
			End   int    `json:"end"`
			Class string `json:"class"`
		} `json:"lines"`
	} `json:"commits"`
	Usage []struct {
		Model        string `json:"model"`
		OutputTokens int64  `json:"output_tokens"`
	} `json:"usage"`
}

func findPayload(t *testing.T, payloads []payload, sessionID string) payload {
	t.Helper()
	for _, p := range payloads {
		if p.SessionID == sessionID {
			return p
		}
	}
	t.Fatalf("no payload for session %s (got %d payloads)", sessionID, len(payloads))
	return payload{}
}

// --- independent oracles --------------------------------------------------------

// normalizeLine ignores what a formatter changes: whitespace, quote style,
// trailing commas and semicolons. Lines with too little substance to say
// who wrote them (`}`, `});`) normalize to "".
func normalizeLine(s string) string {
	var b strings.Builder
	alnum := 0
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
		case r == '"' || r == '`':
			b.WriteByte('\'')
		default:
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				alnum++
			}
			b.WriteRune(r)
		}
	}
	if alnum < 3 {
		return ""
	}
	return strings.TrimRight(b.String(), ",;")
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// introducedLines lists the non-blank lines a commit added, per path and line.
func introducedLines(e *env, commit string) map[string]map[int]string {
	out := e.run(e.repo, nil, "git", "show", "--format=", "-U0", "--no-color", commit)
	result := map[string]map[int]string{}
	var path string
	n := 0
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ b/"):
			path = strings.TrimPrefix(l, "+++ b/")
			result[path] = map[int]string{}
		case strings.HasPrefix(l, "+++ "):
			path = "" // deleted file
		case strings.HasPrefix(l, "@@"):
			if m := hunkHeader.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "+"):
			if text := l[1:]; strings.TrimSpace(text) != "" && path != "" {
				result[path][n] = text
			}
			n++
		}
	}
	return result
}

func fileContains(t *testing.T, path, needle string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), needle)
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}

// --- metrics ------------------------------------------------------------------------

type sessionMetrics struct {
	TruePositives   int     `json:"true_positives"`
	FalsePositives  int     `json:"false_positives"`
	Missed          int     `json:"missed"`
	ClassAgreements int     `json:"class_agreements"`
	Precision       float64 `json:"precision"`
	Recall          float64 `json:"recall"`
	ClassAgreement  float64 `json:"class_agreement"`
}

type metrics struct {
	Agent             string  `json:"agent"`
	AgentVersion      string  `json:"agent_version"`
	Model             string  `json:"model"`
	CostUSD           float64 `json:"cost_usd"`
	AgentToolCalls    int     `json:"agent_tool_calls"`
	CapturedToolCalls int     `json:"captured_tool_calls"`
	// UncapturedToolCalls are read-only calls no hook ran for.
	UncapturedToolCalls int                       `json:"uncaptured_tool_calls"`
	OutputTokens        int64                     `json:"output_tokens"`
	Resumed             bool                      `json:"resumed_session_committed"`
	Sessions            map[string]sessionMetrics `json:"sessions"`
}

func writeMetrics(e *env, m metrics) {
	data, _ := json.MarshalIndent(m, "", "  ")
	e.t.Logf("metrics: %s", data)
	e.keepBytes("metrics.json", data)
}
