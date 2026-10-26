// Package payload builds what delivery sends to Swarmia for one session. It is
// where the privacy rules are applied, in one place:
//
//   - no source code: line attribution only, commands with heredoc bodies and
//     inline scripts stripped, and tool output never included;
//   - secrets redacted from prompts and commands;
//   - prompts withheld entirely when configuration opts out;
//   - only pushed commits, file paths relative to their repository, and local
//     paths and the user name masked in free text.
package payload

import (
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
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
	AuthorEmail     string    `json:"author_email,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	Ended           bool      `json:"ended"`
	Prompts         []Prompt  `json:"prompts"`
	PromptsWithheld bool      `json:"prompts_withheld"`
	Repos           []Repo    `json:"repos"`
	Tools           []Tool    `json:"tools"`
	Commits         []Commit  `json:"commits"`
	// Usage is the session's token usage per model, speed and main session vs
	// subagents, read from the agent's transcript. Cost is left to the server.
	Usage []usage.Totals `json:"usage"`
}

// Repo is a repository the session worked in, with the session's AI line
// counts there.
type Repo struct {
	Repo        string `json:"repo"`
	AILines     int    `json:"ai_lines_generated"`
	AILinesKept int    `json:"ai_lines_committed"`
}

type Prompt struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

type Tool struct {
	At       time.Time `json:"at"`
	Name     string    `json:"name"`
	Repo     string    `json:"repo,omitempty"`
	File     string    `json:"file,omitempty"`
	Command  string    `json:"command,omitempty"`
	Status   string    `json:"status,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
}

type Commit struct {
	Repo  string         `json:"repo"`
	SHA   string         `json:"sha"`
	Path  string         `json:"path"`
	Lines []report.Range `json:"lines"`
}

// RepoInfo is what a payload needs to know about a worktree.
type RepoInfo struct {
	Slug        string // see RepoSlug; "" if it can't be identified, and then never sent
	Prompts     bool   // its configuration lets prompts be shared
	AuthorEmail string // the developer's commit email there
}

// Context is what payloads need beyond the session.
type Context struct {
	CLIVersion string
	// Prompts is whether the configuration the session is sent with lets
	// prompts be shared.
	Prompts bool
	// Repo describes the worktree at root; Build calls it once per root.
	Repo func(root string) RepoInfo
	// ProjectDir is where the agent was started, masked as <project> in free
	// text where no worktree root covers it.
	ProjectDir string
	Home       string // home directory, masked as ~ in free text
	UserName   string // local user name, masked as <user> in free text
}

// Build makes the payload of s, listing the repositories the session worked
// in, in the order it first worked in them (the one it was started in first).
// A worktree that can't be identified is left out; a session that worked in
// no identifiable repository isn't sent, and Build reports false.
//
// The whole session goes where the project's configuration says, whatever
// the repositories it worked in are configured with: a developer reports to
// one organization. Prompts are withheld if the project's configuration, or
// that of any repository the session worked in, opts out.
func Build(s *report.Session, ctx Context) (Payload, bool) {
	info := map[string]RepoInfo{}
	var roots []string // in the order the session worked in them
	addRoot := func(root string) {
		if _, ok := info[root]; ok || root == "" {
			return
		}
		info[root] = ctx.Repo(root)
		roots = append(roots, root)
	}
	addRoot(s.Root)
	for _, t := range s.Tools {
		addRoot(t.Root)
	}
	for _, e := range s.Edits {
		addRoot(e.Root)
	}
	for _, c := range s.Commits() {
		addRoot(c.Root)
	}

	var slugs []string
	prompts, author := ctx.Prompts, ""
	for _, root := range roots {
		ri := info[root]
		prompts = prompts && ri.Prompts
		if ri.Slug == "" || slices.Contains(slugs, ri.Slug) {
			continue
		}
		slugs = append(slugs, ri.Slug)
		if author == "" {
			author = ri.AuthorEmail
		}
	}
	if len(slugs) == 0 {
		return Payload{}, false
	}
	slugOf := func(root string) string { return info[root].Slug }
	masks := ctx.masks(roots, slugOf)

	p := Payload{
		SessionID: s.ID, Agent: s.Agent, Model: s.Model,
		MatcherVersion: attribution.Version, CLIVersion: ctx.CLIVersion,
		AuthorEmail: author,
		StartedAt:   s.StartedAt, EndedAt: s.EndedAt, Ended: s.Ended,
		Prompts: []Prompt{}, PromptsWithheld: !prompts,
		Repos: []Repo{}, Tools: []Tool{}, Commits: []Commit{},
		Usage: []usage.Totals{},
	}
	if s.Usage != nil {
		p.Usage = s.Usage.Totals()
	}
	if p.Model == "" {
		p.Model = mainModel(p.Usage) // headless (-p) sessions don't report it at start
	}
	if prompts {
		for _, pr := range s.Prompts {
			p.Prompts = append(p.Prompts, Prompt{At: pr.At, Text: masks.apply(redact.Text(pr.Text))})
		}
	}
	for _, slug := range slugs {
		r := Repo{Repo: slug}
		for root := range s.AILinesByRoot {
			if slugOf(root) == slug {
				r.AILines += s.AILinesIn(root)
			}
		}
		r.AILinesKept = s.Survived(func(c report.Commit) bool { return c.Pushed && slugOf(c.Root) == slug })
		p.Repos = append(p.Repos, r)
	}
	for _, tl := range s.Tools {
		tool := Tool{At: tl.At, Name: tl.Name, Status: tl.Status, ExitCode: tl.ExitCode}
		if tool.Repo = slugOf(tl.Root); tool.Repo != "" {
			tool.File = relativePath(tl.Root, tl.File)
		}
		if tl.Command != "" {
			tool.Command = masks.apply(redact.Command(tl.Command))
		}
		p.Tools = append(p.Tools, tool)
	}
	for _, c := range s.Commits() {
		slug := slugOf(c.Root)
		if !c.Pushed || slug == "" {
			continue // only commits the server can see
		}
		for _, f := range c.Files {
			p.Commits = append(p.Commits, Commit{Repo: slug, SHA: c.SHA, Path: f.Path, Lines: f.Lines})
		}
	}
	return p, true
}

// masks is the replacements that hide where things live on this machine, in
// order: longer (inner) paths first. A directory is only replaced where it
// ends, so /src/app doesn't turn /src/app-v2 into <repo…>-v2.
type masks []mask

type mask struct {
	from, to string
	dir      bool
}

// masks hides each worktree root of the session — as <repo:slug> if it can
// be identified, else <repo> — then the project directory as <project>, the
// home directory as ~ and the user name (e.g. inside tool-generated temp
// paths) as <user>.
func (ctx Context) masks(roots []string, slugOf func(root string) string) masks {
	var m masks
	for _, root := range roots {
		if slug := slugOf(root); slug != "" {
			m = append(m, mask{root, "<repo:" + slug + ">", true})
		} else {
			m = append(m, mask{root, "<repo>", true})
		}
	}
	if ctx.ProjectDir != "" && ctx.ProjectDir != "/" {
		m = append(m, mask{ctx.ProjectDir, "<project>", true})
	}
	if ctx.Home != "" && ctx.Home != "/" {
		m = append(m, mask{ctx.Home, "~", true})
	}
	sort.SliceStable(m, func(a, b int) bool { return len(m[a].from) > len(m[b].from) })
	if len(ctx.UserName) >= 3 {
		m = append(m, mask{ctx.UserName, "<user>", false})
	}
	return m
}

// apply masks s. Each replacement first goes in as a placeholder, so a later
// one (the user name) never rewrites an earlier one's text (a slug).
func (m masks) apply(s string) string {
	placeholder := func(i int) string { return fmt.Sprintf("\x00%d\x00", i) }
	for i, r := range m {
		if r.dir {
			s = replaceDir(s, r.from, placeholder(i))
		} else {
			s = strings.ReplaceAll(s, r.from, placeholder(i))
		}
	}
	for i, r := range m {
		s = strings.ReplaceAll(s, placeholder(i), r.to)
	}
	return s
}

// replaceDir replaces dir in s where it is a whole directory: not followed by
// more of a file name. A dot continues the name only if more of it follows
// (app.old), not at the end of a sentence (app.).
func replaceDir(s, dir, to string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, dir)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(dir)
		b.WriteString(s[:i])
		if continuesName(s[end:]) {
			b.WriteString(dir)
		} else {
			b.WriteString(to)
		}
		s = s[end:]
	}
}

func continuesName(rest string) bool {
	switch {
	case rest == "" || !isNameByte(rest[0]):
		return false
	case rest[0] == '.':
		return len(rest) > 1 && isNameByte(rest[1]) && rest[1] != '.'
	}
	return true
}

func isNameByte(c byte) bool {
	return c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
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
	if org, project, repo, ok := azureDevOps(host, parts); ok {
		host, parts = "dev.azure.com", []string{org, project, repo}
	}
	if host == "" || len(parts) == 0 {
		return ""
	}
	return host + "/" + strings.Join(parts, "/")
}

// azureDevOps reads an Azure DevOps Services remote, whose HTTPS and SSH URLs
// name a repository differently, so that both give dev.azure.com/org/project/repo:
//
//	https://dev.azure.com/org/project/_git/repo
//	https://org.visualstudio.com/[DefaultCollection/]project/_git/repo
//	git@ssh.dev.azure.com:v3/org/project/repo
//	org@vs-ssh.visualstudio.com:v3/org/project/repo
//
// An HTTPS URL may leave out the project when the repository has its name, and
// put _full/ or _optimized/ after _git/. (Azure DevOps Server keeps the same
// path over both, so it needs nothing.)
func azureDevOps(host string, parts []string) (org, project, repo string, ok bool) {
	switch {
	case host == "ssh.dev.azure.com" || host == "vs-ssh.visualstudio.com":
		if len(parts) == 4 && parts[0] == "v3" {
			return parts[1], parts[2], parts[3], true
		}
		return "", "", "", false
	case host == "dev.azure.com":
		if len(parts) == 0 {
			return "", "", "", false
		}
		org, parts = parts[0], parts[1:]
	case strings.HasSuffix(host, ".visualstudio.com"):
		org = strings.TrimSuffix(host, ".visualstudio.com")
		if len(parts) > 0 && strings.EqualFold(parts[0], "DefaultCollection") {
			parts = parts[1:]
		}
	default:
		return "", "", "", false
	}
	// [project/]_git/[_full/|_optimized/]repo
	git := slices.Index(parts, "_git")
	if git < 0 || git > 1 || org == "" || strings.Contains(org, ".") {
		return "", "", "", false
	}
	rest := parts[git+1:]
	if len(rest) == 2 && (rest[0] == "_full" || rest[0] == "_optimized") {
		rest = rest[1:]
	}
	if len(rest) != 1 {
		return "", "", "", false
	}
	repo = rest[0]
	project = repo
	if git == 1 {
		project = parts[0]
	}
	return org, project, repo, true
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
	if p, err := url.PathUnescape(path); err == nil {
		path = p // as url.Parse gives it for URLs: My%20Project → My Project
	}
	return host, path, true
}
