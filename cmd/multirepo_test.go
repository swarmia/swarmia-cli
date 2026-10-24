package cmd

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/swarmia/swarmia-cli/internal/deliver"
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

// newRepoAt is initRepo in a given directory, with origin at
// https://github.com/acme/<name>.
func newRepoAt(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, dir, "remote", "add", "origin", "https://github.com/acme/"+filepath.Base(dir)+".git")
	return dir
}

// pushed marks main as pushed: origin's remote-tracking branch holds it.
func pushed(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
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

func reportPayloads(t *testing.T) []payload.Payload {
	t.Helper()
	var payloads []payload.Payload
	if err := json.Unmarshal([]byte(runReportFor(t, true)), &payloads); err != nil {
		t.Fatal(err)
	}
	return payloads
}

// projectIn sets the project directory and gives the test its own state.
func projectIn(t *testing.T, dir string) {
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	t.Setenv("SWARMIA_STATE_DIR", t.TempDir())
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
}

func tempDir(t *testing.T) string {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Started in a directory holding two repositories, which isn't one itself:
// log-hook logs the session, report attributes its edits in both, and its
// payload lists both, each file relative to its own repository.
func TestReportFromDirectoryOfRepositories(t *testing.T) {
	parent := tempDir(t)
	a, b := newRepoAt(t, filepath.Join(parent, "a")), newRepoAt(t, filepath.Join(parent, "b"))
	projectIn(t, parent)

	shaA := agentWritesAndCommits(t, a, filepath.Join(a, "a.go"), "var a1 = \"from the agent\"\nvar a2 = \"from the agent\"\n")
	shaB := agentWritesAndCommits(t, b, filepath.Join(b, "b.go"), "var b1 = \"from the agent\"\n")
	pushed(t, a)
	pushed(t, b)

	out := runReportFor(t, false)
	for _, want := range []string{shaA[:7], shaB[:7], "a.go", "b.go", "3 of 3 AI line(s) committed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}

	payloads := reportPayloads(t)
	if len(payloads) != 1 {
		t.Fatalf("payloads = %+v, want one", payloads)
	}
	p := payloads[0]
	wantRepos := []payload.Repo{
		{Repo: "github.com/acme/a", AILines: 2, AILinesKept: 2},
		{Repo: "github.com/acme/b", AILines: 1, AILinesKept: 1},
	}
	if !reflect.DeepEqual(p.Repos, wantRepos) {
		t.Errorf("repos = %+v, want %+v", p.Repos, wantRepos)
	}
	var tools, commits []string
	for _, tl := range p.Tools {
		tools = append(tools, tl.Repo+" "+tl.File)
	}
	for _, c := range p.Commits {
		commits = append(commits, c.Repo+" "+c.SHA+" "+c.Path)
	}
	if want := []string{"github.com/acme/a a.go", "github.com/acme/b b.go"}; !reflect.DeepEqual(tools, want) {
		t.Errorf("tools = %q, want %q", tools, want)
	}
	if want := []string{"github.com/acme/a " + shaA + " a.go", "github.com/acme/b " + shaB + " b.go"}; !reflect.DeepEqual(commits, want) {
		t.Errorf("commits = %q, want %q", commits, want)
	}
}

// Started in one repository, editing a sibling: both are in the payload, the
// one it was started in first.
func TestReportOfSessionEditingSibling(t *testing.T) {
	parent := tempDir(t)
	app, lib := newRepoAt(t, filepath.Join(parent, "app")), newRepoAt(t, filepath.Join(parent, "lib"))
	projectIn(t, app)

	shaApp := agentWritesAndCommits(t, app, filepath.Join(app, "app.go"), "var app = \"from the agent\"\n")
	shaLib := agentWritesAndCommits(t, lib, filepath.Join(lib, "lib.go"), "var lib1 = \"from the agent\"\nvar lib2 = \"from the agent\"\n")
	pushed(t, lib)

	out := runReportFor(t, false)
	if !strings.Contains(out, shaApp[:7]) || !strings.Contains(out, shaLib[:7]) {
		t.Errorf("report lacks a repository's commit:\n%s", out)
	}
	payloads := reportPayloads(t)
	if len(payloads) != 1 {
		t.Fatalf("payloads = %+v, want one", payloads)
	}
	p := payloads[0]
	wantRepos := []payload.Repo{
		{Repo: "github.com/acme/app", AILines: 1}, // not pushed
		{Repo: "github.com/acme/lib", AILines: 2, AILinesKept: 2},
	}
	if !reflect.DeepEqual(p.Repos, wantRepos) || len(p.Commits) != 1 || p.Commits[0].SHA != shaLib {
		t.Errorf("repos = %+v, commits = %+v; want %+v and lib's commit", p.Repos, p.Commits, wantRepos)
	}
}

// ingest is a fake ingest endpoint that records each request's token and the
// repositories of the sessions in it.
type ingest struct {
	mu  sync.Mutex
	got map[string][]string // token -> repos, per session
	srv *httptest.Server
}

func newIngest(t *testing.T) *ingest {
	in := &ingest{got: map[string][]string{}}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var req deliver.Request
		if err := json.NewDecoder(zr).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		in.mu.Lock()
		defer in.mu.Unlock()
		for _, s := range req.Sessions {
			var repos []string
			for _, repo := range s.Repos {
				repos = append(repos, repo.Repo)
			}
			in.got[token] = append(in.got[token], strings.Join(repos, ","))
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(in.srv.Close)
	return in
}

// configure commits a .swarmia.json with token to repo.
func configure(t *testing.T, repo, endpoint, token string) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"token": token, "endpoint": endpoint})
	if err := os.WriteFile(filepath.Join(repo, ".swarmia.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".swarmia.json")
	git(t, repo, "commit", "-q", "-m", "configure swarmia-cli")
}

// Repositories configured with different tokens get their own parts of a
// session, each with its token; one configured with none sends nothing.
func TestSendSplitsSessionByToken(t *testing.T) {
	in := newIngest(t)
	parent := tempDir(t)
	app, client, oss := newRepoAt(t, filepath.Join(parent, "app")), newRepoAt(t, filepath.Join(parent, "client")), newRepoAt(t, filepath.Join(parent, "oss"))
	configure(t, app, in.srv.URL, "swm_cli_"+strings.Repeat("a", 64))
	configure(t, client, in.srv.URL, "swm_cli_"+strings.Repeat("c", 64))
	projectIn(t, parent)

	agentWritesAndCommits(t, app, filepath.Join(app, "app.go"), "var app = \"from the agent\"\n")
	agentWritesAndCommits(t, client, filepath.Join(client, "c.go"), "var c = \"from the agent\"\n")
	agentWritesAndCommits(t, oss, filepath.Join(oss, "o.go"), "var o = \"from the agent\"\n")

	var out bytes.Buffer
	sendCmd.SetOut(&out)
	t.Cleanup(func() { sendCmd.SetOut(nil) })
	if err := sendCmd.RunE(sendCmd, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"swm_cli_" + strings.Repeat("a", 64): {"github.com/acme/app"},
		"swm_cli_" + strings.Repeat("c", 64): {"github.com/acme/client"},
	}
	if !reflect.DeepEqual(in.got, want) {
		t.Errorf("sent %v, want %v", in.got, want)
	}
	if !strings.Contains(out.String(), "sent 2 session(s)") {
		t.Errorf("output = %q", out.String())
	}

	// report --json shows all three parts, including the one with no token.
	if n := len(reportPayloads(t)); n != 3 {
		t.Errorf("report --json printed %d payloads, want 3", n)
	}
}
