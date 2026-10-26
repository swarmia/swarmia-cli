//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestClaudeCodeMovedSession runs the moved-session scenario in docs/e2e.md
// against Claude Code, with project hooks in app.
func TestClaudeCodeMovedSession(t *testing.T) {
	runMovedSessionScenario(t, claudeCode{}, movedSession{})
}

// TestClaudeCodeMovedSessionUserHooks runs it with user-scope hooks instead.
func TestClaudeCodeMovedSessionUserHooks(t *testing.T) {
	runMovedSessionScenario(t, claudeCode{}, movedSession{userHooks: true})
}

// TestClaudeCodeMovedSessionBack runs it the other way round, with
// user-scope hooks: started in app, resumed from the workspace.
func TestClaudeCodeMovedSessionBack(t *testing.T) {
	runMovedSessionScenario(t, claudeCode{}, movedSession{userHooks: true, back: true})
}

// TestCodexMovedSession runs the moved-session scenario in docs/e2e.md
// against Codex, with project hooks in app.
func TestCodexMovedSession(t *testing.T) { runMovedSessionScenario(t, codex{}, movedSession{}) }

// TestCodexMovedSessionUserHooks runs it with user-scope hooks instead.
func TestCodexMovedSessionUserHooks(t *testing.T) {
	runMovedSessionScenario(t, codex{}, movedSession{userHooks: true})
}

// TestCodexMovedSessionBack runs it the other way round, with user-scope
// hooks: started in app, resumed from the workspace.
func TestCodexMovedSessionBack(t *testing.T) {
	runMovedSessionScenario(t, codex{}, movedSession{userHooks: true, back: true})
}

// movedSession is a variant of the moved-session scenario.
type movedSession struct {
	// userHooks puts swarmia-cli's hooks in the user's settings instead of
	// app's project settings (the workspace has project hooks either way).
	userHooks bool
	// back starts the session in app and resumes it from the workspace,
	// instead of the other way round.
	back bool
}

// inDir runs f with the agent working in dir instead of the workspace.
func (e *env) inDir(dir string, f func()) {
	e.t.Helper()
	workspace := e.workspace
	e.workspace = dir
	defer func() { e.workspace = workspace }()
	f()
}

// runMovedSessionScenario: a session doesn't stay where it was started. It
// is started in a directory holding several repositories, moves into one of
// them with cd, and is later resumed from a subdirectory of another, where
// the developer has hooks too: the repository's project hooks, or user-scope
// ones, which stand down where the launch directory has its own. Or (back)
// it is started in a repository and resumed from the directory holding it.
// swarmia-cli keeps its state per directory the agent is started in, so a
// resumed session must not end up split across two of them: the server keeps
// only the latest payload of a session, and the two halves would replace each
// other.
func runMovedSessionScenario(t *testing.T, ag agent, v movedSession) {
	if envOr("E2E_AGENT", "claude-code") != ag.name() {
		t.Skipf("E2E_AGENT selects %s", envOr("E2E_AGENT", "claude-code"))
	}
	e := newEnv(t, ag)
	e.workspace = filepath.Join(e.root, "work")
	e.buildSwarmia()
	ag.install(e, envOr(ag.versionVar(), "latest"))
	app := e.newRepo("app", map[string]string{
		"README.md":    "# app\n",
		"src/index.ts": "export const name = 'app'\n",
	})
	lib := e.newRepo("lib", map[string]string{"README.md": "# lib\n"})
	appSrc := filepath.Join(app, "src")
	e.installHooks()
	if v.userHooks {
		e.settingSources = "user,project"
		args := append([]string{"install", "--scope", "user", "--binary", e.swarmia}, ag.installFlags()...)
		t.Logf("swarmia-cli install --scope user:\n%s", e.run(e.workspace, nil, e.swarmia, args...))
		e.inDir(app, func() { ag.prepare(e) }) // Codex: trust app as a project
	} else {
		e.inDir(app, e.installHooks)
	}
	if e.auth.kind == authNone {
		if os.Getenv("E2E_REQUIRE_AUTH") != "" {
			t.Fatal("no credentials for the agent, and E2E_REQUIRE_AUTH is set (see docs/e2e.md)")
		}
		t.Skip("setup OK; no credentials for the agent (see docs/e2e.md), skipping the agent steps")
	}

	// Where the session is started and resumed, and what it's asked to do
	// there. Whichever step runs in the workspace also moves into lib with
	// cd, and writes there from a shell command.
	const moveToLib = "Then run `cd lib` as a shell command on its own. " +
		"Then, as a separate shell command run from there, append the line `Used by app.` to README.md with echo. "
	startDir, resumeDir := e.workspace, appSrc
	prompt1 := "You are in a directory holding several git repositories. " +
		"First, with your file editing tools, create app/src/greet.ts exporting " +
		"`greet(name: string): string` that returns `Hello, ${name}!`. " + moveToLib + "Do not run git commands."
	prompt2 := "Now create farewell.ts in the current directory with your file editing tools, exporting " +
		"`farewell(name: string): string` that returns `Goodbye, ${name}!`. Do not run git commands."
	if v.back {
		startDir, resumeDir = app, e.workspace
		prompt1 = "With your file editing tools, create src/greet.ts exporting " +
			"`greet(name: string): string` that returns `Hello, ${name}!`. Do not run git commands."
		prompt2 = "You are now in the directory holding app and other git repositories. " +
			"First, with your file editing tools, create app/src/farewell.ts exporting " +
			"`farewell(name: string): string` that returns `Goodbye, ${name}!`. " + moveToLib + "Do not run git commands."
	}

	// 1. The developer starts the session.
	w := &agentWork{raw: map[string]bool{}, norm: map[string]bool{}}
	var run1 agentRun
	e.inDir(startDir, func() { run1 = e.agentStep(w, "1-start", "", prompt1) })
	if !fileExists(filepath.Join(appSrc, "greet.ts")) {
		t.Skipf("inconclusive: the agent didn't create greet.ts (result %q, tools %v)", run1.Result, run1.ToolUses)
	}
	t.Logf("step 1: session %s in %s, tools %v", run1.SessionID, startDir, run1.ToolUses)

	// 2. The developer resumes it from another directory.
	var run2 agentRun
	e.inDir(resumeDir, func() { run2 = e.agentStep(w, "2-resume", run1.SessionID, prompt2) })
	resumedID := run2.SessionID
	if run2.StreamSessionID != "" {
		resumedID = run2.StreamSessionID
	}
	t.Logf("step 2: resumed %s from %s as session %s (result %q, tools %v)",
		run1.SessionID, resumeDir, resumedID, run2.Result, run2.ToolUses)
	if ag.name() == "claude-code" { // Codex's stream doesn't show hooks
		t.Logf("hooks started: step 1 %d, step 2 %d", run1.HooksStarted, run2.HooksStarted)
		for _, r := range []struct {
			run agentRun
			dir string
		}{{run1, startDir}, {run2, resumeDir}} {
			if r.run.HooksStarted == 0 {
				t.Errorf("%s ran no hooks in %s: it loads project hooks from the directory it's launched in, "+
					"not the repository root, so that part of the session went unrecorded", ag.name(), r.dir)
			}
		}
	}
	if !fileExists(filepath.Join(appSrc, "farewell.ts")) || !fileContains(t, filepath.Join(lib, "README.md"), "Used by app.") {
		t.Skipf("inconclusive: the agent didn't create farewell.ts and write to lib (result %q: %s)", run2.Result, truncate(run2.Message, 500))
	}
	if resumedID != run1.SessionID {
		t.Logf("%s continued the resumed session under a new id: the halves are two sessions, not a split one", ag.name())
	}

	for _, dir := range []string{app, lib} {
		e.run(dir, nil, "git", "add", "-A")
		e.run(dir, nil, "git", "commit", "-q", "-m", "Greetings")
		e.run(dir, nil, "git", "push", "-q", "origin", "main")
	}

	// What each place the session was started from would send. app and
	// app/src share the repository's state; the workspace has its own.
	ids := []string{run1.SessionID, resumedID}
	type found struct {
		from string
		p    payload
	}
	var parts []found
	for _, dir := range []string{e.workspace, appSrc} {
		name := strings.TrimPrefix(dir, e.root+"/")
		raw := e.run(dir, nil, e.swarmia, "report", "--json", "--limit", "0")
		e.keepBytes(strings.ReplaceAll(name, "/", "_")+".payloads.json", []byte(raw))
		var payloads []payload
		must(t, json.Unmarshal([]byte(raw), &payloads))
		for _, p := range payloads {
			if slices.Contains(ids, p.SessionID) {
				parts = append(parts, found{name, p})
				t.Logf("%s: payload for session %s with %d tool calls, repos %+v", name, p.SessionID, len(p.Tools), p.Repos)
			}
		}
		if strings.Contains(raw, e.root) {
			t.Errorf("%s: the payloads contain local paths", name)
		}
	}
	e.keepStateDirs(ids)

	t.Run("one place per session", func(t *testing.T) {
		count := map[string][]string{}
		for _, f := range parts {
			count[f.p.SessionID] = append(count[f.p.SessionID], f.from)
		}
		for _, id := range slices.Compact(slices.Clone(ids)) {
			switch places := count[id]; len(places) {
			case 0:
				t.Errorf("session %s: no payload anywhere", id)
			case 1:
			default:
				t.Errorf("session %s is split: a payload from each of %v, which would replace each other on the server", id, places)
			}
		}
	})

	t.Run("all its work", func(t *testing.T) {
		if resumedID != run1.SessionID {
			t.Skip("the resumed session has its own id")
		}
		// In one payload: the server keeps a session's latest, so work spread
		// over several isn't all there.
		for _, f := range parts {
			files := map[string]bool{}
			repos := map[string]int{}
			for _, tool := range f.p.Tools {
				if tool.Repo != "" && tool.File != "" {
					files[tool.Repo+":"+tool.File] = true
				}
			}
			for _, r := range f.p.Repos {
				repos[r.Repo] += r.AILinesKept
			}
			for _, want := range []string{"e2e.example/acme/app:src/greet.ts", "e2e.example/acme/app:src/farewell.ts"} {
				if !files[want] {
					t.Errorf("%s: no tool call for %s; got %v", f.from, want, files)
				}
			}
			if repos["e2e.example/acme/app"] == 0 {
				t.Errorf("%s: no committed AI lines in app; repos %v", f.from, repos)
			}
			// The shell write after cd: snapshotted where the command ran.
			if repos["e2e.example/acme/lib"] == 0 {
				t.Errorf("%s: no committed AI lines in lib (written after cd lib); repos %v", f.from, repos)
			}
		}
	})
}

// keepStateDirs logs which of swarmia-cli's state directories logged events
// of the sessions, and keeps their logs as artifacts.
func (e *env) keepStateDirs(ids []string) {
	e.t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(e.root, "state", "repos", "*"))
	for _, dir := range dirs {
		meta, _ := os.ReadFile(filepath.Join(dir, "repo.json"))
		var m struct {
			Repo string `json:"repo"`
		}
		_ = json.Unmarshal(meta, &m)
		log, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
		var stored, logged []string
		for _, id := range slices.Compact(slices.Clone(ids)) {
			if fileExists(filepath.Join(dir, "sessions", id+".json")) {
				stored = append(stored, id)
				e.keep(filepath.Join(dir, "sessions", id+".json"), "state-"+filepath.Base(dir)+"."+id+".json")
			}
			if strings.Contains(string(log), id) {
				logged = append(logged, id)
			}
		}
		e.t.Logf("state %s (%s): sessions stored %v, unfolded events of %v",
			filepath.Base(dir), strings.TrimPrefix(m.Repo, e.root+"/"), stored, logged)
	}
}
