package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/config"
	"github.com/swarmia/swarmia-cli/internal/gitrepo"
	"github.com/swarmia/swarmia-cli/internal/payload"
	"github.com/swarmia/swarmia-cli/internal/report"
	"github.com/swarmia/swarmia-cli/internal/snapshot"
	"github.com/swarmia/swarmia-cli/internal/usage"
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
		"edits, and are yours: authored with your git email, or created on this\n" +
		"machine according to the reflog (--all-authors to include everyone). 'pushed' marks commits a remote-tracking branch contains; only those\n" +
		"would ever be sent.\n\n" +
		"--json prints exactly what delivery would send for each session: pushed\n" +
		"commits only, secrets redacted, commands stripped of heredoc bodies and inline\n" +
		"scripts, prompts withheld if configuration opts out (see `swarmia-cli config`).\n" +
		"Nothing is sent by this command.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReport(cmd.OutOrStdout())
	},
}

func init() {
	reportCmd.Flags().BoolVar(&reportJSON, "json", false, "print exactly what delivery would send, as JSON")
	reportCmd.Flags().IntVar(&reportLimit, "limit", 5, "show the N most recent sessions (0 = all)")
	reportCmd.Flags().StringVar(&reportSession, "session", "", "show only the session with this id (or id prefix)")
	reportCmd.Flags().BoolVar(&reportAllAuth, "all-authors", false, "consider commits by any author, not just your git email")
	reportCmd.Flags().StringVar(&reportFile, "file", "", "path to the JSONL log file (overrides $SWARMIA_LOG_FILE and the default)")
	rootCmd.AddCommand(reportCmd)
}

func runReport(out io.Writer) error {
	opts, repo, err := processOptions(reportFile, !reportAllAuth, true)
	if err != nil {
		return err
	}
	res, err := report.Process(opts)
	if err != nil {
		return err
	}
	sessions := selectSessions(res.Sessions)

	if reportJSON {
		return writeReportJSON(out, sessions, repo)
	}
	if len(sessions) == 0 {
		fmt.Fprintf(out, "No agent sessions logged yet (%s).\n", opts.StateDir)
		return nil
	}
	cfg, err := config.Load(repo.Root)
	if err != nil {
		return err
	}
	showDelivery = cfg.Token != ""
	if w := upgradeWarning(opts.StateDir); showDelivery && w != "" {
		fmt.Fprintf(out, "Warning: %s\n\n", w)
	}
	for i, s := range sessions {
		if i > 0 {
			fmt.Fprintln(out)
		}
		writeSessionText(out, s)
	}
	return nil
}

func selectSessions(sessions []*report.Session) []*report.Session {
	if reportSession != "" {
		var out []*report.Session
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

// showDelivery adds each session's delivery state to the text report; only
// meaningful when a token is configured.
var showDelivery bool

func writeSessionText(out io.Writer, s *report.Session) {
	defer func() {
		if showDelivery {
			fmt.Fprintf(out, "  %s\n", deliveryState(s))
		}
	}()
	local := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }
	model := s.Model
	if model == "" {
		model = s.Agent
	}
	fmt.Fprintf(out, "Session %s  %s  %s → %s\n", shortID(s.ID), model, local(s.StartedAt), s.EndedAt.Local().Format("15:04"))
	shell := ""
	if n := s.ShellEdits(); n > 0 {
		shell = fmt.Sprintf(" (%d via shell)", n)
	}
	fmt.Fprintf(out, "  %d prompt(s), %d edit(s)%s, %d AI line(s) at session end\n", len(s.Prompts), len(s.Edits), shell, s.AILines())
	if s.ShellUnrecorded > 0 {
		fmt.Fprintf(out, "  %d shell command(s) not checked for edits: the worktree snapshot took over %v, failed, or found a git operation stopped part way\n", s.ShellUnrecorded, snapshot.Budget)
	}
	if s.Usage != nil {
		if line := usageSummary(s.Usage.Totals()); line != "" {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	if len(s.Commits()) == 0 {
		if s.AILines() > 0 {
			fmt.Fprintln(out, "  not committed yet")
		}
		return
	}
	for _, c := range s.Commits() {
		state := "local "
		if c.Pushed {
			state = "pushed"
		}
		fmt.Fprintf(out, "  %s  %s  %s\n", c.SHA[:7], state, truncate(c.Subject, 60))
		for _, f := range c.Files {
			fmt.Fprintf(out, "      %-44s %4d line(s)%s\n", f.Path, f.Total(), classBreakdown(f))
		}
	}
	if ai := s.AILines(); ai > 0 {
		survived := s.Survived(func(report.Commit) bool { return true })
		fmt.Fprintf(out, "  %d of %d AI line(s) committed unchanged or reformatted (%d%%)\n",
			survived, ai, 100*survived/ai)
	}
}

// usageSummary condenses token usage into one line, e.g.
// "271 model calls: 96.6M input tokens (99% cache reads), 402k output".
func usageSummary(totals []usage.Totals) string {
	var calls, input, cached, output int64
	for _, t := range totals {
		calls += t.Requests
		cached += t.CacheReadInputTokens
		input += t.InputTokens + t.CacheReadInputTokens + t.CacheCreation5mInputTokens + t.CacheCreation1hInputTokens
		output += t.OutputTokens
	}
	if calls == 0 {
		return ""
	}
	return fmt.Sprintf("%d model call(s): %s input tokens (%d%% cache reads), %s output",
		calls, humanCount(input), 100*cached/max(input, 1), humanCount(output))
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprint(n)
	}
}

func deliveryState(s *report.Session) string {
	switch d := s.Delivery; {
	case d == nil:
		return "not sent yet"
	case d.Rejected != "":
		return "rejected by Swarmia: " + d.Rejected
	default:
		return "sent " + d.SentAt.Local().Format("2006-01-02 15:04")
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

// writeReportJSON prints the payloads delivery would send for the sessions —
// the same builder, with redaction, prompt opt-out and the pushed-only rule.
func writeReportJSON(out io.Writer, sessions []*report.Session, repo gitrepo.Repo) error {
	cfg, err := config.Load(repo.Root)
	if err != nil {
		return err
	}
	ctx := payloadContext(repo, cfg)
	payloads := make([]payload.Payload, 0, len(sessions))
	for _, s := range sessions {
		payloads = append(payloads, payload.Build(s, ctx))
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payloads)
}

// payloadContext gathers what payloads need beyond the session itself.
func payloadContext(repo gitrepo.Repo, cfg config.Config) payload.Context {
	ctx := payload.Context{
		RepoRoot:    repo.Root,
		Repo:        payload.RepoSlug(gitOutput(repo.Root, "remote", "get-url", "origin"), sshHostName),
		AuthorEmail: authorEmail(repo.Root),
		CLIVersion:  version,
		Prompts:     cfg.Prompts,
	}
	if home, err := os.UserHomeDir(); err == nil {
		ctx.Home, ctx.UserName = home, filepath.Base(home)
	}
	if u, err := user.Current(); err == nil {
		ctx.UserName = u.Username
	}
	return ctx
}

// authorEmail is the email the developer's next commit in dir would carry:
// $GIT_AUTHOR_EMAIL, else user.email (git var resolves it as git commit does).
// It's "" when neither is set rather than the address git would guess from
// the user and host names, which would only leak the hostname. The name is
// set to a placeholder: it's unused, and git refuses a missing one too.
func authorEmail(dir string) string {
	ident := gitOutput(dir, "-c", "user.useConfigOnly=true", "-c", "user.name=-", "var", "GIT_AUTHOR_IDENT")
	start, end := strings.LastIndexByte(ident, '<'), strings.LastIndexByte(ident, '>')
	if start < 0 || end < start {
		return ""
	}
	return ident[start+1 : end]
}

func gitOutput(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sshHostName resolves an SSH host alias from ~/.ssh/config to the host it
// connects to, as ssh would: `ssh -G` prints the resolved configuration without
// connecting. It gives "" if ssh isn't available or doesn't answer in time.
func sshHostName(alias string) string {
	if alias == "" || strings.HasPrefix(alias, "-") {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-G", "--", alias).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if name, ok := strings.CutPrefix(line, "hostname "); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
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
