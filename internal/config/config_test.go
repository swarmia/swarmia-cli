package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (repo, userDir string) {
	userDir = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", userDir)
	for _, k := range []string{"SWARMIA_TOKEN", "SWARMIA_ENDPOINT", "SWARMIA_PROMPTS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return t.TempDir(), userDir
}

func TestDefaults(t *testing.T) {
	repo, _ := setup(t)
	cfg, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "" || cfg.Endpoint != DefaultEndpoint || !cfg.Prompts {
		t.Errorf("defaults = %+v, want no token, the default endpoint, prompts on", cfg)
	}
}

func TestPrecedence(t *testing.T) {
	repo, userDir := setup(t)
	write(t, filepath.Join(repo, RepoFile), `{"token":"repo-token","endpoint":"https://repo.example"}`)
	write(t, filepath.Join(userDir, "swarmia", "config.json"), `{"token":"user-token"}`)

	cfg, _ := Load(repo)
	if cfg.Token != "user-token" || cfg.Endpoint != "https://repo.example" {
		t.Errorf("got token %q endpoint %q, want user token and repo endpoint", cfg.Token, cfg.Endpoint)
	}
	t.Setenv("SWARMIA_TOKEN", "env-token")
	if cfg, _ := Load(repo); cfg.Token != "env-token" || cfg.TokenSource != "environment" {
		t.Errorf("env should win: %+v", cfg)
	}
}

// Any layer can turn prompts off; none can turn them back on.
func TestPromptsOptOutWins(t *testing.T) {
	repo, userDir := setup(t)
	write(t, filepath.Join(userDir, "swarmia", "config.json"), `{"prompts":false}`)
	write(t, filepath.Join(repo, RepoFile), `{"prompts":true}`)
	t.Setenv("SWARMIA_PROMPTS", "true")
	cfg, _ := Load(repo)
	if cfg.Prompts || cfg.PromptsSource != filepath.Join(userDir, "swarmia", "config.json") {
		t.Errorf("user opt-out lost: %+v", cfg)
	}

	repo2, _ := setup(t)
	t.Setenv("SWARMIA_PROMPTS", "off")
	if cfg, _ := Load(repo2); cfg.Prompts {
		t.Error("SWARMIA_PROMPTS=off ignored")
	}
}

func TestInvalidConfigIsAnError(t *testing.T) {
	repo, _ := setup(t)
	write(t, filepath.Join(repo, RepoFile), `{not json`)
	if _, err := Load(repo); err == nil {
		t.Error("expected a parse error")
	}
	repo2, _ := setup(t)
	t.Setenv("SWARMIA_PROMPTS", "maybe")
	if _, err := Load(repo2); err == nil {
		t.Error("expected an error for SWARMIA_PROMPTS=maybe")
	}
}

func TestTokenWarning(t *testing.T) {
	good := "swm_cli_" + strings.Repeat("0123456789abcdef", 4)
	for tok, wantWarning := range map[string]bool{
		"":                    false,
		good:                  false,
		"swm_cli_abc":         true,
		strings.ToUpper(good): true,
		"ghp_something":       true,
	} {
		if got := (Config{Token: tok}).TokenWarning() != ""; got != wantWarning {
			t.Errorf("TokenWarning(%q) = %v, want %v", tok, got, wantWarning)
		}
	}
}

func TestMaskedToken(t *testing.T) {
	if got := (Config{Token: "swm_abcdefghijkl"}).MaskedToken(); got != "swm_…ijkl" {
		t.Errorf("MaskedToken = %q", got)
	}
}

// mkRepo makes dir look like a repository's worktree root.
func mkRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A .swarmia.json in a directory above a repository applies to it: the
// nearest one, filling in what the repository's own doesn't set, under the
// user's and the environment's.
func TestDirectoryLayer(t *testing.T) {
	_, userDir := setup(t)
	top := t.TempDir()
	write(t, filepath.Join(top, RepoFile), `{"token":"top-token","prompts":false}`)
	write(t, filepath.Join(top, "work", RepoFile), `{"token":"work-token","endpoint":"https://work.example"}`)
	plain := mkRepo(t, filepath.Join(top, "work", "group", "plain"))
	own := mkRepo(t, filepath.Join(top, "work", "own"))
	write(t, filepath.Join(own, RepoFile), `{"token":"own-token"}`)

	cfg, err := Load(plain)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "work-token" || cfg.TokenSource != filepath.Join(top, "work", RepoFile) || cfg.Endpoint != "https://work.example" {
		t.Errorf("plain: %+v, want the nearest directory's token and endpoint", cfg)
	}
	if !cfg.Prompts {
		t.Errorf("plain: a farther directory's file applied: %+v", cfg)
	}
	if cfg, _ := Load(own); cfg.Token != "own-token" || cfg.Endpoint != "https://work.example" {
		t.Errorf("own: %+v, want its own token and the directory's endpoint", cfg)
	}

	// Outside any repository (where an agent was started), the directory's
	// own file applies.
	if cfg, _ := Load(filepath.Join(top, "work")); cfg.Token != "work-token" {
		t.Errorf("work dir: %+v", cfg)
	}

	// The user's file and the environment win over directories.
	write(t, filepath.Join(userDir, "swarmia", "config.json"), `{"token":"user-token"}`)
	if cfg, _ := Load(plain); cfg.Token != "user-token" || cfg.Endpoint != "https://work.example" {
		t.Errorf("with a user token: %+v", cfg)
	}
	t.Setenv("SWARMIA_TOKEN", "env-token")
	if cfg, _ := Load(plain); cfg.Token != "env-token" {
		t.Errorf("with an environment token: %+v", cfg)
	}
}

// A repository inside another (vendored, say) isn't configured by the outer
// one's .swarmia.json, nor by anything above it.
func TestDirectoryLayerStopsAtEnclosingRepository(t *testing.T) {
	setup(t)
	top := t.TempDir()
	write(t, filepath.Join(top, RepoFile), `{"token":"top-token"}`)
	outer := mkRepo(t, filepath.Join(top, "outer"))
	write(t, filepath.Join(outer, RepoFile), `{"token":"outer-token"}`)
	inner := mkRepo(t, filepath.Join(outer, "vendor", "inner"))

	if cfg, _ := Load(inner); cfg.Token != "" {
		t.Errorf("inner: %+v, want no token", cfg)
	}
	if cfg, _ := Load(outer); cfg.Token != "outer-token" {
		t.Errorf("outer: %+v, want its own", cfg)
	}
}
