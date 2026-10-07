package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The expected hashes are what codex-cli 0.161.0 reported for this file
// (app-server hooks/list, currentHash): its own computation, covering a
// command with && and >, a matcher, a statusMessage, async, a timeout clamped
// from 0 to 1, the default timeout and a null matcher.
const hooksJSON = `{"hooks":{
 "SessionStart":[{"hooks":[{"type":"command","command":"echo hi && true >/dev/null","timeout":10}]}],
 "PostToolUse":[{"matcher":"*","hooks":[
   {"type":"command","command":"swarmia-cli log-hook --agent codex","statusMessage":"logging <x>"},
   {"type":"command","command":"b","async":true,"timeout":0}]}],
 "SessionEnd":[{"matcher":null,"hooks":[{"type":"command","command":"swarmia-cli log-hook --agent codex","timeout":3}]}]
}}`

func TestHashesMatchCodex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(hooksJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	hs, err := ReadHandlers(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"post_tool_use:0:0": "sha256:40fd530dee7abc58b204bb3fabf9f82c3e33968a562c7dc5f01ebba9cfc21f0a",
		"post_tool_use:0:1": "sha256:a62ab6bc1a0a4aae32c85a97267cdeac011cc1e32fdf3260d2e6094501b162f0",
		"session_start:0:0": "sha256:3e61f18a377c7332ec3dc0ed0334875ed1f51427433702adce0db8e25941efd4",
		"session_end:0:0":   "sha256:d2a5b5397efc705800babfa4eba86e678c0ec74270f6501419990664c2707041",
	}
	if len(hs) != len(want) {
		t.Fatalf("got %d handlers, want %d", len(hs), len(want))
	}
	real, _ := filepath.EvalSymlinks(path)
	for _, h := range hs {
		key := h.Key(path)
		short := strings.TrimPrefix(key, real+":")
		if short == key {
			t.Errorf("key %q doesn't start with the real path %q", key, real)
		}
		if got := h.Hash(); got != want[short] {
			t.Errorf("%s: hash %s, want %s", short, got, want[short])
		}
	}
}

func TestTrustEditsConfigInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "# my settings\nmodel = \"gpt-5\"\n\n[hooks.state.\"/r/.codex/hooks.json:stop:0:0\"]\ntrusted_hash = \"sha256:old\"\n\n[projects.\"/r\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/r/.codex/hooks.json:stop:0:0":                 "sha256:new",
		`/r "quoted"/.codex/hooks.json:session_end:0:0`: "sha256:end",
	}
	n, err := Trust(path, want)
	if err != nil || n != 2 {
		t.Fatalf("Trust = %d, %v", n, err)
	}
	got, err := TrustedHashes(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "# my settings\nmodel = \"gpt-5\"\n") || !strings.Contains(string(data), "trust_level = \"trusted\"") {
		t.Errorf("lost the rest of the file:\n%s", data)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
	if n, err := Trust(path, want); err != nil || n != 0 {
		t.Errorf("second Trust = %d, %v; want 0, nil", n, err)
	}
}

func TestTrustRefusesUneditableForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "[hooks.state]\n\"/r/.codex/hooks.json:stop:0:0\" = { trusted_hash = \"sha256:old\" }\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trust(path, map[string]string{"/r/.codex/hooks.json:stop:0:0": "sha256:new"}); err == nil {
		t.Error("Trust edited a record it can't safely edit")
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Errorf("file changed:\n%s", data)
	}
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{"PostToolUse": "post_tool_use", "Stop": "stop", "UserPromptSubmit": "user_prompt_submit"} {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q", in, got)
		}
	}
}
