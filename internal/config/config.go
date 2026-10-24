// Package config resolves swarmia-cli settings from four layers:
//
//   - a .swarmia.json in a directory above the repository, the nearest one,
//     for a developer who keeps many repositories in one directory: up to the
//     repository enclosing it, if any, whose own file doesn't apply;
//   - the repository's .swarmia.json, which a team may commit (its token is
//     write-only: it can submit data for its organization and nothing else);
//   - the user's ~/.config/swarmia/config.json ($XDG_CONFIG_HOME honoured);
//   - environment variables: SWARMIA_TOKEN, SWARMIA_ENDPOINT, SWARMIA_PROMPTS.
//
// The token and endpoint come from the most specific layer that sets them
// (environment, then user, then repository, then directory): what an
// organization distributes to machines (MDM) overrides files on disk, and a
// directory's file only fills in for repositories that don't say, so that a
// repository of another organization in the same directory keeps its own
// token. Prompts
// are shared unless any layer turns them off: an individual can always
// withhold their own prompts, and so can a repository's configuration.
//
// swarmia-cli only reads these files; it never writes them.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// File is the shape of both config files.
type File struct {
	Token    string `json:"token,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Prompts  *bool  `json:"prompts,omitempty"`
}

// Config is the effective configuration, with where each value came from.
type Config struct {
	Token          string
	TokenSource    string
	Endpoint       string
	EndpointSource string
	Prompts        bool
	PromptsSource  string
}

// DefaultEndpoint is Swarmia's ingest endpoint for the CLI; the endpoint
// setting overrides it (e.g. for testing against a local server).
const DefaultEndpoint = "https://hook.swarmia.com/cli"

// RepoFile is the repository config's file name, at the worktree root.
const RepoFile = ".swarmia.json"

// UserPath is the user config's location.
func UserPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "swarmia", "config.json"), nil
}

// Load resolves the configuration for dir: a repository's worktree root, or a
// directory in no repository (where an agent was started, say), which has no
// repository layer and whose own .swarmia.json is a directory one.
func Load(dir string) (Config, error) {
	cfg := Config{Endpoint: DefaultEndpoint, EndpointSource: "default", Prompts: true, PromptsSource: "default"}

	type layer struct {
		name string
		file File
	}
	var layers []layer // least to most specific
	var paths []string
	if dir != "" {
		above, repoFile := dir, ""
		if isWorktreeRoot(dir) {
			above, repoFile = filepath.Dir(dir), filepath.Join(dir, RepoFile)
		}
		if path := directoryFile(above); path != "" {
			paths = append(paths, path)
		}
		if repoFile != "" {
			paths = append(paths, repoFile)
		}
	}
	for _, path := range paths {
		f, err := readFile(path)
		if err != nil {
			return cfg, err
		}
		layers = append(layers, layer{path, f})
	}
	if path, err := UserPath(); err == nil {
		f, err := readFile(path)
		if err != nil {
			return cfg, err
		}
		layers = append(layers, layer{path, f})
	}
	env := File{Token: os.Getenv("SWARMIA_TOKEN"), Endpoint: os.Getenv("SWARMIA_ENDPOINT")}
	if v, ok := os.LookupEnv("SWARMIA_PROMPTS"); ok {
		b, err := parseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("SWARMIA_PROMPTS: %w", err)
		}
		env.Prompts = &b
	}
	layers = append(layers, layer{"environment", env})

	for _, l := range layers {
		if l.file.Token != "" {
			cfg.Token, cfg.TokenSource = l.file.Token, l.name
		}
		if l.file.Endpoint != "" {
			cfg.Endpoint, cfg.EndpointSource = l.file.Endpoint, l.name
		}
		if l.file.Prompts != nil && !*l.file.Prompts && cfg.Prompts {
			cfg.Prompts, cfg.PromptsSource = false, l.name
		}
	}
	return cfg, nil
}

// directoryFile is the nearest .swarmia.json in dir or a directory above it,
// stopping at a repository's worktree root: that file is the repository's,
// not one for the repositories below it.
func directoryFile(dir string) string {
	for {
		if isWorktreeRoot(dir) {
			return ""
		}
		path := filepath.Join(dir, RepoFile)
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func isWorktreeRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func readFile(path string) (File, error) {
	var f File
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("parsing %s: %w", path, err)
	}
	return f, nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("want true or false, got %q", v)
}

// tokenPattern is the format Swarmia issues CLI tokens in. The ingest endpoint
// refuses anything else (401), so a mismatch is worth a warning — but only a
// warning, in case the format evolves.
var tokenPattern = regexp.MustCompile(`^swm_cli_[0-9a-f]{64,}$`)

// TokenWarning describes what looks wrong with the configured token, or "".
func (c Config) TokenWarning() string {
	if c.Token == "" || tokenPattern.MatchString(c.Token) {
		return ""
	}
	return "token doesn't look like a Swarmia CLI token (swm_cli_ followed by 64+ lowercase hex characters); the endpoint will likely refuse it"
}

// MaskedToken shows enough of the token to recognise it.
func (c Config) MaskedToken() string {
	if len(c.Token) <= 8 {
		return strings.Repeat("*", len(c.Token))
	}
	return c.Token[:4] + "…" + c.Token[len(c.Token)-4:]
}
