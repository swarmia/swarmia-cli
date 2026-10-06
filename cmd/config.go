package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/swarmia/swarmia-cli/internal/config"
	"github.com/swarmia/swarmia-cli/internal/gitrepo"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show the effective configuration and where each value comes from",
	Long: "config prints the settings swarmia-cli would use in this repository, and the\n" +
		"source of each. Settings come from, most specific first:\n\n" +
		"  environment   SWARMIA_TOKEN, SWARMIA_ENDPOINT, SWARMIA_PROMPTS\n" +
		"  user          ~/.config/swarmia/config.json ($XDG_CONFIG_HOME honoured)\n" +
		"  repository    .swarmia.json at the worktree root (may be committed; its\n" +
		"                token can only submit data, so it isn't a secret)\n\n" +
		"File format: {\"token\": \"…\", \"endpoint\": \"…\", \"prompts\": false}\n\n" +
		"Prompts are shared unless any layer sets prompts to false: you can always\n" +
		"withhold your own prompts, e.g. with SWARMIA_PROMPTS=0. swarmia-cli never\n" +
		"writes these files.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		root := ""
		if repo, err := gitrepo.Find(projectDir()); err == nil {
			root = repo.Root
		}
		cfg, err := config.Load(root)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		token := "(not set — nothing will be sent)"
		if cfg.Token != "" {
			token = cfg.MaskedToken() + "  (" + cfg.TokenSource + ")"
		}
		fmt.Fprintf(out, "token:     %s\n", token)
		fmt.Fprintf(out, "endpoint:  %s  (%s)\n", cfg.Endpoint, cfg.EndpointSource)
		prompts := "shared"
		if !cfg.Prompts {
			prompts = "withheld"
		}
		fmt.Fprintf(out, "prompts:   %s  (%s)\n", prompts, cfg.PromptsSource)
		if w := cfg.TokenWarning(); w != "" {
			fmt.Fprintf(out, "\nWarning: %s.\n", w)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
}
