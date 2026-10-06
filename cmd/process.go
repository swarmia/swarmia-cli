package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

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
		"log-hook starts it in the background when a session ends or the log grows\n" +
		"large, and report runs it before printing, so running it by hand is rarely\n" +
		"needed. Nothing is sent anywhere.",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, _, err := processOptions(processFile, true, !processBackground)
		if err != nil {
			return quietIf(processBackground, err)
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

// processOptions resolves the repository, log and state directory for the
// current project. byAuthor limits candidate commits to the user's git email.
func processOptions(logFlag string, byAuthor, wait bool) (report.ProcessOptions, gitrepo.Repo, error) {
	repo, err := gitrepo.Find(projectDir())
	if err != nil {
		return report.ProcessOptions{}, gitrepo.Repo{}, err
	}
	logPath, err := eventsPath(logFlag)
	if err != nil {
		return report.ProcessOptions{}, repo, err
	}
	opts := report.ProcessOptions{
		Options:  report.Options{RepoDir: repo.Root, FallbackRoot: repo.Root},
		LogPath:  logPath,
		StateDir: filepath.Dir(logPath),
		Wait:     wait,
		Now:      time.Now(),
	}
	if byAuthor {
		opts.AuthorEmail = gitOutput(repo.Root, "config", "--get", "user.email")
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
