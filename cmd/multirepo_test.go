package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swarmia/swarmia-cli/internal/payload"
)

// agentWritesAndCommits logs, through log-hook, a Claude Code Write of file
// (absolute) in session s, then writes it and commits it in repo. It returns
// the commit.
func agentWritesAndCommits(t *testing.T, repo, file, content string) string {
	t.Helper()
	event, _ := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": file, "content": content},
	})
	if err := runLogHook(bytes.NewReader(event)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "agent's work")
	return git(t, repo, "rev-parse", "HEAD")
}

// newRepoAt is initRepo in a given directory.
func newRepoAt(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func runReportFor(t *testing.T, json bool) string {
	t.Helper()
	reportJSON = json
	t.Cleanup(func() { reportJSON = false })
	var out bytes.Buffer
	if err := runReport(&out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Started in a directory holding two repositories, which isn't one itself:
// log-hook logs the session, and report attributes its edits in both.
func TestReportFromDirectoryOfRepositories(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := newRepoAt(t, filepath.Join(parent, "a")), newRepoAt(t, filepath.Join(parent, "b"))
	t.Setenv("CLAUDE_PROJECT_DIR", parent)
	t.Setenv("SWARMIA_STATE_DIR", t.TempDir())
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")

	shaA := agentWritesAndCommits(t, a, filepath.Join(a, "a.go"), "var a1 = \"from the agent\"\nvar a2 = \"from the agent\"\n")
	shaB := agentWritesAndCommits(t, b, filepath.Join(b, "b.go"), "var b1 = \"from the agent\"\n")

	out := runReportFor(t, false)
	for _, want := range []string{shaA[:7], shaB[:7], "a.go", "b.go", "3 of 3 AI line(s) committed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// Started in one repository, editing a sibling: the text report shows both
// repositories' commits, while the payload (one repository, until payloads
// carry several) holds only the project's.
func TestReportOfSessionEditingSibling(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, lib := newRepoAt(t, filepath.Join(parent, "app")), newRepoAt(t, filepath.Join(parent, "lib"))
	t.Setenv("CLAUDE_PROJECT_DIR", app)
	t.Setenv("SWARMIA_STATE_DIR", t.TempDir())
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")

	shaApp := agentWritesAndCommits(t, app, filepath.Join(app, "app.go"), "var app = \"from the agent\"\n")
	shaLib := agentWritesAndCommits(t, lib, filepath.Join(lib, "lib.go"), "var lib1 = \"from the agent\"\nvar lib2 = \"from the agent\"\n")

	for _, repo := range []string{app, lib} {
		remote := filepath.Join(t.TempDir(), "remote.git")
		git(t, repo, "init", "-q", "--bare", remote)
		git(t, repo, "remote", "add", "origin", remote)
		git(t, repo, "push", "-q", "origin", "main")
	}

	out := runReportFor(t, false)
	if !strings.Contains(out, shaApp[:7]) || !strings.Contains(out, shaLib[:7]) {
		t.Errorf("report lacks a repository's commit:\n%s", out)
	}

	var payloads []payload.Payload
	if err := json.Unmarshal([]byte(runReportFor(t, true)), &payloads); err != nil || len(payloads) != 1 {
		t.Fatalf("payloads: %v, %v", payloads, err)
	}
	p := payloads[0]
	if len(p.Commits) != 1 || p.Commits[0].SHA != shaApp || p.AILines != 1 || p.AILinesKept != 1 {
		t.Errorf("payload commits = %+v, AI lines = %d/%d; want the project's alone", p.Commits, p.AILinesKept, p.AILines)
	}
}
