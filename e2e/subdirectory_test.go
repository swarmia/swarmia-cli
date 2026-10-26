//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestClaudeCodeSubdirectoryUserHooks runs the subdirectory scenario in
// docs/e2e.md with swarmia-cli's hooks in the user's settings.
func TestClaudeCodeSubdirectoryUserHooks(t *testing.T) { runSubdirectoryScenario(t, true) }

// TestClaudeCodeSubdirectoryProjectHooks runs it with the hooks in the
// repository's project settings, at its root. Claude Code loads project
// settings only from the directory it's launched in, so the part of the
// session launched in a subdirectory goes unrecorded, and this fails.
func TestClaudeCodeSubdirectoryProjectHooks(t *testing.T) { runSubdirectoryScenario(t, false) }

// setupMonorepo creates the developer's clone as a small monorepo, with a
// bare origin to push to.
func (e *env) setupMonorepo() {
	e.t.Helper()
	files := map[string]string{
		"package.json":            "{\"name\": \"mono\", \"private\": true, \"workspaces\": [\"apps/*\"]}\n",
		"apps/web/package.json":   "{\"name\": \"web\"}\n",
		"apps/web/src/index.ts":   "export const name = 'web'\n",
		"apps/admin/package.json": "{\"name\": \"admin\"}\n",
		"apps/admin/src/index.ts": "export const name = 'admin'\n",
	}
	for path, content := range files {
		full := filepath.Join(e.repo, path)
		must(e.t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(e.t, os.WriteFile(full, []byte(content), 0o644))
	}
	e.run(e.repo, nil, "git", "init", "-q")
	e.run(e.repo, nil, "git", "add", "-A")
	e.run(e.repo, nil, "git", "commit", "-q", "-m", "Initial commit")
	e.run(e.root, nil, "git", "clone", "-q", "--bare", e.repo, e.origin)
	e.run(e.repo, nil, "git", "remote", "add", "origin", e.origin)
	e.run(e.repo, nil, "git", "fetch", "-q", "origin")
	e.run(e.repo, nil, "git", "branch", "-q", "--set-upstream-to=origin/main", "main")
}

// runSubdirectoryScenario: in a monorepo, a developer launches Claude Code in
// one app's directory, and later resumes the session from the repository
// root. Every part of the session should be recorded, in one session.
func runSubdirectoryScenario(t *testing.T, userHooks bool) {
	if envOr("E2E_AGENT", "claude-code") != "claude-code" {
		t.Skipf("E2E_AGENT selects %s", envOr("E2E_AGENT", "claude-code"))
	}
	ag := claudeCode{}
	e := newEnv(t, ag)
	e.buildSwarmia()
	ag.install(e, envOr(ag.versionVar(), "latest"))
	e.setupMonorepo()
	if userHooks {
		e.settingSources = "user,project"
		t.Logf("swarmia-cli install --scope user:\n%s",
			e.run(e.repo, nil, e.swarmia, "install", "--scope", "user", "--binary", e.swarmia))
	} else {
		// install detects the agent by its settings directory.
		must(t, os.MkdirAll(filepath.Join(e.repo, ag.projectDir()), 0o755))
		exclude := filepath.Join(e.repo, ".git", "info", "exclude")
		f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		must(t, err)
		f.WriteString("\n" + ag.projectDir() + "/\n")
		f.Close()
		t.Logf("swarmia-cli install --scope project:\n%s",
			e.run(e.repo, nil, e.swarmia, "install", "--scope", "project", "--dir", e.repo, "--binary", e.swarmia))
	}
	if e.auth.kind == authNone {
		if os.Getenv("E2E_REQUIRE_AUTH") != "" {
			t.Fatal("no credentials for the agent, and E2E_REQUIRE_AUTH is set (see docs/e2e.md)")
		}
		t.Skip("setup OK; no credentials for the agent (see docs/e2e.md), skipping the agent steps")
	}

	// 1. Launched in apps/web.
	web := filepath.Join(e.repo, "apps", "web")
	w := &agentWork{raw: map[string]bool{}, norm: map[string]bool{}}
	e.cwd = web
	run1 := e.agentStep(w, "1-in-app", "",
		"With your file editing tools, create src/greet.ts exporting `greet(name: string): string` that "+
			"returns `Hello, ${name}!`. Do not run git commands.")
	if !fileExists(filepath.Join(web, "src", "greet.ts")) {
		t.Skipf("inconclusive: the agent didn't create src/greet.ts (result %q, tools %v)", run1.Result, run1.ToolUses)
	}

	// 2. Resumed from the repository root.
	e.cwd = e.repo
	run2 := e.agentStep(w, "2-resume-at-root", run1.SessionID,
		"Now, with your file editing tools, create apps/web/src/farewell.ts exporting "+
			"`farewell(name: string): string` that returns `Goodbye, ${name}!`. Do not run git commands.")
	if !fileExists(filepath.Join(web, "src", "farewell.ts")) {
		t.Skipf("inconclusive: the resumed session didn't create farewell.ts (result %q, tools %v)", run2.Result, run2.ToolUses)
	}
	t.Logf("hooks started: launched in apps/web %d, resumed at the root %d", run1.HooksStarted, run2.HooksStarted)

	e.run(e.repo, nil, "git", "add", "-A")
	e.run(e.repo, nil, "git", "commit", "-q", "-m", "Add greetings")
	e.run(e.repo, nil, "git", "push", "-q", "origin", "main")

	raw := e.run(e.repo, nil, e.swarmia, "report", "--json", "--limit", "0")
	e.keepBytes("payloads.json", []byte(raw))
	var payloads []payload
	must(t, json.Unmarshal([]byte(raw), &payloads))

	t.Run("hooks ran in apps/web", func(t *testing.T) {
		if run1.HooksStarted == 0 {
			t.Errorf("Claude Code ran no hooks for the session launched in apps/web: it loads project settings only " +
				"from the directory it's launched in, not from the repository root above it")
		}
	})

	t.Run("one session with all its work", func(t *testing.T) {
		var p *payload
		for i := range payloads {
			if payloads[i].SessionID == run1.SessionID {
				if p != nil {
					t.Fatalf("session %s has more than one payload", run1.SessionID)
				}
				p = &payloads[i]
			}
		}
		if p == nil {
			t.Fatalf("no payload for session %s (got %d payloads)", run1.SessionID, len(payloads))
		}
		tools, committed := map[string]bool{}, map[string]bool{}
		for _, tool := range p.Tools {
			tools[tool.File] = true
		}
		for _, c := range p.Commits {
			committed[c.Path] = true
		}
		for _, path := range []string{"apps/web/src/greet.ts", "apps/web/src/farewell.ts"} {
			if !tools[path] {
				t.Errorf("no tool call for %s; tool files %v", path, tools)
			}
			if !committed[path] {
				t.Errorf("no committed AI lines in %s; committed files %v", path, committed)
			}
		}
	})
}
