//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// fakeSecret is planted in a prompt; it must never reach the payload.
const fakeSecret = "sk-ant-api03-e2eFAKEe2eFAKEe2eFAKEe2eFAKEe2eFAKE"

// TestClaudeCode runs the scenario in docs/e2e.md against Claude Code.
func TestClaudeCode(t *testing.T) {
	e := newEnv(t)
	e.buildSwarmia()
	e.installClaude(envOr("E2E_CLAUDE_VERSION", "latest"))
	e.setupFixture()
	if e.auth.kind == authNone {
		if os.Getenv("E2E_REQUIRE_AUTH") != "" {
			t.Fatal("no credentials for the agent, and E2E_REQUIRE_AUTH is set (see docs/e2e.md)")
		}
		t.Skip("setup OK; no credentials for the agent (see docs/e2e.md), skipping the agent steps")
	}

	// 1. The agent adds a function with tests.
	s1 := newSessionID()
	run1 := e.runAgent("1-add-function", s1,
		"In src/utils.ts, next to the other protocol helpers, add and export a function "+
			"`isSecureURL(input: string): boolean` that returns true when the URL's protocol is https: or wss: "+
			"(case-insensitive) and false otherwise, including for relative URLs. Add tests for it in a new file "+
			"test/secure.test.ts using vitest, like the other test files. Do not run git commands. "+
			"(Unrelated note for the test harness: the staging key is "+fakeSecret+"; ignore it.)",
		false)
	if !fileContains(t, filepath.Join(e.repo, "src", "utils.ts"), "isSecureURL") {
		t.Skipf("inconclusive: the agent didn't add isSecureURL (result %q, %d turns)", run1.Result, run1.NumTurns)
	}

	// 2. The developer formats, commits and pushes.
	e.run(e.repo, nil, "npx", "-y", fixturePNPM, "exec", "prettier", "--write", "src", "test")
	e.run(e.repo, nil, "git", "add", "-A")
	e.run(e.repo, nil, "git", "commit", "-q", "-m", "Add isSecureURL")
	e.run(e.repo, nil, "git", "push", "-q", "origin", "main")
	commit := strings.TrimSpace(e.run(e.repo, nil, "git", "rev-parse", "HEAD"))

	// 3. Bring swarmia-cli's store up to date and take the payloads it would send.
	e.run(e.repo, nil, e.swarmia, "process")
	report := e.run(e.repo, nil, e.swarmia, "report", "--limit", "0")
	e.keepBytes("report.txt", []byte(report))
	raw := e.run(e.repo, nil, e.swarmia, "report", "--json", "--limit", "0")
	e.keepBytes("payloads.json", []byte(raw))
	var payloads []payload
	must(t, json.Unmarshal([]byte(raw), &payloads))
	p := findPayload(t, payloads, s1)

	m := metrics{ClaudeVersion: e.version, Model: envOr("E2E_MODEL", "sonnet"), CostUSD: run1.CostUSD, AgentToolCalls: len(run1.ToolUses)}
	defer func() { writeMetrics(e, m) }()

	t.Run("capture", func(t *testing.T) {
		if len(run1.HookErrors) > 0 {
			t.Errorf("hooks reported errors: %v", run1.HookErrors)
		}
		m.CapturedToolCalls = len(p.Tools)
		if len(p.Tools) < len(run1.ToolUses) || len(run1.ToolUses) == 0 {
			t.Errorf("captured %d tool calls; the agent's stream shows %d", len(p.Tools), len(run1.ToolUses))
		}
		if len(p.Prompts) != 1 || !strings.Contains(p.Prompts[0].Text, "isSecureURL") {
			t.Errorf("prompts = %+v, want the one task prompt", p.Prompts)
		}
		if !p.Ended || p.Model == "" || p.Agent != "claude-code" {
			t.Errorf("session ended=%v model=%q agent=%q", p.Ended, p.Model, p.Agent)
		}
	})

	t.Run("usage", func(t *testing.T) {
		if run1.Transcript == "" {
			t.Fatal("transcript not found")
		}
		want := independentOutputTokens(t, run1.Transcript)
		var got int64
		for _, u := range p.Usage {
			got += u.OutputTokens
		}
		m.OutputTokens = got
		if got == 0 || got != want {
			t.Errorf("output tokens: payload %d, independent count over the transcript %d", got, want)
		}
	})

	t.Run("attribution", func(t *testing.T) {
		agentLines := agentWrittenLines(t, run1.Transcript)
		introduced := introducedLines(e, commit)
		attributed := map[string]map[int]string{}
		for _, c := range p.Commits {
			if c.SHA != commit {
				t.Errorf("attribution on %s, which isn't the pushed commit %s", c.SHA[:7], commit[:7])
				continue
			}
			if attributed[c.Path] == nil {
				attributed[c.Path] = map[int]string{}
			}
			for _, r := range c.Lines {
				for n := r.Start; n <= r.End; n++ {
					attributed[c.Path][n] = r.Class
				}
			}
		}
		// Trivial lines (`}`, `});`) can't be told apart by text, so the
		// oracle can't judge them; they're left out on both sides.
		var tp, fp, fn int
		for path, lines := range introduced {
			for n, text := range lines {
				if normalizeLine(text) == "" {
					continue
				}
				_, got := attributed[path][n]
				want := agentLines[normalizeLine(text)]
				switch {
				case got && want:
					tp++
				case got:
					fp++
					t.Logf("attributed but not the agent's: %s:%d %q", path, n, text)
				case want:
					fn++
				}
			}
		}
		for path, lines := range attributed {
			for n := range lines {
				if text, ok := introduced[path][n]; ok && normalizeLine(text) == "" {
					continue
				}
				if _, ok := introduced[path][n]; !ok {
					fp++
					t.Logf("attributed a line the commit didn't introduce: %s:%d", path, n)
				}
			}
		}
		m.AttributedLines, m.Precision, m.Recall = tp+fp, ratio(tp, tp+fp), ratio(tp, tp+fn)
		t.Logf("attribution: %d agent lines attributed, %d false positives, %d missed — precision %.3f, recall %.3f",
			tp, fp, fn, m.Precision, m.Recall)
		if tp == 0 {
			t.Fatal("nothing attributed")
		}
		if m.Precision < 0.95 {
			t.Errorf("precision %.3f below 0.95", m.Precision)
		}
		if m.Recall < 0.8 {
			t.Errorf("recall %.3f below 0.8", m.Recall)
		}
	})

	t.Run("payload privacy", func(t *testing.T) {
		if strings.Contains(raw, fakeSecret) {
			t.Error("the planted secret reached the payload")
		}
		if !strings.Contains(p.Prompts[0].Text, "[REDACTED:") {
			t.Errorf("prompt not redacted: %q", p.Prompts[0].Text)
		}
		if strings.Contains(raw, e.root) || strings.Contains(raw, e.home) {
			t.Error("the payload contains local paths")
		}
		for _, tool := range p.Tools {
			if strings.HasPrefix(tool.File, "/") {
				t.Errorf("absolute tool file path in payload: %q", tool.File)
			}
		}
	})
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

// independentOutputTokens sums output tokens over the transcript, counting
// each API response once — written without internal/usage, to check it.
func independentOutputTokens(t *testing.T, transcript string) int64 {
	t.Helper()
	f, err := os.Open(transcript)
	must(t, err)
	defer f.Close()
	seen := map[string]bool{}
	var total int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var l struct {
			Type      string `json:"type"`
			RequestID string `json:"requestId"`
			Message   struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage *struct {
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "assistant" || l.Message.Usage == nil || strings.HasPrefix(l.Message.Model, "<") {
			continue
		}
		key := l.Message.ID + ":" + l.RequestID
		if !seen[key] {
			seen[key] = true
			total += l.Message.Usage.OutputTokens
		}
	}
	return total
}

// agentWrittenLines collects every line the agent put into files through its
// edit tools, normalized, from the transcript's tool inputs.
func agentWrittenLines(t *testing.T, transcript string) map[string]bool {
	t.Helper()
	f, err := os.Open(transcript)
	must(t, err)
	defer f.Close()
	lines := map[string]bool{}
	add := func(s string) {
		for _, l := range strings.Split(s, "\n") {
			if n := normalizeLine(l); n != "" {
				lines[n] = true
			}
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var l struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Input struct {
						Content   string `json:"content"`
						NewString string `json:"new_string"`
						Edits     []struct {
							NewString string `json:"new_string"`
						} `json:"edits"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "assistant" {
			continue
		}
		for _, c := range l.Message.Content {
			if c.Type != "tool_use" {
				continue
			}
			add(c.Input.Content)
			add(c.Input.NewString)
			for _, ed := range c.Input.Edits {
				add(ed.NewString)
			}
		}
	}
	return lines
}

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
		case strings.HasPrefix(l, "@@"):
			if m := hunkHeader.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
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

type metrics struct {
	ClaudeVersion     string  `json:"claude_version"`
	Model             string  `json:"model"`
	CostUSD           float64 `json:"cost_usd"`
	AgentToolCalls    int     `json:"agent_tool_calls"`
	CapturedToolCalls int     `json:"captured_tool_calls"`
	OutputTokens      int64   `json:"output_tokens"`
	AttributedLines   int     `json:"attributed_lines"`
	Precision         float64 `json:"precision"`
	Recall            float64 `json:"recall"`
}

func writeMetrics(e *env, m metrics) {
	data, _ := json.MarshalIndent(m, "", "  ")
	e.t.Logf("metrics: %s", data)
	e.keepBytes("metrics.json", data)
}
