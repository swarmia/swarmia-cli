package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/events"
	"github.com/swarmia/swarmia-cli/internal/gitrepo"
	"github.com/swarmia/swarmia-cli/internal/report"
)

var (
	reportJSON    bool
	reportLimit   int
	reportSession string
	reportAllAuth bool
	reportFile    string
)

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Show which committed lines came from recent agent sessions",
	Long: "report reads the local event log, matches each session's edits to the commits\n" +
		"they landed in, and prints the result. Only lines a commit itself introduced\n" +
		"are attributed to it, each classed as exact, normalized (only reformatted) or\n" +
		"modified (edited by a human).\n\n" +
		"Commits are considered if they are on any branch, were made after the session's\n" +
		"edits, and are authored by your git user.email (--all-authors to include\n" +
		"everyone). 'pushed' marks commits a remote-tracking branch contains; only those\n" +
		"would ever be sent.\n\n" +
		"--json prints the per-session payload that delivery will send (draft format;\n" +
		"secret redaction is not applied yet). Nothing is sent by this command.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReport(cmd.OutOrStdout())
	},
}

func init() {
	reportCmd.Flags().BoolVar(&reportJSON, "json", false, "print the draft delivery payload as JSON")
	reportCmd.Flags().IntVar(&reportLimit, "limit", 5, "show the N most recent sessions (0 = all)")
	reportCmd.Flags().StringVar(&reportSession, "session", "", "show only the session with this id (or id prefix)")
	reportCmd.Flags().BoolVar(&reportAllAuth, "all-authors", false, "consider commits by any author, not just your git user.email")
	reportCmd.Flags().StringVar(&reportFile, "file", "", "path to the JSONL log file (overrides $SWARMIA_LOG_FILE and the default)")
	rootCmd.AddCommand(reportCmd)
}

func runReport(out io.Writer) error {
	repo, err := gitrepo.Find(projectDir())
	if err != nil {
		return err
	}
	path, err := eventsPath(reportFile)
	if err != nil {
		return err
	}
	recs, err := events.ReadFile(path)
	if err != nil {
		return err
	}
	email := gitOutput(repo.Root, "config", "--get", "user.email")
	opts := report.Options{RepoDir: repo.Root, FallbackRoot: repo.Root}
	if !reportAllAuth {
		opts.AuthorEmail = email
	}
	sessions, err := report.Build(recs, opts)
	if err != nil {
		return err
	}
	sessions = selectSessions(sessions)

	if reportJSON {
		return writeReportJSON(out, sessions, gitOutput(repo.Root, "remote", "get-url", "origin"), email)
	}
	if len(sessions) == 0 {
		fmt.Fprintf(out, "No agent sessions logged yet (%s).\n", path)
		return nil
	}
	for i, s := range sessions {
		if i > 0 {
			fmt.Fprintln(out)
		}
		writeSessionText(out, s)
	}
	return nil
}

func selectSessions(sessions []report.Session) []report.Session {
	if reportSession != "" {
		var out []report.Session
		for _, s := range sessions {
			if strings.HasPrefix(s.ID, reportSession) {
				out = append(out, s)
			}
		}
		return out
	}
	if reportLimit > 0 && len(sessions) > reportLimit {
		return sessions[:reportLimit]
	}
	return sessions
}

func writeSessionText(out io.Writer, s report.Session) {
	local := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }
	model := s.Model
	if model == "" {
		model = s.Agent
	}
	fmt.Fprintf(out, "Session %s  %s  %s → %s\n", shortID(s.ID), model, local(s.StartedAt), s.EndedAt.Local().Format("15:04"))
	fmt.Fprintf(out, "  %d prompt(s), %d edit(s), %d AI line(s) at session end\n", len(s.Prompts), s.Edits, s.AILines)
	if len(s.Commits) == 0 {
		if s.AILines > 0 {
			fmt.Fprintln(out, "  not committed yet")
		}
		return
	}
	survived := 0
	for _, c := range s.Commits {
		state := "local "
		if c.Pushed {
			state = "pushed"
		}
		fmt.Fprintf(out, "  %s  %s  %s\n", c.SHA[:7], state, truncate(c.Subject, 60))
		for _, f := range c.Files {
			fmt.Fprintf(out, "      %-44s %4d line(s)%s\n", f.Path, len(f.Lines), classBreakdown(f))
		}
		survived += c.Survived()
	}
	if s.AILines > 0 {
		fmt.Fprintf(out, "  %d of %d AI line(s) committed unchanged or reformatted (%d%%)\n",
			survived, s.AILines, 100*survived/s.AILines)
	}
}

func classBreakdown(f report.File) string {
	var parts []string
	for _, c := range []attribution.Class{attribution.Exact, attribution.Normalized, attribution.Modified} {
		if n := f.Count(c); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, c))
		}
	}
	if len(parts) == 1 && f.Count(attribution.Exact) > 0 {
		return ""
	}
	return "  (" + strings.Join(parts, ", ") + ")"
}

// payload is the draft per-session delivery format (see the roadmap). It is
// printed for inspection only; nothing sends it yet.
type payload struct {
	SessionID      string           `json:"session_id"`
	Agent          string           `json:"agent"`
	Model          string           `json:"model,omitempty"`
	MatcherVersion string           `json:"matcher_version"`
	CLIVersion     string           `json:"cli_version"`
	Repo           string           `json:"repo,omitempty"`
	AuthorEmail    string           `json:"author_email,omitempty"`
	StartedAt      time.Time        `json:"started_at"`
	EndedAt        time.Time        `json:"ended_at"`
	Prompts        []payloadPrompt  `json:"prompts"`
	Tools          []payloadTool    `json:"tools"`
	Commits        []payloadCommit  `json:"commits"`
	AILines        int              `json:"ai_lines_generated"`
	AILinesKept    int              `json:"ai_lines_committed"`
	Unsent         *payloadUnsentNB `json:"_note,omitempty"`
}

type payloadPrompt struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

type payloadTool struct {
	At      time.Time `json:"at"`
	Name    string    `json:"name"`
	File    string    `json:"file,omitempty"`
	Command string    `json:"command,omitempty"`
}

type payloadCommit struct {
	SHA    string         `json:"sha"`
	Path   string         `json:"path"`
	Pushed bool           `json:"pushed"`
	Lines  []report.Range `json:"lines"`
}

type payloadUnsentNB struct {
	Text string `json:"text"`
}

func writeReportJSON(out io.Writer, sessions []report.Session, remote, email string) error {
	payloads := make([]payload, 0, len(sessions))
	for _, s := range sessions {
		p := payload{
			SessionID: s.ID, Agent: s.Agent, Model: s.Model,
			MatcherVersion: attribution.Version, CLIVersion: version,
			Repo: repoSlug(remote), AuthorEmail: email,
			StartedAt: s.StartedAt, EndedAt: s.EndedAt,
			Prompts: []payloadPrompt{}, Tools: []payloadTool{}, Commits: []payloadCommit{},
			AILines: s.AILines,
			Unsent:  &payloadUnsentNB{Text: "draft: not sent; prompts are not redacted yet; only pushed commits would be included"},
		}
		for _, pr := range s.Prompts {
			p.Prompts = append(p.Prompts, payloadPrompt{At: pr.At, Text: pr.Text})
		}
		for _, t := range s.Tools {
			p.Tools = append(p.Tools, payloadTool{At: t.At, Name: t.Name, File: t.File, Command: t.Command})
		}
		for _, c := range s.Commits {
			for _, f := range c.Files {
				p.Commits = append(p.Commits, payloadCommit{SHA: c.SHA, Path: f.Path, Pushed: c.Pushed, Lines: report.Ranges(f.Lines)})
			}
			p.AILinesKept += c.Survived()
		}
		payloads = append(payloads, p)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payloads)
}

// repoSlug turns a remote URL into host/owner/name, e.g.
// git@github.com:acme/app.git → github.com/acme/app.
func repoSlug(remote string) string {
	s := strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	if at := strings.Index(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	return strings.Replace(s, ":", "/", 1)
}

func gitOutput(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
