package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var stateDirCmd = &cobra.Command{
	Use:   "state-dir",
	Short: "Print the local state directory for the current repository",
	Long: "state-dir prints where swarmia-cli keeps local state (such as events.jsonl) for\n" +
		"the repository enclosing $CLAUDE_PROJECT_DIR or the current directory. State\n" +
		"lives outside the repository, keyed by its git common dir, so all worktrees of\n" +
		"a clone share it. Override the base directory with $SWARMIA_STATE_DIR.\n\n" +
		"Example: tail -f \"$(swarmia-cli state-dir)/events.jsonl\"",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := repoStateDir(projectDir())
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), dir)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stateDirCmd)
}
