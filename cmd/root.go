package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var name string

var rootCmd = &cobra.Command{
	Use:   "swarmia-cli",
	Short: "swarmia-cli is the command-line interface for Swarmia",
	Long:  "swarmia-cli is the command-line interface for Swarmia.\n\nIt captures local AI-agent activity so AI-generated code can be attributed to\nthe commits it lands in. See `swarmia-cli log-hook`.",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Hello, %s!\n", name)
		return err
	},
}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().StringVar(&name, "name", "world", "who to greet")
}
