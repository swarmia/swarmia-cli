package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/swarmia/swarmia-cli/internal/config"
	"github.com/swarmia/swarmia-cli/internal/deliver"
	"github.com/swarmia/swarmia-cli/internal/report"
)

var (
	sendFile   string
	sendDryRun bool
)

var sendCmd = &cobra.Command{
	Use:   "send",
	Short: "Deliver changed sessions to Swarmia now",
	Long: "send brings the session store up to date (as `process` does) and delivers every\n" +
		"session whose payload changed since it was last delivered, ignoring the usual\n" +
		"throttling and any backoff after failures. What is sent for each session is\n" +
		"exactly what `report --json` prints.\n\n" +
		"It needs a token (see `swarmia-cli config`). Background runs deliver on their\n" +
		"own, so this is for trying things out or catching up.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		opts, repo, err := processOptions(sendFile, true, true)
		if err != nil {
			return err
		}
		cfg, err := config.Load(repo.Root)
		if err != nil {
			return err
		}
		if cfg.Token == "" && !sendDryRun {
			return fmt.Errorf("no token configured; see `swarmia-cli config`")
		}
		if w := cfg.TokenWarning(); w != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s.\n", w)
		}
		var outcome deliver.Outcome
		var pending int
		opts.AfterAttribute = func(sessions []*report.Session) error {
			if sendDryRun {
				pending = deliver.Pending(sessions, deliveryOptions(repo, cfg, opts, true))
				return nil
			}
			outcome, err = deliver.Deliver(sessions, deliveryOptions(repo, cfg, opts, true))
			return err
		}
		if _, err := report.Process(opts); err != nil {
			return err
		}
		if sendDryRun {
			fmt.Fprintf(out, "%d session(s) would be sent to %s (see `swarmia-cli report --json`)\n", pending, cfg.Endpoint)
			return nil
		}
		fmt.Fprintf(out, "sent %d session(s) to %s", outcome.Sent, cfg.Endpoint)
		if outcome.Rejected > 0 {
			fmt.Fprintf(out, " (%d rejected)", outcome.Rejected)
		}
		fmt.Fprintln(out)
		if w := upgradeWarning(opts.StateDir); w != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", w)
		}
		return nil
	},
}

func init() {
	sendCmd.Flags().StringVar(&sendFile, "file", "", "path to the JSONL log file (overrides $SWARMIA_LOG_FILE and the default)")
	sendCmd.Flags().BoolVar(&sendDryRun, "dry-run", false, "only report how many sessions would be sent")
	rootCmd.AddCommand(sendCmd)
}
