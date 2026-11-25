package launcher

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBinary stands in for swarmia-cli: it prints its arguments and stdin.
const fakeBinary = `#!/bin/sh
printf 'args=%s|%s\n' "$1" "$2"
printf 'stdin=%s\n' "$(cat)"
exit 3
`

// tarball is a release tarball holding files.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// release serves version's tarballs (the same one for every platform) and
// SHA256SUMS, as GitHub serves a release's assets, and counts tarball
// downloads.
type release struct {
	*httptest.Server
	tarball   []byte
	sums      map[string]string
	downloads atomic.Int32
}

func newRelease(t *testing.T, version string) *release {
	r := &release{
		tarball: tarball(t, map[string]string{"swarmia-cli": fakeBinary, "LICENSE": "MIT"}),
		sums:    map[string]string{},
	}
	sum := sha256.Sum256(r.tarball)
	var sumsFile strings.Builder
	for _, p := range Platforms {
		r.sums[p] = hex.EncodeToString(sum[:])
		sumsFile.WriteString(r.sums[p] + "  " + TarballName(version, p) + "\n")
	}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		dir, file := "/v"+version+"/", strings.TrimPrefix(req.URL.Path, "/v"+version+"/")
		switch {
		case !strings.HasPrefix(req.URL.Path, dir):
			http.NotFound(w, req)
		case file == "SHA256SUMS":
			w.Write([]byte(sumsFile.String()))
		case strings.HasSuffix(file, ".tar.gz"):
			r.downloads.Add(1)
			w.Write(r.tarball)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

// shells are the shells the launcher runs under: "" for its shebang.
var shells = []string{"", "dash", "bash", "ksh"}

// run runs the launcher at path under shell with args and stdin, in env.
func run(t *testing.T, shell, path string, env []string, stdin string, args ...string) (string, int) {
	t.Helper()
	argv := append([]string{path}, args...)
	if shell != "" {
		argv = append([]string{shell}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// setup writes the launcher for r's version, with sums, and returns its path
// and the environment it runs in: its own cache, downloading from r.
func setup(t *testing.T, shell string, r *release, sums map[string]string) (string, []string, string) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is POSIX sh")
	}
	if shell != "" {
		if _, err := exec.LookPath(shell); err != nil {
			t.Skipf("no %s", shell)
		}
	}
	script, err := Script("1.2.3", sums)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "a repo", Path) // a space, as in real paths
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, script, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache dir")
	return path, []string{"XDG_CACHE_HOME=" + cache, DownloadURLEnv + "=" + r.URL + "/"}, filepath.Join(cache, "swarmia", "cli", "1.2.3")
}

// waitFor waits up to 10 seconds for cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A command run through the launcher downloads the release first, then runs
// it with the arguments, stdin and exit code passing through; after that it
// runs the cached binary.
func TestLauncherDownloadsThenRuns(t *testing.T) {
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			r := newRelease(t, "1.2.3")
			path, env, cache := setup(t, shell, r, r.sums)
			for i := range 2 {
				out, code := run(t, shell, path, env, "payload", "report", "a b")
				if code != 3 || !strings.Contains(out, "args=report|a b\nstdin=payload\n") {
					t.Fatalf("run %d: exit %d, output:\n%s", i, code, out)
				}
				if got := strings.Contains(out, "downloading"); got != (i == 0) {
					t.Errorf("run %d: says it's downloading: %v\n%s", i, got, out)
				}
			}
			if n := r.downloads.Load(); n != 1 {
				t.Errorf("downloaded %d times, want once", n)
			}
			if !exists(filepath.Join(cache, "swarmia-cli")) {
				t.Error("binary isn't cached")
			}
			if left, _ := filepath.Glob(filepath.Join(cache, "..", ".download.*")); len(left) > 0 {
				t.Errorf("left behind %v", left)
			}
		})
	}
}

// A hook doesn't wait for the download: it starts one in the background and
// does nothing. Once that's done, hooks run the binary.
func TestLauncherHookDownloadsInBackground(t *testing.T) {
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			r := newRelease(t, "1.2.3")
			path, env, cache := setup(t, shell, r, r.sums)
			out, code := run(t, shell, path, env, "event", "log-hook", "--agent")
			if code != 0 || out != "" {
				t.Fatalf("first hook: exit %d, output %q; want 0 and nothing", code, out)
			}
			waitFor(t, "the background download", func() bool {
				return exists(filepath.Join(cache, "swarmia-cli")) && !exists(cache+".lock")
			})
			out, code = run(t, shell, path, env, "event", "log-hook", "--agent")
			if code != 3 || !strings.Contains(out, "args=log-hook|--agent\nstdin=event\n") {
				t.Fatalf("hook after the download: exit %d, output:\n%s", code, out)
			}
		})
	}
}

// A release that doesn't match its checksum isn't run or cached. After a
// failed background download, hooks don't retry until its lock expires.
func TestLauncherRejectsWrongChecksum(t *testing.T) {
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			r := newRelease(t, "1.2.3")
			wrong := map[string]string{}
			for _, p := range Platforms {
				wrong[p] = strings.Repeat("0", 64)
			}
			path, env, cache := setup(t, shell, r, wrong)

			out, code := run(t, shell, path, env, "", "report")
			if code != 1 || !strings.Contains(out, "doesn't match its checksum") {
				t.Fatalf("exit %d, want 1 and a checksum error; output:\n%s", code, out)
			}
			if exists(filepath.Join(cache, "swarmia-cli")) {
				t.Fatal("cached a binary that doesn't match its checksum")
			}

			before := r.downloads.Load()
			if out, code := run(t, shell, path, env, "", "log-hook"); code != 0 || out != "" {
				t.Fatalf("hook: exit %d, output %q; want 0 and nothing", code, out)
			}
			waitFor(t, "the background download to fail", func() bool {
				log, _ := os.ReadFile(cache + ".log")
				return strings.Contains(string(log), "doesn't match its checksum")
			})
			if out, code := run(t, shell, path, env, "", "log-hook"); code != 0 || out != "" {
				t.Fatalf("second hook: exit %d, output %q; want 0 and nothing", code, out)
			}
			time.Sleep(100 * time.Millisecond)
			if n := r.downloads.Load() - before; n != 1 {
				t.Errorf("hooks downloaded %d times, want once until the lock expires", n)
			}
			if exists(filepath.Join(cache, "swarmia-cli")) {
				t.Error("cached a binary that doesn't match its checksum")
			}
		})
	}
}

// A release that can't be downloaded fails the command, saying where from.
func TestLauncherDownloadFails(t *testing.T) {
	r := newRelease(t, "1.2.3")
	path, env, _ := setup(t, "", r, r.sums)
	env = append(env, DownloadURLEnv+"="+r.URL+"/missing")
	out, code := run(t, "", path, env, "", "report")
	if code != 1 || !strings.Contains(out, "can't download "+r.URL+"/missing/v1.2.3/") {
		t.Fatalf("exit %d, want 1 and the URL; output:\n%s", code, out)
	}
}

func TestFetchSums(t *testing.T) {
	r := newRelease(t, "1.2.3")
	t.Setenv(DownloadURLEnv, r.URL)
	sums, err := FetchSums(context.Background(), "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range Platforms {
		if sums[p] != r.sums[p] {
			t.Errorf("%s: %q, want %q", p, sums[p], r.sums[p])
		}
	}
	if _, err := FetchSums(context.Background(), "9.9.9"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing release: %v, want a 404", err)
	}
}

func TestParseSumsNeedsEveryPlatform(t *testing.T) {
	in := strings.Repeat("a", 64) + "  " + TarballName("1.2.3", "linux-x64") + "\n"
	if _, err := ParseSums(strings.NewReader(in), "1.2.3"); err == nil || !strings.Contains(err.Error(), "darwin-arm64") {
		t.Errorf("err = %v, want one naming darwin-arm64", err)
	}
}

func TestScript(t *testing.T) {
	sums := map[string]string{}
	for i, p := range Platforms {
		sums[p] = strings.Repeat(string(rune('a'+i)), 64)
	}
	script, err := Script("1.2.3-rc.1", sums)
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`@[A-Z0-9_]+@`).Find(script); m != nil {
		t.Errorf("placeholder %s left in the script", m)
	}
	if !strings.Contains(string(script), "sha256_linux_arm64="+sums["linux-arm64"]+"\n") {
		t.Errorf("linux-arm64's checksum isn't in the script")
	}
	if got := PinnedVersion(script); got != "1.2.3-rc.1" {
		t.Errorf("PinnedVersion = %q", got)
	}
	if _, err := Script("dev", sums); err == nil {
		t.Error("Script accepted version dev")
	}
	delete(sums, "darwin-x64")
	if _, err := Script("1.2.3", sums); err == nil {
		t.Error("Script accepted missing checksums")
	}
}
