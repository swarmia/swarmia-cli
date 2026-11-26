package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/swarmia/swarmia-cli/internal/launcher"
)

// writeLauncher writes the repository's launcher, for opts.launcherVersion
// or this version, and returns how project hooks run it.
func writeLauncher(out io.Writer, repoDir string, opts installOpts) (binaryRef, error) {
	ver := strings.TrimPrefix(opts.launcherVersion, "v")
	if ver == "" {
		ver = strings.TrimPrefix(version, "v")
	}
	if ver == "dev" {
		return binaryRef{}, errors.New("this swarmia-cli is a development build: pass the release the launcher should run with --launcher-version")
	}
	ref := binaryRef{rel: launcher.Path, source: "launcher for swarmia-cli " + ver}
	path := filepath.Join(repoDir, filepath.FromSlash(launcher.Path))
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return binaryRef{}, err
	}
	pinned := launcher.PinnedVersion(old)

	if opts.dryRun {
		switch pinned {
		case "":
			fmt.Fprintf(out, "Launcher (%s): would write it, for swarmia-cli %s\n", launcher.Path, ver)
		case ver:
			fmt.Fprintf(out, "Launcher (%s): already runs swarmia-cli %s\n", launcher.Path, ver)
		default:
			fmt.Fprintf(out, "Launcher (%s): would switch it from swarmia-cli %s to %s\n", launcher.Path, pinned, ver)
		}
		return ref, nil
	}

	sums, err := launcher.FetchSums(context.Background(), ver)
	if err != nil {
		return binaryRef{}, fmt.Errorf("launcher: %w", err)
	}
	script, err := launcher.Script(ver, sums)
	if err != nil {
		return binaryRef{}, fmt.Errorf("launcher: %w", err)
	}
	switch {
	case bytes.Equal(old, script):
		fmt.Fprintf(out, "Launcher (%s): already runs swarmia-cli %s\n", launcher.Path, ver)
	default:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return binaryRef{}, err
		}
		if err := os.WriteFile(path, script, 0o755); err != nil {
			return binaryRef{}, err
		}
		if err := os.Chmod(path, 0o755); err != nil { // WriteFile keeps an existing file's mode
			return binaryRef{}, err
		}
		if pinned == "" || pinned == ver {
			fmt.Fprintf(out, "Launcher (%s): wrote it, for swarmia-cli %s; commit it\n", launcher.Path, ver)
		} else {
			fmt.Fprintf(out, "Launcher (%s): switched it from swarmia-cli %s to %s; commit it\n", launcher.Path, pinned, ver)
		}
	}
	if gitIgnores(repoDir, launcher.Path) {
		fmt.Fprintf(out, "Warning: git ignores %s, so teammates won't get it: commit it with `git add -f %s`, or unignore it.\n",
			launcher.Path, launcher.Path)
	}
	return ref, nil
}

// gitIgnores reports whether git ignores rel in the repository at repoDir.
func gitIgnores(repoDir, rel string) bool {
	return exec.Command("git", "-C", repoDir, "check-ignore", "-q", "--no-index", rel).Run() == nil
}
