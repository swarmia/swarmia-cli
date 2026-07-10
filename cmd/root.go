package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var name string

var rootCmd = &cobra.Command{
	Use:   "swarmia",
	Short: "swarmia-cli is the command-line interface for Swarmia",
	Long:  "swarmia-cli is the command-line interface for Swarmia.\n\nAt this stage it just says hello. More commands are on the way.",
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
