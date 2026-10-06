package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "swarmia-cli",
	Short: "swarmia-cli is the command-line interface for Swarmia",
	Long: "swarmia-cli is the command-line interface for Swarmia.\n\n" +
		"It captures local AI-agent activity so AI-generated code can be attributed to\n" +
		"the commits it lands in. See `swarmia-cli log-hook`.",
	// No Run/RunE: with subcommands present, cobra prints help when invoked bare.
	SilenceUsage: true,
}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
