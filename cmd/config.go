package cmd

import (
	"fmt"
	"os"
	"path/filepath"

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
		"It also warns about swarmia-cli hooks Codex won't run because they aren't\n" +
		"trusted yet, and shows it if Swarmia asked this version to be upgraded.\n\n" +
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
		if cfg.Token != "" && root != "" {
			dir, err := repoStateDir(root) // as eventsPath, without creating it
			if env := os.Getenv("SWARMIA_LOG_FILE"); env != "" {
				dir, err = filepath.Dir(env), nil
			}
			if err == nil {
				if w := upgradeWarning(dir); w != "" {
					fmt.Fprintf(out, "\nWarning: %s\n", w)
				}
			}
		}
		for _, path := range codexHooksFiles(root) {
			if untrusted, err := untrustedCodexHooks(path); err == nil && len(untrusted) > 0 {
				fmt.Fprintf(out, "\nWarning: Codex skips %d swarmia-cli hook(s) in %s until you trust them:\n"+
					"run /hooks in Codex and approve them, or `swarmia-cli install --trust`.\n", len(untrusted), displayPath(root, path))
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
}
