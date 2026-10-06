// Package payload builds what delivery sends to Swarmia for one session. It is
// where the privacy rules are applied, in one place:
//
//   - no source code: line attribution only, commands with heredoc bodies and
//     inline scripts stripped, and tool output never included;
//   - secrets redacted from prompts and commands;
//   - prompts withheld entirely when configuration opts out;
//   - only pushed commits, file paths relative to the repository, and local
//     paths and the user name masked in free text.
package payload

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/redact"
	"github.com/swarmia/swarmia-cli/internal/report"
)

// Payload is one session as sent to Swarmia.
type Payload struct {
	SessionID       string    `json:"session_id"`
	Agent           string    `json:"agent"`
	Model           string    `json:"model,omitempty"`
	MatcherVersion  string    `json:"matcher_version"`
	CLIVersion      string    `json:"cli_version"`
	Repo            string    `json:"repo,omitempty"`
	AuthorEmail     string    `json:"author_email,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	Ended           bool      `json:"ended"`
	Prompts         []Prompt  `json:"prompts"`
	PromptsWithheld bool      `json:"prompts_withheld"`
	Tools           []Tool    `json:"tools"`
	Commits         []Commit  `json:"commits"`
	AILines         int       `json:"ai_lines_generated"`
	AILinesKept     int       `json:"ai_lines_committed"`
}

type Prompt struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

type Tool struct {
	At      time.Time `json:"at"`
	Name    string    `json:"name"`
	File    string    `json:"file,omitempty"`
	Command string    `json:"command,omitempty"`
}

type Commit struct {
	SHA   string         `json:"sha"`
	Path  string         `json:"path"`
	Lines []report.Range `json:"lines"`
}

// Context is what a payload needs beyond the session.
type Context struct {
	RepoRoot    string // worktree root, for making tool file paths relative
	Repo        string // e.g. github.com/acme/app (see RepoSlug)
	AuthorEmail string
	CLIVersion  string
	Prompts     bool   // false withholds prompt text (see config)
	Home        string // home directory, masked as ~ in free text
	UserName    string // local user name, masked as <user> in free text
}

// Build makes the payload for s.
func Build(s *report.Session, ctx Context) Payload {
	p := Payload{
		SessionID: s.ID, Agent: s.Agent, Model: s.Model,
		MatcherVersion: attribution.Version, CLIVersion: ctx.CLIVersion,
		Repo: ctx.Repo, AuthorEmail: ctx.AuthorEmail,
		StartedAt: s.StartedAt, EndedAt: s.EndedAt, Ended: s.Ended,
		Prompts: []Prompt{}, PromptsWithheld: !ctx.Prompts,
		Tools: []Tool{}, Commits: []Commit{},
		AILines: s.AILines(),
	}
	if ctx.Prompts {
		for _, pr := range s.Prompts {
			p.Prompts = append(p.Prompts, Prompt{At: pr.At, Text: ctx.maskLocal(redact.Text(pr.Text))})
		}
	}
	for _, t := range s.Tools {
		tool := Tool{At: t.At, Name: t.Name, File: relativePath(ctx.RepoRoot, t.File)}
		if t.Command != "" {
			tool.Command = ctx.maskLocal(redact.Command(t.Command))
		}
		p.Tools = append(p.Tools, tool)
	}
	for _, c := range s.Commits() {
		if !c.Pushed {
			continue // only commits the server can see
		}
		for _, f := range c.Files {
			p.Commits = append(p.Commits, Commit{SHA: c.SHA, Path: f.Path, Lines: f.Lines})
		}
		p.AILinesKept += c.Survived()
	}
	return p
}

// maskLocal hides where things live on this machine: the repository root
// becomes <repo>, the home directory ~, and any other occurrence of the user
// name (e.g. inside tool-generated temp paths) <user>.
func (ctx Context) maskLocal(s string) string {
	if ctx.RepoRoot != "" {
		s = strings.ReplaceAll(s, ctx.RepoRoot, "<repo>")
	}
	if ctx.Home != "" && ctx.Home != "/" {
		s = strings.ReplaceAll(s, ctx.Home, "~")
	}
	if len(ctx.UserName) >= 3 {
		s = strings.ReplaceAll(s, ctx.UserName, "<user>")
	}
	return s
}

// relativePath turns an absolute path inside root into a slash-separated
// relative one; paths outside the repository are dropped, so no local
// directory names leave the machine.
func relativePath(root, path string) string {
	if path == "" || root == "" || !filepath.IsAbs(path) {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// RepoSlug turns a remote URL into host/owner/name, e.g.
// git@github.com:acme/app.git → github.com/acme/app.
func RepoSlug(remote string) string {
	s := strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	if at := strings.Index(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	return strings.Replace(s, ":", "/", 1)
}
