package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// These are set at build time via -ldflags "-X github.com/swarmia/swarmia-cli/cmd.version=...".
// See the Makefile. Defaults apply to `go run` / `go build` without ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"swarmia-cli %s\ncommit:  %s\nbuilt:   %s\ngo:      %s\nplatform: %s/%s\n",
			version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return err
	},
}

func init() {
	// Enables `swarmia-cli --version` in addition to the `version` subcommand.
	rootCmd.Version = version
	rootCmd.AddCommand(versionCmd)
}
