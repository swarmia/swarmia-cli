package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain stops log-hook from launching background `process` runs during
// tests (it would re-run the test binary), and points every per-user location
// (home, agent config, swarmia-cli state) into a temporary directory, so no
// test can ever touch the developer's own Claude Code or Codex setup. Tests
// that need one of these set their own with t.Setenv.
func TestMain(m *testing.M) {
	startBackgroundProcess = func(string) {}
	home, err := os.MkdirTemp("", "swarmia-cli-test-home-")
	if err != nil {
		panic(err)
	}
	for k, v := range map[string]string{
		"HOME":              home,
		"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"),
		"CODEX_HOME":        filepath.Join(home, ".codex"),
		"XDG_CONFIG_HOME":   filepath.Join(home, ".config"),
		"SWARMIA_STATE_DIR": filepath.Join(home, "state"),
	} {
		os.Setenv(k, v)
	}
	os.Unsetenv("CLAUDE_PROJECT_DIR")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
