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
		AILinesByPath: map[string]int{"internal/x.go": 5},
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
		"git@github.com:acme/app.git":                 "github.com/acme/app",
		"https://github.com/acme/app.git":             "github.com/acme/app",
		"https://x-token:secret@github.com/acme/app":  "github.com/acme/app",
		"ssh://git@gitlab.example.com/group/sub/repo": "gitlab.example.com/group/sub/repo",
		"/tmp/e2e/origin.git":                         "",
		"../origin":                                   "",
		"file:///srv/git/app.git":                     "",
		"":                                            "",
	} {
		if got := RepoSlug(in); got != want {
			t.Errorf("RepoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
