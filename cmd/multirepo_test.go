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
	"time"

	"github.com/swarmia/swarmia-cli/internal/deliver"
	"github.com/swarmia/swarmia-cli/internal/events"
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

// A session is sent whole, with the configuration where the agent was
// started: the repositories' own tokens don't route parts of it elsewhere.
// Started in a directory with none, it isn't sent.
func TestSendUsesTheProjectsConfiguration(t *testing.T) {
	in := newIngest(t)
	parent := tempDir(t)
	app, client, oss := newRepoAt(t, filepath.Join(parent, "app")), newRepoAt(t, filepath.Join(parent, "client")), newRepoAt(t, filepath.Join(parent, "oss"))
	configure(t, app, in.srv.URL, "swm_cli_"+strings.Repeat("a", 64))
	configure(t, client, in.srv.URL, "swm_cli_"+strings.Repeat("c", 64))
	projectIn(t, parent)

	agentWritesAndCommits(t, app, filepath.Join(app, "app.go"), "var app = \"from the agent\"\n")
	agentWritesAndCommits(t, client, filepath.Join(client, "c.go"), "var c = \"from the agent\"\n")
	agentWritesAndCommits(t, oss, filepath.Join(oss, "o.go"), "var o = \"from the agent\"\n")

	if err := sendCmd.RunE(sendCmd, nil); err == nil || !strings.Contains(err.Error(), "no token configured") {
		t.Errorf("send = %v, want no token configured", err)
	}
	if len(in.got) != 0 {
		t.Errorf("sent %v, want nothing", in.got)
	}

	// report --json shows the one payload, with all three repositories.
	payloads := reportPayloads(t)
	if len(payloads) != 1 || len(payloads[0].Repos) != 3 {
		t.Errorf("report --json printed %+v, want one payload with three repositories", payloads)
	}
}

// A .swarmia.json in the directory holding the repositories configures a
// session started there: all of it goes with that token, including a
// repository that commits a token of its own.
func TestSendWithTokenInParentDirectory(t *testing.T) {
	in := newIngest(t)
	parent := tempDir(t)
	dirToken, clientToken := "swm_cli_"+strings.Repeat("d", 64), "swm_cli_"+strings.Repeat("c", 64)
	data, _ := json.Marshal(map[string]string{"token": dirToken, "endpoint": in.srv.URL})
	if err := os.WriteFile(filepath.Join(parent, ".swarmia.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	a, b, client := newRepoAt(t, filepath.Join(parent, "a")), newRepoAt(t, filepath.Join(parent, "b")), newRepoAt(t, filepath.Join(parent, "client"))
	configure(t, client, in.srv.URL, clientToken)
	projectIn(t, parent)

	agentWritesAndCommits(t, a, filepath.Join(a, "a.go"), "var a = \"from the agent\"\n")
	agentWritesAndCommits(t, b, filepath.Join(b, "b.go"), "var b = \"from the agent\"\n")
	agentWritesAndCommits(t, client, filepath.Join(client, "c.go"), "var c = \"from the agent\"\n")

	if err := sendCmd.RunE(sendCmd, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{dirToken: {"github.com/acme/a,github.com/acme/b,github.com/acme/client"}}
	if !reflect.DeepEqual(in.got, want) {
		t.Errorf("sent %v, want %v", in.got, want)
	}

	// config, run in the directory, shows its file.
	var out bytes.Buffer
	configCmd.SetOut(&out)
	t.Cleanup(func() { configCmd.SetOut(nil) })
	if err := configCmd.RunE(configCmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), filepath.Join(parent, ".swarmia.json")) {
		t.Errorf("config output doesn't name the directory's file:\n%s", out.String())
	}
}

// hook sends log-hook a Claude Code tool event of session s.
func hook(t *testing.T, event map[string]any) {
	t.Helper()
	event["session_id"] = "s"
	data, _ := json.Marshal(event)
	if err := runLogHook(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

// shellCommand runs a Bash tool call through log-hook: PreToolUse, then
// effect (what the command does), then PostToolUse. It returns the
// PostToolUse record.
func shellCommand(t *testing.T, cwd, command string, effect func()) events.Record {
	t.Helper()
	event := func(name string) map[string]any {
		return map[string]any{"hook_event_name": name, "tool_name": "Bash", "tool_use_id": "toolu_" + command,
			"cwd": cwd, "tool_input": map[string]any{"command": command}}
	}
	hook(t, event("PreToolUse"))
	time.Sleep(10 * time.Millisecond) // distinct mtimes on coarse filesystems
	effect()
	hook(t, event("PostToolUse"))
	path, err := eventsPath("")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := events.ReadFile(path)
	if err != nil || len(recs) == 0 {
		t.Fatalf("records: %v, %v", recs, err)
	}
	return recs[len(recs)-1]
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func changed(rec events.Record) []string {
	var out []string
	for _, c := range rec.Changes {
		out = append(out, c.Root+" "+c.Path)
	}
	return out
}

// A shell command that names a sibling repository is snapshotted there too:
// what it writes there is recorded in that repository (as format 2, which
// older binaries leave alone), and attributed to its commits.
func TestShellCommandNamingSibling(t *testing.T) {
	parent := tempDir(t)
	app, lib := newRepoAt(t, filepath.Join(parent, "app")), newRepoAt(t, filepath.Join(parent, "lib"))
	projectIn(t, app)

	code := "package lib\n\nfunc Generated() string { return \"from the generator\" }\n"
	rec := shellCommand(t, app, "cd ../lib && ./gen.sh", func() { writeFile(t, filepath.Join(lib, "gen.go"), code) })
	if want := []string{lib + " gen.go"}; !reflect.DeepEqual(changed(rec), want) || rec.Format != 2 {
		t.Errorf("changes = %q (format %d), want %q in format 2", changed(rec), rec.Format, want)
	}
	if want := []string{app, lib}; !reflect.DeepEqual(rec.Snapshotted, want) {
		t.Errorf("snapshotted = %q, want %q", rec.Snapshotted, want)
	}

	git(t, lib, "add", ".")
	git(t, lib, "commit", "-q", "-m", "generate")
	sha := git(t, lib, "rev-parse", "HEAD")
	if out := runReportFor(t, false); !strings.Contains(out, sha[:7]) || !strings.Contains(out, "gen.go") || !strings.Contains(out, "(1 via shell)") {
		t.Errorf("report lacks the generated file's commit:\n%s", out)
	}
}

// A repository the session worked in before is snapshotted for later
// commands, even ones that don't name it; one it never worked in isn't, and
// what a command writes there goes unrecorded (the gap docs/multirepo.md
// accepts and measures).
func TestShellCommandInWorkingSet(t *testing.T) {
	parent := tempDir(t)
	app, lib, other := newRepoAt(t, filepath.Join(parent, "app")), newRepoAt(t, filepath.Join(parent, "lib")), newRepoAt(t, filepath.Join(parent, "other"))
	projectIn(t, app)

	hook(t, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "cwd": app,
		"tool_input": map[string]any{"file_path": filepath.Join(lib, "README.md")}})
	rec := shellCommand(t, app, "./sync-all.sh", func() {
		writeFile(t, filepath.Join(lib, "synced.go"), "package lib\n\nvar Synced = true\n")
		writeFile(t, filepath.Join(other, "synced.go"), "package other\n\nvar Synced = true\n")
	})
	if want := []string{lib + " synced.go"}; !reflect.DeepEqual(changed(rec), want) {
		t.Errorf("changes = %q, want %q", changed(rec), want)
	}
	if want := []string{app, lib}; !reflect.DeepEqual(rec.Snapshotted, want) {
		t.Errorf("snapshotted = %q, want %q (not other)", rec.Snapshotted, want)
	}
}

// Started in a directory holding repositories, which isn't one: a command
// run there is snapshotted in the repository it names, and its changes carry
// their root, there being no record's own. A command run there before the
// session worked anywhere has nothing to snapshot.
func TestShellCommandOutsideRepository(t *testing.T) {
	parent := tempDir(t)
	a := newRepoAt(t, filepath.Join(parent, "a"))
	projectIn(t, parent)

	if rec := shellCommand(t, parent, "make", func() {}); len(rec.Snapshotted) != 0 || rec.Snapshot != "" {
		t.Errorf("record = %+v, want nothing snapshotted", rec)
	}
	rec := shellCommand(t, parent, "cd a && make", func() { writeFile(t, filepath.Join(a, "built.go"), "package a\n\nvar Built = 1\n") })
	if want := []string{a + " built.go"}; rec.Git != nil || !reflect.DeepEqual(changed(rec), want) || rec.Format != 2 {
		t.Errorf("git = %+v, changes = %q, format %d; want no git, %q, format 2", rec.Git, changed(rec), rec.Format, want)
	}
	// The command it changed is now in the working set: a later command that
	// doesn't name it is snapshotted there too.
	rec = shellCommand(t, parent, "make", func() { writeFile(t, filepath.Join(a, "again.go"), "package a\n\nvar Again = 1\n") })
	if want := []string{a + " again.go"}; !reflect.DeepEqual(rec.Snapshotted, []string{a}) || !reflect.DeepEqual(changed(rec), want) {
		t.Errorf("snapshotted = %q, changes = %q; want a and %q", rec.Snapshotted, changed(rec), want)
	}
}
