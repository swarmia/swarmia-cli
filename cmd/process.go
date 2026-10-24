package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/swarmia/swarmia-cli/internal/deliver"
	"github.com/swarmia/swarmia-cli/internal/gitrepo"
	"github.com/swarmia/swarmia-cli/internal/report"
)

var (
	processFile       string
	processBackground bool
)

var processCmd = &cobra.Command{
	Use:   "process",
	Short: "Fold the event log into per-session records and attribute them to commits",
	Long: "process brings the local session store up to date: it folds the raw event log\n" +
		"into one compact record per session (dropping the bulky whole-file payloads)\n" +
		"and empties the log, matches each session's edits to commits, and deletes\n" +
		"sessions inactive for 30 days. Attribution is only recomputed when a session's\n" +
		"edits or candidate commits changed; rebased or amended commits drop out and\n" +
		"their replacements are matched.\n\n" +
		"Changed sessions are then delivered to Swarmia (see `swarmia-cli send`), each\n" +
		"repository's part with the token configured for it (see `swarmia-cli config`);\n" +
		"without one nothing is sent.\n\n" +
		"log-hook starts it in the background when a session ends or the log grows\n" +
		"large, and report runs it (without delivering) before printing, so running it\n" +
		"by hand is rarely needed.",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, _, err := processOptions(processFile, true, !processBackground)
		if err != nil {
			return quietIf(processBackground, err)
		}
		var delivered deliver.Outcome
		opts.AfterAttribute = func(sessions []*report.Session) error {
			delivered, err = deliver.Deliver(sessions, deliveryOptions(opts, false))
			return err
		}
		res, err := report.Process(opts)
		if errors.Is(err, report.ErrBusy) {
			return nil // a run is already in progress
		}
		if err != nil {
			return quietIf(processBackground, err)
		}
		if !processBackground {
			fmt.Fprintf(cmd.OutOrStdout(), "folded %d event(s); %d session(s) stored; %d pruned\n",
				res.Folded, len(res.Sessions), res.Pruned)
			if delivered.Sent > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "sent %d session(s) to Swarmia\n", delivered.Sent)
			}
		}
		return nil
	},
}

func init() {
	processCmd.Flags().StringVar(&processFile, "file", "", "path to the JSONL log file (overrides $SWARMIA_LOG_FILE and the default)")
	processCmd.Flags().BoolVar(&processBackground, "background", false, "skip if another run is in progress and print nothing (used by log-hook)")
	rootCmd.AddCommand(processCmd)
}

// quietIf drops err for background runs, which have nowhere to report it.
func quietIf(background bool, err error) error {
	if background {
		return nil
	}
	return err
}

// processOptions resolves the log and state directory for the current
// project, and its repository: the zero Repo if the project isn't in one (it
// may hold several), whose sessions are still processed. byAuthor limits each
// repository's candidate commits to the user's git email there.
func processOptions(logFlag string, byAuthor, wait bool) (report.ProcessOptions, gitrepo.Repo, error) {
	repo, _ := gitrepo.Find(projectDir())
	logPath, err := eventsPath(logFlag)
	if err != nil {
		return report.ProcessOptions{}, repo, err
	}
	opts := report.ProcessOptions{
		Options:  report.Options{FallbackRoot: repo.Root},
		LogPath:  logPath,
		StateDir: filepath.Dir(logPath),
		Wait:     wait,
		Now:      time.Now(),
	}
	if byAuthor {
		opts.AuthorEmail = authorEmail
	}
	return opts, repo, nil
}

// backgroundLogSize is the log size at which log-hook folds it without waiting
// for the session to end.
const backgroundLogSize = 8 << 20

// startBackgroundProcess launches `swarmia-cli process --background`, detached
// so it outlives the hook and never delays the agent. It's a variable so tests
// can observe it.
var startBackgroundProcess = func(logPath string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "process", "--background", "--file", logPath)
	cmd.Dir = projectDir()
	detach(cmd)
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// upgradeWarning is what Swarmia last said about upgrading this swarmia-cli
// version, from the delivery state in stateDir, for the developer; "" if it
// said nothing.
func upgradeWarning(stateDir string) string {
	u, ok := deliver.UpgradeFor(stateDir, version)
	switch {
	case !ok:
		return ""
	case u.Required:
		return fmt.Sprintf("Swarmia no longer accepts sessions from swarmia-cli %s, so none are sent until you upgrade it: %s", version, u.Message)
	default:
		return fmt.Sprintf("Swarmia asks you to upgrade swarmia-cli %s: %s", version, u.Message)
	}
}

// deliveryOptions configures delivery for the project.
func deliveryOptions(opts report.ProcessOptions, force bool) deliver.Options {
	return deliver.Options{
		CLIVersion: version,
		Context:    payloadContext(),
		StateDir:   opts.StateDir,
		Now:        opts.Now,
		Force:      force,
	}
}
