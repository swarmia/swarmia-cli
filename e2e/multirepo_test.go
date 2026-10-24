//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestClaudeCodeMultiRepo runs the multi-repository scenario in docs/e2e.md
// against Claude Code.
func TestClaudeCodeMultiRepo(t *testing.T) { runMultiRepoScenario(t, claudeCode{}) }

// TestCodexMultiRepo runs the multi-repository scenario in docs/e2e.md
// against Codex.
func TestCodexMultiRepo(t *testing.T) { runMultiRepoScenario(t, codex{}) }

// newRepo creates the repository workspace/<name> with files in one commit,
// published as acme/<name>.
func (e *env) newRepo(name string, files map[string]string) string {
	e.t.Helper()
	dir := filepath.Join(e.workspace, name)
	for path, content := range files {
		full := filepath.Join(dir, path)
		must(e.t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(e.t, os.WriteFile(full, []byte(content), 0o644))
	}
	e.run(dir, nil, "git", "init", "-q")
	e.run(dir, nil, "git", "add", "-A")
	e.run(dir, nil, "git", "commit", "-q", "-m", "Initial commit")
	e.addRemote(dir, name)
	return dir
}

// runMultiRepoScenario: the agent is started in a directory holding several
// repositories, which isn't one itself, as developers with many repositories
// do. In one session it edits one repository with its edit tools and writes
// into another through a shell command; a third stays untouched.
func runMultiRepoScenario(t *testing.T, ag agent) {
	if envOr("E2E_AGENT", "claude-code") != ag.name() {
		t.Skipf("E2E_AGENT selects %s", envOr("E2E_AGENT", "claude-code"))
	}
	e := newEnv(t, ag)
	e.workspace = filepath.Join(e.root, "work")
	e.repo = filepath.Join(e.workspace, "ufo")
	e.buildSwarmia()
	ag.install(e, envOr(ag.versionVar(), "latest"))
	e.setupFixture()
	client := e.newRepo("client", map[string]string{
		"README.md":    "# client\n\nA client of ufo.\n",
		"src/index.ts": "export const name = 'client'\n",
	})
	e.newRepo("unrelated", map[string]string{"README.md": "# unrelated\n\nNothing to see here.\n"})
	e.installHooks()
	if e.auth.kind == authNone {
		if os.Getenv("E2E_REQUIRE_AUTH") != "" {
			t.Fatal("no credentials for the agent, and E2E_REQUIRE_AUTH is set (see docs/e2e.md)")
		}
		t.Skip("setup OK; no credentials for the agent (see docs/e2e.md), skipping the agent steps")
	}

	w := &agentWork{raw: map[string]bool{}, norm: map[string]bool{}}
	run := e.agentStep(w, "1-two-repos", "",
		"You are in a directory holding several git repositories. "+
			"First, in ufo/src/utils.ts, next to the other protocol helpers, add and export a function "+
			"`isLocalhostURL(input: string): boolean` that returns true when the URL's hostname is localhost, "+
			"127.0.0.1 or [::1], and false otherwise, including for relative URLs. "+
			"Then create the file client/src/hosts.ts, exporting a constant LOCAL_HOSTS that lists those three "+
			"hostnames, one per line, with a one-line comment above it saying what they are. Create that file with "+
			"a single shell command (cat with a heredoc), not with your file editing tools. "+
			"Do not run git commands, and do not run prettier or any formatter.")
	if !fileContains(t, filepath.Join(e.repo, "src", "utils.ts"), "isLocalhostURL") ||
		!fileContains(t, filepath.Join(client, "src", "hosts.ts"), "LOCAL_HOSTS") {
		t.Skipf("inconclusive: the agent didn't do both parts (result %q, %d turns)", run.Result, run.NumTurns)
	}

	// The developer commits and pushes both repositories.
	repos := map[string]string{"e2e.example/acme/ufo": e.repo, "e2e.example/acme/client": client}
	bases := map[string]string{}
	for slug, dir := range repos {
		bases[slug] = strings.TrimSpace(e.run(dir, nil, "git", "rev-parse", "HEAD"))
		e.run(dir, nil, "git", "add", "-A")
		e.run(dir, nil, "git", "commit", "-q", "-m", "Add localhost helpers")
		e.run(dir, nil, "git", "push", "-q", "origin", "main")
	}

	e.run(e.workspace, nil, e.swarmia, "process")
	e.keepBytes("report.txt", []byte(e.run(e.workspace, nil, e.swarmia, "report", "--limit", "0")))
	raw := e.run(e.workspace, nil, e.swarmia, "report", "--json", "--limit", "0")
	e.keepBytes("payloads.json", []byte(raw))
	var payloads []payload
	must(t, json.Unmarshal([]byte(raw), &payloads))
	var parts []payload
	for _, p := range payloads {
		if p.SessionID == run.SessionID {
			parts = append(parts, p)
		}
	}
	if len(parts) != 1 {
		t.Fatalf("got %d payloads for the session, want one (no tokens: one delivery target)", len(parts))
	}
	p := parts[0]

	m := metrics{Agent: ag.name(), AgentVersion: e.version, Model: envOr("E2E_MODEL", envOr("E2E_CODEX_MODEL", "")),
		CostUSD: run.CostUSD, AgentToolCalls: len(run.ToolUses), CapturedToolCalls: len(p.Tools),
		Sessions: map[string]sessionMetrics{}}
	defer func() { writeMetrics(e, m) }()

	t.Run("capture", func(t *testing.T) {
		streamed, captured := map[string]int{}, map[string]int{}
		for _, name := range run.ToolUses {
			streamed[name]++
		}
		for _, tool := range p.Tools {
			captured[tool.Name]++
		}
		for name, n := range streamed {
			if editOrShellTools[name] && captured[name] < n {
				t.Errorf("captured %d %s calls; the agent's stream shows %d", captured[name], name, n)
			}
		}
		if len(run.HookErrors) > 0 {
			t.Errorf("hooks reported errors: %v", run.HookErrors)
		}
		if !hasTool(p, "Bash") {
			t.Error("no Bash call; client/src/hosts.ts should have been written by a shell command")
		}
		for _, u := range p.Usage {
			m.OutputTokens += u.OutputTokens
		}
		if m.OutputTokens == 0 {
			t.Error("no usage in the payload")
		}
	})

	t.Run("repositories", func(t *testing.T) {
		// Both repositories it wrote in, with lines in each; any other only
		// if a tool call worked in it (the agent may look around).
		listed := map[string]bool{}
		for _, r := range p.Repos {
			listed[r.Repo] = true
			_, wrote := repos[r.Repo]
			switch {
			case wrote && (r.AILines == 0 || r.AILinesKept == 0):
				t.Errorf("%s: %d AI lines, %d committed; want both non-zero", r.Repo, r.AILines, r.AILinesKept)
			case !wrote && !slices.ContainsFunc(p.Tools, func(tl toolPayload) bool { return tl.Repo == r.Repo }):
				t.Errorf("%s listed, but no tool call worked in it", r.Repo)
			}
		}
		for slug := range repos {
			if !listed[slug] {
				t.Errorf("%s not listed in repos", slug)
			}
		}
		for _, tool := range p.Tools {
			switch {
			case tool.File != "" && tool.Repo == "":
				t.Errorf("%s of %q has no repository", tool.Name, tool.File)
			case strings.HasPrefix(tool.File, "ufo/") || strings.HasPrefix(tool.File, "client/") || strings.HasPrefix(tool.File, "/"):
				t.Errorf("%s file %q isn't relative to its repository", tool.Name, tool.File)
			case tool.Repo == "e2e.example/acme/ufo" && editOrShellTools[tool.Name] && tool.File != "" && tool.File != "src/utils.ts":
				t.Errorf("%s edited %s in ufo; want src/utils.ts", tool.Name, tool.File)
			}
		}
	})

	t.Run("attribution", func(t *testing.T) {
		for slug, dir := range repos {
			predicted := map[string]string{} // sha:path:line → class
			for _, c := range p.Commits {
				if c.Repo != slug {
					continue
				}
				for _, r := range c.Lines {
					for n := r.Start; n <= r.End; n++ {
						predicted[fmt.Sprintf("%s:%s:%d", c.SHA, c.Path, n)] = r.Class
					}
				}
			}
			var sm sessionMetrics
			for _, sha := range strings.Fields(e.run(dir, nil, "git", "rev-list", "origin/main", "^"+bases[slug])) {
				for path, lines := range introducedLines(e, dir, sha) {
					for n, text := range lines {
						if normalizeLine(text) == "" {
							continue
						}
						key := fmt.Sprintf("%s:%s:%d", sha, path, n)
						want := w.owns(text, "")
						got, attributed := predicted[key]
						switch {
						case want != "" && attributed:
							sm.TruePositives++
							if got == want {
								sm.ClassAgreements++
							}
						case want != "":
							sm.Missed++
							t.Logf("%s: missed %s %q (expected %s)", slug, key, text, want)
						case attributed:
							sm.FalsePositives++
							t.Logf("%s: attributed a line no agent wrote: %s %q (%s)", slug, key, text, got)
						}
					}
				}
			}
			sm.Precision = ratio(sm.TruePositives, sm.TruePositives+sm.FalsePositives)
			sm.Recall = ratio(sm.TruePositives, sm.TruePositives+sm.Missed)
			sm.ClassAgreement = ratio(sm.ClassAgreements, sm.TruePositives)
			m.Sessions[slug] = sm
			t.Logf("%s: %d lines attributed correctly, %d false positives, %d missed — precision %.3f, recall %.3f, class agreement %.3f",
				slug, sm.TruePositives, sm.FalsePositives, sm.Missed, sm.Precision, sm.Recall, sm.ClassAgreement)
			if sm.TruePositives == 0 {
				t.Errorf("%s: nothing attributed", slug)
			}
			if sm.Precision < 0.95 || sm.Recall < 0.8 || sm.ClassAgreement < 0.8 {
				t.Errorf("%s: precision %.3f, recall %.3f, class agreement %.3f; want at least 0.95, 0.8, 0.8",
					slug, sm.Precision, sm.Recall, sm.ClassAgreement)
			}
		}
	})

	t.Run("payload privacy", func(t *testing.T) {
		if strings.Contains(raw, e.workspace) || strings.Contains(raw, e.home) {
			t.Error("the payload contains local paths")
		}
	})
}
