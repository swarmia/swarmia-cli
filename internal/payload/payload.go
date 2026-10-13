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
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/redact"
	"github.com/swarmia/swarmia-cli/internal/report"
	"github.com/swarmia/swarmia-cli/internal/usage"
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
	// Usage is the session's token usage per model, speed and main session vs
	// subagents, read from the agent's transcript. Cost is left to the server.
	Usage []usage.Totals `json:"usage"`
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
		Usage:   []usage.Totals{},
	}
	if s.Usage != nil {
		p.Usage = s.Usage.Totals()
	}
	if p.Model == "" {
		p.Model = mainModel(p.Usage) // headless (-p) sessions don't report it at start
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
	}
	p.AILinesKept = s.Survived(func(c report.Commit) bool { return c.Pushed })
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

// mainModel is the model that made the most calls in the main session (not
// its subagents), or "" without usage.
func mainModel(totals []usage.Totals) string {
	best, most := "", int64(0)
	for _, t := range totals {
		if !t.Subagent && t.Requests > most {
			best, most = t.Model, t.Requests
		}
	}
	return best
}

// RepoSlug turns a remote URL into host/path, e.g. git@github.com:acme/app.git
// → github.com/acme/app, so every clone of a repository gives the same slug
// whichever way it was cloned. It keeps the host, lowercased, and the path, with
// credentials, port, query and a trailing .git or / dropped. The path isn't
// otherwise interpreted: GitLab subgroups and self-hosted instances under a path
// prefix keep all of it, and the server matches it to the repositories it knows.
//
// sshHostName, if not nil, resolves the host of an SSH remote, which may be an
// alias from ~/.ssh/config (git@github-work:acme/app), to the real host name.
// Hosts that the cloud services accept SSH on besides their own (ssh.github.com)
// are mapped back to it.
//
// A remote on the local filesystem (a path or file:// URL) gives "": it names
// no repository Swarmia could know, and would reveal a local path.
func RepoSlug(remote string, sshHostName func(alias string) string) string {
	host, path, ssh := parseRemote(strings.TrimSpace(remote))
	if host == "" {
		return ""
	}
	if ssh && sshHostName != nil {
		if h := sshHostName(host); h != "" {
			host = h
		}
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if canonical, ok := sshHosts[host]; ok {
		host = canonical
	}
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if n := len(parts); n > 0 {
		parts[n-1] = strings.TrimSuffix(parts[n-1], ".git")
		if parts[n-1] == "" {
			parts = parts[:n-1]
		}
	}
	if host == "" || len(parts) == 0 {
		return ""
	}
	return host + "/" + strings.Join(parts, "/")
}

// sshHosts maps the hosts that cloud Git services accept SSH on, e.g. through
// port 443, to the service's own host.
var sshHosts = map[string]string{
	"ssh.github.com":       "github.com",
	"altssh.gitlab.com":    "gitlab.com",
	"altssh.bitbucket.org": "bitbucket.org",
}

// parseRemote splits a remote URL into its host and path, and reports whether
// it's reached over SSH. It reads the forms git accepts: scheme://[user@]host
// [:port]/path and the scp-like [user@]host:path. A local path gives no host.
func parseRemote(remote string) (host, path string, ssh bool) {
	if remote == "" || filepath.IsAbs(remote) {
		return "", "", false
	}
	if scheme, rest, ok := strings.Cut(remote, "://"); ok {
		u, err := url.Parse(remote)
		if err != nil {
			// e.g. a userinfo with characters url.Parse refuses; drop it by hand
			authority, p, _ := strings.Cut(rest, "/")
			if at := strings.LastIndex(authority, "@"); at >= 0 {
				authority = authority[at+1:]
			}
			if u, err = url.Parse(scheme + "://" + authority + "/" + p); err != nil {
				return "", "", false
			}
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh", "git+ssh", "ssh+git":
			return u.Hostname(), u.Path, true
		case "https", "http", "git":
			return u.Hostname(), u.Path, false
		}
		return "", "", false // file:// or a transport we don't know
	}
	// scp-like: a colon before any slash. Without one it's a local path.
	colon := strings.Index(remote, ":")
	if colon < 0 || strings.Contains(remote[:colon], "/") {
		return "", "", false
	}
	host, path = remote[:colon], remote[colon+1:]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]") // [host]:path
	return host, path, true
}
