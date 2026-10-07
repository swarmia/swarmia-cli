package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// config shows what Swarmia last said about upgrading this version, from the
// repository's delivery state, without creating that state.
func TestConfigShowsUpgradeWarning(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	t.Setenv("SWARMIA_TOKEN", "swm_cli_"+strings.Repeat("0", 64))
	run := func() string {
		t.Helper()
		var out bytes.Buffer
		configCmd.SetOut(&out)
		t.Cleanup(func() { configCmd.SetOut(nil) })
		if err := configCmd.RunE(configCmd, nil); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	dir, err := repoStateDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if out := run(); strings.Contains(out, "upgrade") {
		t.Errorf("warning without one recorded:\n%s", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("config created the state directory: %v", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"upgrades":{"` + version + `":{"required":true,"message":"0.1.0 leaks paths","at":"2026-10-07T10:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(dir, "delivery.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(); !strings.Contains(out, "no longer accepts sessions from swarmia-cli "+version) || !strings.Contains(out, "0.1.0 leaks paths") {
		t.Errorf("no upgrade warning:\n%s", out)
	}
}
