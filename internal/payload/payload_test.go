package payload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/report"
	"github.com/swarmia/swarmia-cli/internal/usage"
)

var at = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

var (
	acme   = Target{Endpoint: "https://swarmia.example/cli", Token: "acme-token"}
	client = Target{Endpoint: "https://swarmia.example/cli", Token: "client-token"}
	app    = RepoInfo{Slug: "github.com/acme/app", Target: acme, Prompts: true, AuthorEmail: "me@acme.com"}
)

// contextFor is a Context whose worktrees are known by root; any other is in
// no identifiable repository.
func contextFor(repos map[string]RepoInfo) Context {
	return Context{CLIVersion: "0.9.0", Repo: func(root string) RepoInfo { return repos[root] }}
}

// only builds s, which must have exactly one part.
func only(t *testing.T, s *report.Session, ctx Context) Payload {
	t.Helper()
	parts := Build(s, ctx)
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	return parts[0].Payload
}

func session() *report.Session {
	return &report.Session{
		ID: "s1", Agent: "claude-code", Model: "m", StartedAt: at, EndedAt: at.Add(time.Hour), Root: "/src/app",
		Prompts: []report.Prompt{{At: at, Text: "use key ghp_" + strings.Repeat("a1B2", 9) + " to fix it"}},
		Tools: []report.Tool{
			{At: at, Name: "Bash", Root: "/src/app", Command: "cat > /src/app/x.go <<'EOF'\nfunc Code() {}\nEOF\ngo test ./..."},
			{At: at, Name: "Edit", Root: "/src/app", File: "/src/app/internal/x.go"},
			{At: at, Name: "Read", File: "/home/me/.claude/plans/plan.md"},
		},
		AILinesByRoot: map[string]map[string]int{"/src/app": {"internal/x.go": 5}},
		Attribution: &report.Attribution{Commits: []report.Commit{
			{Root: "/src/app", SHA: "pushed1", Pushed: true, Files: []report.File{{Path: "internal/x.go", Lines: []report.Range{{Start: 1, End: 3, Class: attribution.Exact}}}}, Kept: []report.Span{{Start: 0, End: 2}}},
			{Root: "/src/app", SHA: "local1", Pushed: false, Files: []report.File{{Path: "internal/x.go", Lines: []report.Range{{Start: 4, End: 5, Class: attribution.Exact}}}}, Kept: []report.Span{{Start: 3, End: 4}}},
		}},
	}
}

func TestBuildAppliesPrivacyRules(t *testing.T) {
	parts := Build(session(), contextFor(map[string]RepoInfo{"/src/app": app}))
	if len(parts) != 1 || parts[0].Target != acme {
		t.Fatalf("parts = %+v, want one for acme", parts)
	}
	p := parts[0].Payload

	if len(p.Prompts) != 1 || !strings.Contains(p.Prompts[0].Text, "[REDACTED:github-token]") || strings.Contains(p.Prompts[0].Text, "ghp_") {
		t.Errorf("prompt not redacted: %+v", p.Prompts)
	}
	if want := "cat > <repo:github.com/acme/app>/x.go <<'EOF'\n[1 lines omitted]\nEOF\ngo test ./..."; p.Tools[0].Command != want {
		t.Errorf("command = %q, want %q", p.Tools[0].Command, want)
	}
	if p.Tools[0].Repo != app.Slug || p.Tools[1].Repo != app.Slug || p.Tools[1].File != "internal/x.go" || p.Tools[2].Repo != "" || p.Tools[2].File != "" {
		t.Errorf("tools = %+v; want the repository's with relative files, the other without either", p.Tools)
	}
	if want := []Commit{{Repo: app.Slug, SHA: "pushed1", Path: "internal/x.go", Lines: []report.Range{{Start: 1, End: 3, Class: attribution.Exact}}}}; !reflect.DeepEqual(p.Commits, want) {
		t.Errorf("commits = %+v, want only the pushed one", p.Commits)
	}
	if want := []Repo{{Repo: app.Slug, AILines: 5, AILinesKept: 3}}; !reflect.DeepEqual(p.Repos, want) {
		t.Errorf("repos = %+v, want %+v", p.Repos, want)
	}
	if p.Ended || p.AuthorEmail != "me@acme.com" || p.MatcherVersion != attribution.Version || p.CLIVersion != "0.9.0" {
		t.Errorf("payload = %+v", p)
	}

	// Nothing in the serialized payload should carry the local home directory.
	data, _ := json.Marshal(p)
	if strings.Contains(string(data), "/home/me") {
		t.Errorf("payload leaks a local path: %s", data)
	}
}

func TestBuildMasksLocalPaths(t *testing.T) {
	s := session()
	s.Root = "/home/jdoe/src/app"
	s.Tools = []report.Tool{
		{At: at, Name: "Bash", Root: s.Root, Command: "cd /home/jdoe/src/app && ls /home/jdoe/notes /tmp/claude-501/-home-jdoe-src/x"},
		// A sibling directory sharing the root's name isn't the root.
		{At: at, Name: "Bash", Root: s.Root, Command: "diff /home/jdoe/src/app/a /home/jdoe/src/app-v2/a"},
	}
	s.Prompts = []report.Prompt{{At: at, Text: "see /home/jdoe/src/app/README. Paths are relative to /home/jdoe/src/app. Not /home/jdoe/src/app.old"}}
	ctx := contextFor(map[string]RepoInfo{s.Root: {Slug: "github.com/jdoe/app", Target: acme, Prompts: true}})
	ctx.Home, ctx.UserName = "/home/jdoe", "jdoe"
	p := only(t, s, ctx)
	// The user name is masked, but not inside the slug, which isn't local.
	for i, want := range []string{
		"cd <repo:github.com/jdoe/app> && ls ~/notes /tmp/claude-501/-home-<user>-src/x",
		"diff <repo:github.com/jdoe/app>/a ~/src/app-v2/a",
	} {
		if p.Tools[i].Command != want {
			t.Errorf("command = %q, want %q", p.Tools[i].Command, want)
		}
	}
	if want := "see <repo:github.com/jdoe/app>/README. Paths are relative to <repo:github.com/jdoe/app>. Not ~/src/app.old"; p.Prompts[0].Text != want {
		t.Errorf("prompt = %q, want %q", p.Prompts[0].Text, want)
	}
}

func TestBuildSendsToolStatus(t *testing.T) {
	s := session()
	code := 1
	s.Tools = []report.Tool{
		{At: at, Name: "Bash", Root: "/src/app", Command: "go test ./...", Status: "failed", ExitCode: &code},
		{At: at, Name: "Edit", Root: "/src/app", File: "/src/app/x.go", Status: "ok"},
		{At: at, Name: "apply_patch"}, // an agent that doesn't say
	}
	data, err := json.Marshal(only(t, s, contextFor(map[string]RepoInfo{"/src/app": app})).Tools)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"at":"2026-10-01T12:00:00Z","name":"Bash","repo":"github.com/acme/app","command":"go test ./...","status":"failed","exit_code":1},` +
		`{"at":"2026-10-01T12:00:00Z","name":"Edit","repo":"github.com/acme/app","file":"x.go","status":"ok"},` +
		`{"at":"2026-10-01T12:00:00Z","name":"apply_patch"}]`
	if string(data) != want {
		t.Errorf("tools =\n%s\nwant\n%s", data, want)
	}
}

// Headless sessions don't report their model at start; the main session's
// usage names it.
func TestBuildFallsBackToTheUsageModel(t *testing.T) {
	s := session()
	s.Model = ""
	s.TranscriptPath = ""
	s.Usage = &usage.Tracker{}
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	line := func(id, model string) string {
		return `{"type":"assistant","requestId":"r` + id + `","message":{"id":"m` + id + `","model":"` + model + `","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	}
	os.WriteFile(tr, []byte(line("1", "claude-small")+line("2", "claude-big")+line("3", "claude-big")), 0o600)
	s.Usage.Update(usage.ClaudeTranscript, tr)
	if p := only(t, s, contextFor(map[string]RepoInfo{"/src/app": app})); p.Model != "claude-big" {
		t.Errorf("model = %q, want claude-big", p.Model)
	}
}

func TestBuildWithholdsPrompts(t *testing.T) {
	withheld := app
	withheld.Prompts = false
	p := only(t, session(), contextFor(map[string]RepoInfo{"/src/app": withheld}))
	if len(p.Prompts) != 0 || !p.PromptsWithheld {
		t.Errorf("prompts = %+v, withheld = %v", p.Prompts, p.PromptsWithheld)
	}
	if len(p.Tools) != 3 || len(p.Commits) != 1 {
		t.Error("withholding prompts must not drop tools or attribution")
	}
}

// A session that worked in repositories configured with different tokens is
// split: each target gets its own repositories, and of the others nothing
// but a masked <repo> in free text. Prompts and usage go to each.
func TestBuildSplitsByTarget(t *testing.T) {
	s := &report.Session{
		ID: "s1", Agent: "claude-code", StartedAt: at, EndedAt: at, Root: "/src/app",
		Prompts: []report.Prompt{{At: at, Text: "port the fix from /src/client-api to /src/lib"}},
		Tools: []report.Tool{
			{At: at, Name: "Edit", Root: "/src/client-api", File: "/src/client-api/fix.go"},
			{At: at, Name: "Bash", Root: "/src/app", Command: "cp /src/client-api/fix.go /src/lib/fix.go"},
			{At: at, Name: "Edit", Root: "/src/lib", File: "/src/lib/fix.go"},
			{At: at, Name: "Edit", Root: "/src/scratch", File: "/src/scratch/notes.go"},
			{At: at, Name: "Edit", Root: "/src/oss", File: "/src/oss/x.go"},
		},
		AILinesByRoot: map[string]map[string]int{
			"/src/client-api": {"fix.go": 2}, "/src/lib": {"fix.go": 4}, "/src/oss": {"x.go": 1},
		},
		Attribution: &report.Attribution{Commits: []report.Commit{
			{Root: "/src/client-api", SHA: "c1", Pushed: true, Files: []report.File{{Path: "fix.go", Lines: []report.Range{{Start: 1, End: 2, Class: attribution.Exact}}}}, Kept: []report.Span{{Start: 0, End: 1}}},
			{Root: "/src/lib", SHA: "l1", Pushed: true, Files: []report.File{{Path: "fix.go", Lines: []report.Range{{Start: 1, End: 4, Class: attribution.Exact}}}}, Kept: []report.Span{{Start: 2, End: 5}}},
		}},
		Usage: &usage.Tracker{},
	}
	noToken := Target{Endpoint: acme.Endpoint}
	ctx := contextFor(map[string]RepoInfo{
		"/src/app":        app,
		"/src/lib":        {Slug: "github.com/acme/lib", Target: acme, Prompts: true, AuthorEmail: "me@acme.com"},
		"/src/client-api": {Slug: "github.com/client/api", Target: client, Prompts: false, AuthorEmail: "me@client.com"},
		"/src/oss":        {Slug: "github.com/oss/x", Target: noToken, Prompts: true},
		// /src/scratch has no origin: not identifiable, never listed.
		"/src/scratch": {Target: acme, Prompts: true},
	})
	parts := Build(s, ctx)
	var targets []Target
	for _, p := range parts {
		targets = append(targets, p.Target)
	}
	if want := []Target{acme, client, noToken}; !reflect.DeepEqual(targets, want) {
		t.Fatalf("targets = %+v, want %+v", targets, want)
	}

	a, c := parts[0].Payload, parts[1].Payload
	if want := []Repo{{Repo: "github.com/acme/app"}, {Repo: "github.com/acme/lib", AILines: 4, AILinesKept: 4}}; !reflect.DeepEqual(a.Repos, want) {
		t.Errorf("acme repos = %+v, want %+v", a.Repos, want)
	}
	if want := []Repo{{Repo: "github.com/client/api", AILines: 2, AILinesKept: 2}}; !reflect.DeepEqual(c.Repos, want) {
		t.Errorf("client repos = %+v, want %+v", c.Repos, want)
	}
	if len(a.Commits) != 1 || a.Commits[0].SHA != "l1" || len(c.Commits) != 1 || c.Commits[0].SHA != "c1" {
		t.Errorf("commits: acme %+v, client %+v", a.Commits, c.Commits)
	}
	if a.AuthorEmail != "me@acme.com" || c.AuthorEmail != "me@client.com" {
		t.Errorf("author emails %q, %q", a.AuthorEmail, c.AuthorEmail)
	}
	// The client's configuration withholds prompts: only from its part.
	if a.PromptsWithheld || len(a.Prompts) != 1 || !c.PromptsWithheld || len(c.Prompts) != 0 {
		t.Errorf("prompts: acme %+v (withheld %v), client %+v (withheld %v)", a.Prompts, a.PromptsWithheld, c.Prompts, c.PromptsWithheld)
	}
	if want := "port the fix from <repo> to <repo:github.com/acme/lib>"; a.Prompts[0].Text != want {
		t.Errorf("acme prompt = %q, want %q", a.Prompts[0].Text, want)
	}
	tools := func(p Payload) []string {
		var out []string
		for _, tl := range p.Tools {
			out = append(out, tl.Repo+" "+tl.File+" "+tl.Command)
		}
		return out
	}
	if want := []string{
		"  ",
		"github.com/acme/app  cp <repo>/fix.go <repo:github.com/acme/lib>/fix.go",
		"github.com/acme/lib fix.go ",
		"  ",
		"  ",
	}; !reflect.DeepEqual(tools(a), want) {
		t.Errorf("acme tools = %q, want %q", tools(a), want)
	}
	if want := []string{
		"github.com/client/api fix.go ",
		"  cp <repo:github.com/client/api>/fix.go <repo>/fix.go",
		"  ",
		"  ",
		"  ",
	}; !reflect.DeepEqual(tools(c), want) {
		t.Errorf("client tools = %q, want %q", tools(c), want)
	}
	data, _ := json.Marshal(c)
	for _, leak := range []string{"acme", "scratch", "oss"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("client's payload mentions %q: %s", leak, data)
		}
	}
}

// Worktrees of one repository are one entry in repos, each file relative to
// its own worktree.
func TestBuildMergesWorktreesOfARepository(t *testing.T) {
	s := &report.Session{ID: "s1", Agent: "claude-code", StartedAt: at, EndedAt: at, Root: "/src/app",
		Tools: []report.Tool{
			{At: at, Name: "Edit", Root: "/src/app", File: "/src/app/a.go"},
			{At: at, Name: "Edit", Root: "/src/app-wt", File: "/src/app-wt/pkg/b.go"},
		},
		AILinesByRoot: map[string]map[string]int{"/src/app": {"a.go": 1}, "/src/app-wt": {"pkg/b.go": 2}},
	}
	p := only(t, s, contextFor(map[string]RepoInfo{"/src/app": app, "/src/app-wt": app}))
	if want := []Repo{{Repo: app.Slug, AILines: 3}}; !reflect.DeepEqual(p.Repos, want) {
		t.Errorf("repos = %+v, want %+v", p.Repos, want)
	}
	if p.Tools[0].File != "a.go" || p.Tools[1].File != "pkg/b.go" {
		t.Errorf("tools = %+v", p.Tools)
	}
}

// A repository inside another (vendored, say) is masked as itself, even when
// the session worked in the outer one first.
func TestBuildMasksNestedRepository(t *testing.T) {
	s := session()
	s.Tools = []report.Tool{
		{At: at, Name: "Bash", Root: "/src/app", Command: "make"},
		{At: at, Name: "Bash", Root: "/src/app/vendor/lib", Command: "cd /src/app/vendor/lib && cat /src/app/go.mod"},
	}
	lib := app
	lib.Slug = "github.com/acme/lib"
	p := only(t, s, contextFor(map[string]RepoInfo{"/src/app": app, "/src/app/vendor/lib": lib}))
	if want := "cd <repo:github.com/acme/lib> && cat <repo:github.com/acme/app>/go.mod"; p.Tools[1].Command != want {
		t.Errorf("command = %q, want %q", p.Tools[1].Command, want)
	}
}

// Worktrees of one repository can be configured with different tokens (each
// checkout has its own .swarmia.json): each target gets the repository with
// only the work done in its own worktrees.
func TestBuildSplitsWorktreesConfiguredApart(t *testing.T) {
	s := &report.Session{ID: "s1", Agent: "claude-code", StartedAt: at, EndedAt: at, Root: "/src/app",
		Tools: []report.Tool{
			{At: at, Name: "Edit", Root: "/src/app", File: "/src/app/a.go"},
			{At: at, Name: "Edit", Root: "/src/app-wt", File: "/src/app-wt/b.go"},
		},
		AILinesByRoot: map[string]map[string]int{"/src/app": {"a.go": 1}, "/src/app-wt": {"b.go": 2}},
	}
	wt := app
	wt.Target = client
	parts := Build(s, contextFor(map[string]RepoInfo{"/src/app": app, "/src/app-wt": wt}))
	if len(parts) != 2 {
		t.Fatalf("parts = %+v, want one per target", parts)
	}
	a, c := parts[0].Payload, parts[1].Payload
	if a.Repos[0].AILines != 1 || c.Repos[0].AILines != 2 || a.Tools[1].File != "" || c.Tools[0].File != "" {
		t.Errorf("acme %+v %+v; client %+v %+v", a.Repos, a.Tools, c.Repos, c.Tools)
	}
}

// A session started in a directory holding repositories lists them in the
// order it worked in them, and masks that directory as <project>; one that
// worked in no identifiable repository has no parts at all.
func TestBuildOutsideRepository(t *testing.T) {
	s := &report.Session{ID: "s1", Agent: "claude-code", StartedAt: at, EndedAt: at,
		Tools: []report.Tool{
			{At: at, Name: "Bash", Command: "ls /work /work/b"},
			{At: at, Name: "Edit", Root: "/work/b", File: "/work/b/x.go"},
			{At: at, Name: "Edit", Root: "/work/a", File: "/work/a/y.go"},
		},
	}
	ctx := contextFor(map[string]RepoInfo{
		"/work/a": {Slug: "github.com/acme/a", Target: acme, Prompts: true},
		"/work/b": {Slug: "github.com/acme/b", Target: acme, Prompts: true},
	})
	ctx.ProjectDir = "/work"
	p := only(t, s, ctx)
	if len(p.Repos) != 2 || p.Repos[0].Repo != "github.com/acme/b" || p.Repos[1].Repo != "github.com/acme/a" {
		t.Errorf("repos = %+v, want b then a", p.Repos)
	}
	if want := "ls <project> <repo:github.com/acme/b>"; p.Tools[0].Command != want || p.Tools[0].Repo != "" {
		t.Errorf("tool = %+v, want command %q and no repo", p.Tools[0], want)
	}

	s.Tools = s.Tools[:1]
	if parts := Build(s, ctx); len(parts) != 0 {
		t.Errorf("parts = %+v, want none", parts)
	}
}

func TestRepoSlug(t *testing.T) {
	for in, want := range map[string]string{
		// GitHub
		"git@github.com:acme/app.git":                "github.com/acme/app",
		"git@github.com:acme/app":                    "github.com/acme/app",
		"https://github.com/acme/app.git":            "github.com/acme/app",
		"https://github.com/acme/app/":               "github.com/acme/app",
		"https://github.com/acme/app.git/":           "github.com/acme/app",
		"https://x-token:secret@github.com/acme/app": "github.com/acme/app",
		"https://x:p@ss@github.com/acme/app":         "github.com/acme/app",
		"https://GitHub.com/Acme/App":                "github.com/Acme/App",
		"ssh://git@github.com/acme/app.git":          "github.com/acme/app",
		"ssh://git@ssh.github.com:443/acme/app.git":  "github.com/acme/app",
		"git+ssh://git@github.com/acme/app.git":      "github.com/acme/app",
		"git://github.com/acme/app.git":              "github.com/acme/app",
		"https://github.com/acme/app.git?foo=1#bar":  "github.com/acme/app",
		"https://github.com//acme/app":               "github.com/acme/app",
		"https://github.com/":                        "",
		"https://github.com":                         "",
		// GitHub Enterprise Server
		"git@ghe.example.com:acme/app.git":          "ghe.example.com/acme/app",
		"https://ghe.example.com:8443/acme/app.git": "ghe.example.com/acme/app",
		// GitLab, with subgroups, self-hosted and under a path prefix
		"git@gitlab.com:group/sub/repo.git":                      "gitlab.com/group/sub/repo",
		"https://oauth2:glpat-xyz@gitlab.com/group/sub/repo.git": "gitlab.com/group/sub/repo",
		"ssh://git@altssh.gitlab.com:443/group/repo.git":         "gitlab.com/group/repo",
		"ssh://git@gitlab.example.com/group/sub/repo":            "gitlab.example.com/group/sub/repo",
		"ssh://git@gitlab.example.com:2222/group/sub/repo.git":   "gitlab.example.com/group/sub/repo",
		"https://example.com/gitlab/group/repo.git":              "example.com/gitlab/group/repo",
		"git@[gitlab.example.com]:group/repo.git":                "gitlab.example.com/group/repo",
		// Bitbucket Cloud and Data Center (whose HTTPS URLs add scm/, kept as is)
		"git@bitbucket.org:workspace/repo.git":               "bitbucket.org/workspace/repo",
		"https://user@bitbucket.org/workspace/repo.git":      "bitbucket.org/workspace/repo",
		"ssh://git@bitbucket.example.com:7999/proj/repo.git": "bitbucket.example.com/proj/repo",
		"https://bitbucket.example.com/scm/proj/repo.git":    "bitbucket.example.com/scm/proj/repo",
		// Azure DevOps Services, in all its URL forms
		"https://dev.azure.com/acme/Web/_git/app":                      "dev.azure.com/acme/Web/app",
		"https://acme@dev.azure.com/acme/Web/_git/app":                 "dev.azure.com/acme/Web/app",
		"https://dev.azure.com/acme/Web/_git/app/":                     "dev.azure.com/acme/Web/app",
		"https://dev.azure.com/acme/_git/app":                          "dev.azure.com/acme/app/app",
		"https://dev.azure.com/acme/Web/_git/_optimized/app":           "dev.azure.com/acme/Web/app",
		"https://dev.azure.com/acme/My%20Project/_git/app":             "dev.azure.com/acme/My Project/app",
		"git@ssh.dev.azure.com:v3/acme/Web/app":                        "dev.azure.com/acme/Web/app",
		"git@ssh.dev.azure.com:v3/acme/My%20Project/app":               "dev.azure.com/acme/My Project/app",
		"ssh://git@ssh.dev.azure.com/v3/acme/Web/app":                  "dev.azure.com/acme/Web/app",
		"https://acme.visualstudio.com/Web/_git/app":                   "dev.azure.com/acme/Web/app",
		"https://acme.visualstudio.com/DefaultCollection/Web/_git/app": "dev.azure.com/acme/Web/app",
		"https://Acme.VisualStudio.com/_git/app":                       "dev.azure.com/acme/app/app",
		"acme@vs-ssh.visualstudio.com:v3/acme/Web/app":                 "dev.azure.com/acme/Web/app",
		"https://dev.azure.com/acme/Web/_git":                          "dev.azure.com/acme/Web/_git",
		"https://dev.azure.com/acme/Web/app":                           "dev.azure.com/acme/Web/app",
		// Azure DevOps Server keeps the same path over SSH and HTTPS
		"https://tfs.example.com/tfs/Coll/Web/_git/app":  "tfs.example.com/tfs/Coll/Web/_git/app",
		"ssh://tfs.example.com:22/tfs/Coll/Web/_git/app": "tfs.example.com/tfs/Coll/Web/_git/app",
		// local remotes
		"/tmp/e2e/origin.git":     "",
		"../origin":               "",
		"./origin":                "",
		"origin/repo":             "",
		"~/src/origin.git":        "",
		"file:///srv/git/app.git": "",
		"file://srv/git/app.git":  "",
		"":                        "",
	} {
		if got := RepoSlug(in, nil); got != want {
			t.Errorf("RepoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoSlugSSHAlias(t *testing.T) {
	aliases := map[string]string{"github-work": "github.com", "gh": "GitHub.com"}
	resolve := func(alias string) string { return aliases[alias] }
	for in, want := range map[string]string{
		"git@github-work:acme/app.git":      "github.com/acme/app",
		"ssh://git@gh/acme/app.git":         "github.com/acme/app",
		"git@gitlab.example.com:group/repo": "gitlab.example.com/group/repo", // not an alias
		"https://github-work/acme/app.git":  "github-work/acme/app",          // not SSH
	} {
		if got := RepoSlug(in, resolve); got != want {
			t.Errorf("RepoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
