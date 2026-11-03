// Package npm holds tests for the npm packages; it isn't part of any of them.
package npm

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBinary stands in for a platform package's swarmia-cli: it prints the
// path it was started by, its working directory, arguments and stdin.
const fakeBinary = `#!/bin/sh
printf 'exe=%s\n' "$0"
printf 'pwd=%s\n' "$PWD"
printf 'args=%s|%s\n' "$1" "$2"
printf 'stdin=%s\n' "$(cat)"
exit 3
`

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// installPnpm lays out what pnpm's isolated linker installs in repo, and
// returns the platform binary's real path.
func installPnpm(t *testing.T, repo, launcher string) string {
	store := filepath.Join(repo, "node_modules", ".pnpm")
	write(t, filepath.Join(store, "@swarmia+cli@1.0.0", "node_modules", "@swarmia", "cli", "bin", "swarmia-cli"), launcher)
	bin := filepath.Join(store, "@swarmia+cli-linux-x64@1.0.0", "node_modules", "@swarmia", "cli-linux-x64", "bin", "swarmia-cli")
	write(t, bin, fakeBinary)
	symlink(t, "../../../@swarmia+cli-linux-x64@1.0.0/node_modules/@swarmia/cli-linux-x64",
		filepath.Join(store, "@swarmia+cli@1.0.0", "node_modules", "@swarmia", "cli-linux-x64"))
	symlink(t, "../.pnpm/@swarmia+cli@1.0.0/node_modules/@swarmia/cli",
		filepath.Join(repo, "node_modules", "@swarmia", "cli"))
	return bin
}

// installNpm lays out what npm installs in repo, and returns the platform
// binary's real path.
func installNpm(t *testing.T, repo, launcher string) string {
	modules := filepath.Join(repo, "node_modules")
	write(t, filepath.Join(modules, "@swarmia", "cli", "bin", "swarmia-cli"), launcher)
	bin := filepath.Join(modules, "@swarmia", "cli-linux-x64", "bin", "swarmia-cli")
	write(t, bin, fakeBinary)
	symlink(t, "../@swarmia/cli/bin/swarmia-cli", filepath.Join(modules, ".bin", "swarmia-cli"))
	return bin
}

// TestLauncher checks the launcher runs the platform binary by its real,
// clean path (macOS 27 breaks TLS in a binary started through a symlink
// followed by "..", which pnpm's layout gives), and leaves everything else as
// it was: the working directory, arguments, stdin and exit code.
func TestLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is POSIX sh")
	}
	launcher, err := os.ReadFile(filepath.Join("cli", "bin", "swarmia-cli"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		install func(t *testing.T, repo, launcher string) string
		run     string // relative to the repository
	}{
		{"pnpm", installPnpm, "node_modules/@swarmia/cli/bin/swarmia-cli"},
		{"npm", installNpm, "node_modules/@swarmia/cli/bin/swarmia-cli"},
		{"npm .bin", installNpm, "node_modules/.bin/swarmia-cli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(tmp, "a repo") // a space, as in real paths
			bin := tc.install(t, repo, string(launcher))
			// The working directory is reached through a symlink, which the
			// binary must still see it by.
			if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			cwd := filepath.Join(tmp, "linked repo")
			symlink(t, "a repo", cwd)

			for _, path := range []string{
				filepath.Join(cwd, tc.run),
				"./" + tc.run,
				"../" + tc.run, // from src/
			} {
				dir := cwd
				if strings.HasPrefix(path, "../") {
					dir = filepath.Join(cwd, "src")
				}
				for _, shell := range []string{"", "dash", "bash", "ksh"} {
					name := shell
					if name == "" {
						name = "shebang"
					}
					t.Run(name+" "+path, func(t *testing.T) {
						testRun(t, shell, path, dir, bin)
					})
				}
			}
		})
	}
}

// testRun runs the launcher at path from dir, with shell or else by its
// shebang, and checks it ran the platform binary at bin as it should.
func testRun(t *testing.T, shell, path, dir, bin string) {
	args := []string{path, "a b", "c"}
	if shell != "" {
		if _, err := exec.LookPath(shell); err != nil {
			t.Skipf("no %s", shell)
		}
		args = append([]string{shell}, args...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PWD="+dir, "CDPATH=/")
	cmd.Stdin = strings.NewReader("payload")
	out, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Fatalf("exit: %v, want status 3\n%s", err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	want := map[string]string{"exe": bin, "pwd": dir, "args": "a b|c", "stdin": "payload"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestLauncherDeletedWorkingDirectory checks the launcher still runs the
// binary when it can't come back to the working directory, as after a hook's
// worktree is removed.
func TestLauncherDeletedWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is POSIX sh")
	}
	launcher, err := os.ReadFile(filepath.Join("cli", "bin", "swarmia-cli"))
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "repo")
	installPnpm(t, repo, string(launcher))
	for _, shell := range []string{"sh", "dash", "bash", "ksh"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("no %s", shell)
			}
			gone, err := os.MkdirTemp(tmp, "gone")
			if err != nil {
				t.Fatal(err)
			}
			script := `cd "$1" && rmdir "$1" && exec ` + shell + ` "$2"`
			cmd := exec.Command("sh", "-c", script, "sh", gone, filepath.Join(repo, "node_modules", "@swarmia", "cli", "bin", "swarmia-cli"))
			cmd.Stdin = strings.NewReader("payload")
			out, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
				t.Fatalf("exit: %v, want status 3\n%s", err, out)
			}
			if !strings.Contains(string(out), "stdin=payload") {
				t.Errorf("binary didn't run as expected:\n%s", out)
			}
		})
	}
}
