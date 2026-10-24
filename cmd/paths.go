package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/swarmia/swarmia-cli/internal/gitrepo"
)

// projectDir is the directory the CLI operates against: $CLAUDE_PROJECT_DIR when
// set (Claude Code exports it to hook commands), otherwise the current working
// directory (where Codex runs hooks: the session's, maybe below the repository
// root).
func projectDir() string {
	if base := os.Getenv("CLAUDE_PROJECT_DIR"); base != "" {
		return base
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// stateBaseDir is the per-user root for all local state, outside any repository
// so nothing shows up in `git status` or is lost to `git clean -fdx`.
// $SWARMIA_STATE_DIR overrides the OS default.
func stateBaseDir() (string, error) {
	if dir := os.Getenv("SWARMIA_STATE_DIR"); dir != "" {
		return dir, nil
	}
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "swarmia"), nil
	case "windows":
		if dir := os.Getenv("LocalAppData"); dir != "" {
			return filepath.Join(dir, "swarmia"), nil
		}
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "swarmia"), nil
	default:
		if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
			return filepath.Join(dir, "swarmia"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state", "swarmia"), nil
	}
}

// repoKey identifies the repository enclosing dir: its git common dir, so all
// worktrees of a clone share state. Outside a git repo it falls back to the
// directory itself, so sessions there are still captured.
func repoKey(dir string) string {
	if repo, err := gitrepo.Find(dir); err == nil {
		return repo.CommonDir
	}
	if abs, err := filepath.Abs(dir); err == nil {
		if resolved, err := gitrepo.Canonical(abs); err == nil {
			return resolved
		}
		return abs
	}
	return dir
}

// repoStateDir is <stateBase>/repos/<id> for the repository enclosing dir, where
// id is a short hash of repoKey. A repo.json inside records which repo it is,
// for humans browsing the state directory.
func repoStateDir(dir string) (string, error) {
	base, err := stateBaseDir()
	if err != nil {
		return "", err
	}
	key := repoKey(dir)
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(base, "repos", hex.EncodeToString(sum[:8])), nil
}

// ensureRepoStateDir creates the repo's state directory (and its repo.json) if
// needed and returns its path.
func ensureRepoStateDir(dir string) (string, error) {
	stateDir, err := repoStateDir(dir)
	if err != nil {
		return "", err
	}
	meta := filepath.Join(stateDir, "repo.json")
	if _, err := os.Stat(meta); err == nil {
		return stateDir, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	data, _ := json.MarshalIndent(map[string]string{"repo": repoKey(dir)}, "", "  ")
	if err := os.WriteFile(meta, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return stateDir, nil
}

// eventsPath resolves the JSONL log file, first match wins: an explicit flag
// value, then $SWARMIA_LOG_FILE, then events.jsonl in the project's repo state
// directory.
func eventsPath(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if env := os.Getenv("SWARMIA_LOG_FILE"); env != "" {
		return env, nil
	}
	stateDir, err := ensureRepoStateDir(projectDir())
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "events.jsonl"), nil
}
