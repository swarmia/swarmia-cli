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

func session() *report.Session {
	return &report.Session{
		ID: "s1", Agent: "claude-code", Model: "m", StartedAt: at, EndedAt: at.Add(time.Hour),
		Prompts: []report.Prompt{{At: at, Text: "use key ghp_" + strings.Repeat("a1B2", 9) + " to fix it"}},
		Tools: []report.Tool{
			{At: at, Name: "Bash", Command: "cat > /src/app/x.go <<'EOF'\nfunc Code() {}\nEOF\ngo test ./..."},
			{At: at, Name: "Edit", File: "/src/app/internal/x.go"},
			{At: at, Name: "Read", File: "/home/me/.claude/plans/plan.md"},
		},
		AILinesByRoot: map[string]map[string]int{"/src/app": {"internal/x.go": 5}},
		Attribution: &report.Attribution{Commits: []report.Commit{
			{SHA: "pushed1", Pushed: true, Files: []report.File{{Path: "internal/x.go", Lines: []report.Range{{Start: 1, End: 3, Class: attribution.Exact}}}}, Kept: []report.Span{{Start: 0, End: 2}}},
			{SHA: "local1", Pushed: false, Files: []report.File{{Path: "internal/x.go", Lines: []report.Range{{Start: 4, End: 5, Class: attribution.Exact}}}}, Kept: []report.Span{{Start: 3, End: 4}}},
		}},
	}
}

func TestBuildAppliesPrivacyRules(t *testing.T) {
	p := Build(session(), Context{RepoRoot: "/src/app", Repo: "github.com/acme/app", Prompts: true})

	if len(p.Prompts) != 1 || !strings.Contains(p.Prompts[0].Text, "[REDACTED:github-token]") || strings.Contains(p.Prompts[0].Text, "ghp_") {
		t.Errorf("prompt not redacted: %+v", p.Prompts)
	}
	if want := "cat > <repo>/x.go <<'EOF'\n[1 lines omitted]\nEOF\ngo test ./..."; p.Tools[0].Command != want {
		t.Errorf("command = %q, want %q", p.Tools[0].Command, want)
	}
	if p.Tools[1].File != "internal/x.go" || p.Tools[2].File != "" {
		t.Errorf("tool files = %q, %q; want repo-relative and dropped", p.Tools[1].File, p.Tools[2].File)
	}
	if want := []Commit{{SHA: "pushed1", Path: "internal/x.go", Lines: []report.Range{{Start: 1, End: 3, Class: attribution.Exact}}}}; !reflect.DeepEqual(p.Commits, want) {
		t.Errorf("commits = %+v, want only the pushed one", p.Commits)
	}
	if p.Ended {
		t.Error("session wasn't ended")
	}
	if p.AILines != 5 || p.AILinesKept != 3 || p.MatcherVersion != attribution.Version {
		t.Errorf("counts: generated=%d committed=%d version=%q", p.AILines, p.AILinesKept, p.MatcherVersion)
	}

	// Nothing in the serialized payload should carry the local home directory.
	data, _ := json.Marshal(p)
	if strings.Contains(string(data), "/home/me") {
		t.Errorf("payload leaks a local path: %s", data)
	}
}

func TestBuildMasksLocalPaths(t *testing.T) {
	s := session()
	s.Tools = []report.Tool{
		{At: at, Name: "Bash", Command: "cd /home/jdoe/src/app && ls /home/jdoe/notes /tmp/claude-501/-home-jdoe-src/x"},
	}
	s.Prompts = []report.Prompt{{At: at, Text: "see /home/jdoe/src/app/README"}}
	p := Build(s, Context{RepoRoot: "/home/jdoe/src/app", Home: "/home/jdoe", UserName: "jdoe", Prompts: true})
	if want := "cd <repo> && ls ~/notes /tmp/claude-501/-home-<user>-src/x"; p.Tools[0].Command != want {
		t.Errorf("command = %q, want %q", p.Tools[0].Command, want)
	}
	if want := "see <repo>/README"; p.Prompts[0].Text != want {
		t.Errorf("prompt = %q, want %q", p.Prompts[0].Text, want)
	}
}

func TestBuildSendsToolStatus(t *testing.T) {
	s := session()
	code := 1
	s.Tools = []report.Tool{
		{At: at, Name: "Bash", Command: "go test ./...", Status: "failed", ExitCode: &code},
		{At: at, Name: "Edit", File: "/src/app/x.go", Status: "ok"},
		{At: at, Name: "apply_patch"}, // an agent that doesn't say
	}
	data, err := json.Marshal(Build(s, Context{RepoRoot: "/src/app"}).Tools)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"at":"2026-10-01T12:00:00Z","name":"Bash","command":"go test ./...","status":"failed","exit_code":1},` +
		`{"at":"2026-10-01T12:00:00Z","name":"Edit","file":"x.go","status":"ok"},` +
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
	if p := Build(s, Context{}); p.Model != "claude-big" {
		t.Errorf("model = %q, want claude-big", p.Model)
	}
}

func TestBuildWithholdsPrompts(t *testing.T) {
	p := Build(session(), Context{RepoRoot: "/src/app", Prompts: false})
	if len(p.Prompts) != 0 || !p.PromptsWithheld {
		t.Errorf("prompts = %+v, withheld = %v", p.Prompts, p.PromptsWithheld)
	}
	if len(p.Tools) != 3 || len(p.Commits) != 1 {
		t.Error("withholding prompts must not drop tools or attribution")
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

// Until payloads carry several repositories, one holds only its repository's
// part of a session: commits, survived lines and AI lines elsewhere are left
// out.
func TestBuildKeepsToItsRepository(t *testing.T) {
	s := session()
	s.AILinesByRoot["/src/lib"] = map[string]int{"lib.go": 7}
	s.Attribution.Commits = append(s.Attribution.Commits, report.Commit{
		Root: "/src/lib", SHA: "libsha", Pushed: true,
		Files: []report.File{{Path: "lib.go", Lines: []report.Range{{Start: 1, End: 7, Class: attribution.Exact}}}},
		Kept:  []report.Span{{Start: 10, End: 16}},
	})
	for i := range s.Attribution.Commits[:2] {
		s.Attribution.Commits[i].Root = "/src/app"
	}

	p := Build(s, Context{RepoRoot: "/src/app", InRepo: func(root string) bool { return root == "/src/app" }})
	if len(p.Commits) != 1 || p.Commits[0].SHA != "pushed1" || p.AILines != 5 || p.AILinesKept != 3 {
		t.Errorf("commits = %+v, generated = %d, committed = %d; want app's alone", p.Commits, p.AILines, p.AILinesKept)
	}
	// Without InRepo everything counts.
	if p := Build(s, Context{RepoRoot: "/src/app"}); len(p.Commits) != 2 || p.AILines != 12 || p.AILinesKept != 10 {
		t.Errorf("commits = %+v, generated = %d, committed = %d; want both repositories", p.Commits, p.AILines, p.AILinesKept)
	}
}
