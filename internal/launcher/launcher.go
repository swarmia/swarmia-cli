// Package launcher writes the launcher a repository commits to run a pinned
// swarmia-cli without npm: a POSIX sh script, like gradlew, that downloads its
// version's release on first use, checks it against the checksums it holds,
// and runs it from a per-user cache.
package launcher

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Path is where the launcher lives, relative to the repository root.
const Path = ".swarmia/swarmia-cli"

// DownloadURLEnv names the environment variable that points downloads at a
// mirror of the GitHub releases, laid out as <url>/v<version>/<file>.
const DownloadURLEnv = "SWARMIA_CLI_DOWNLOAD_URL"

// DefaultDownloadURL is where releases are downloaded from.
const DefaultDownloadURL = "https://github.com/swarmia/swarmia-cli/releases/download"

// Platforms are the platforms each release has a binary for.
var Platforms = []string{"darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64"}

//go:embed swarmia-cli.sh
var template string

var (
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	pinnedRe  = regexp.MustCompile(`(?m)^version=(\S+)$`)
)

// Script is the launcher for version, with sums, each platform's release
// tarball's SHA256.
func Script(version string, sums map[string]string) ([]byte, error) {
	if !versionRe.MatchString(version) {
		return nil, fmt.Errorf("%q isn't a release version", version)
	}
	s := strings.ReplaceAll(template, "@VERSION@", version)
	for _, p := range Platforms {
		if !sha256Re.MatchString(sums[p]) {
			return nil, fmt.Errorf("no checksum for %s", p)
		}
		s = strings.ReplaceAll(s, "@SHA256_"+strings.ToUpper(strings.ReplaceAll(p, "-", "_"))+"@", sums[p])
	}
	return []byte(s), nil
}

// PinnedVersion is the version a launcher runs, or "" if script isn't one.
func PinnedVersion(script []byte) string {
	if m := pinnedRe.FindSubmatch(script); m != nil {
		return string(m[1])
	}
	return ""
}

// TarballName is a release's file for platform.
func TarballName(version, platform string) string {
	return "swarmia-cli_" + version + "_" + platform + ".tar.gz"
}

// DownloadURL is where releases are downloaded from: DefaultDownloadURL or
// the mirror in DownloadURLEnv.
func DownloadURL() string {
	if u := os.Getenv(DownloadURLEnv); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	return DefaultDownloadURL
}

// FetchSums downloads version's SHA256SUMS and returns each platform's.
func FetchSums(ctx context.Context, version string) (map[string]string, error) {
	url := DownloadURL() + "/v" + version + "/SHA256SUMS"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading checksums: %s: %s", url, resp.Status)
	}
	sums, err := ParseSums(resp.Body, version)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return sums, nil
}

// ParseSums reads a release's SHA256SUMS (sha256sum's format) and returns
// each platform's tarball's checksum. It fails unless all platforms have one.
func ParseSums(r io.Reader, version string) (map[string]string, error) {
	byFile := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		sum, file, ok := strings.Cut(sc.Text(), "  ")
		if ok && sha256Re.MatchString(sum) {
			byFile[strings.TrimPrefix(file, "*")] = sum
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, p := range Platforms {
		sum, ok := byFile[TarballName(version, p)]
		if !ok {
			return nil, fmt.Errorf("no checksum for %s", TarballName(version, p))
		}
		sums[p] = sum
	}
	return sums, nil
}

// CacheDir is where launchers keep the versions they download, a directory
// per version: ${XDG_CACHE_HOME:-~/.cache}/swarmia/cli.
func CacheDir() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "swarmia", "cli"), nil
}
