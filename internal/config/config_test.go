package config

import (
	"os"
	"path/filepath"
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

func TestMaskedToken(t *testing.T) {
	if got := (Config{Token: "swm_abcdefghijkl"}).MaskedToken(); got != "swm_…ijkl" {
		t.Errorf("MaskedToken = %q", got)
	}
}
