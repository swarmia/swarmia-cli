// Package codex knows the parts of OpenAI Codex's configuration swarmia-cli
// touches: its hooks files and the trust records that let those hooks run.
//
// Codex runs a hook only if the user's config.toml holds a trusted_hash for it,
// normally recorded when the user approves the hook with /hooks inside Codex;
// it silently skips any other. The hash covers the handler's definition, so
// changing a hook's command un-trusts it.
package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

// HooksFile is the name of Codex's hooks file, in a project's .codex directory
// or in Home.
const HooksFile = "hooks.json"

// defaultTimeout is the timeout Codex assumes for a handler without one.
const defaultTimeout = 600

// Home is Codex's configuration directory: $CODEX_HOME, else ~/.codex.
func Home() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// ConfigPath is the user's config.toml, where trust records live.
func ConfigPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.toml"), nil
}

// Handler is one command handler in a hooks file, with what Codex hashes.
type Handler struct {
	Event         string  // hooks.json key, e.g. "PostToolUse"
	Group, Index  int     // position: Codex keys trust by it
	Matcher       *string // nil when absent or null
	Command       string
	Timeout       uint64
	Async         bool
	StatusMessage *string
}

// ReadHandlers lists the command handlers in a hooks file. A missing file has
// none.
func ReadHandlers(path string) ([]Handler, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Type          string  `json:"type"`
				Command       string  `json:"command"`
				Timeout       *uint64 `json:"timeout"`
				TimeoutSec    *uint64 `json:"timeoutSec"`
				Async         bool    `json:"async"`
				StatusMessage *string `json:"statusMessage"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	var out []Handler
	for event, groups := range doc.Hooks {
		for gi, g := range groups {
			for hi, h := range g.Hooks {
				if h.Type != "command" {
					continue
				}
				timeout := uint64(defaultTimeout)
				if h.Timeout != nil {
					timeout = *h.Timeout
				} else if h.TimeoutSec != nil {
					timeout = *h.TimeoutSec
				}
				out = append(out, Handler{Event: event, Group: gi, Index: hi, Matcher: g.Matcher,
					Command: h.Command, Timeout: max(timeout, 1), Async: h.Async, StatusMessage: h.StatusMessage})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Event != b.Event {
			return a.Event < b.Event
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Index < b.Index
	})
	return out, nil
}

// Key is the handler's trust key: the hooks file's real path, the event in
// snake_case, and the handler's position.
func (h Handler) Key(hooksPath string) string {
	if real, err := filepath.EvalSymlinks(hooksPath); err == nil {
		hooksPath = real
	}
	return fmt.Sprintf("%s:%s:%d:%d", hooksPath, snakeCase(h.Event), h.Group, h.Index)
}

// Hash is the trusted_hash Codex expects for the handler: SHA-256 over the
// canonical JSON (sorted keys, no whitespace, no HTML escaping) of its
// identity.
func (h Handler) Hash() string {
	handler := map[string]any{
		"type":    "command",
		"command": h.Command,
		"timeout": json.Number(strconv.FormatUint(h.Timeout, 10)),
		"async":   h.Async,
	}
	if h.StatusMessage != nil {
		handler["statusMessage"] = *h.StatusMessage
	}
	identity := map[string]any{"event_name": snakeCase(h.Event), "hooks": []any{handler}}
	if h.Matcher != nil {
		identity["matcher"] = *h.Matcher
	}
	var buf bytes.Buffer
	writeCanonical(&buf, identity)
	sum := sha256.Sum256(buf.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeCanonical(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, k)
			buf.WriteByte(':')
			writeCanonical(buf, t[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, item)
		}
		buf.WriteByte(']')
	default:
		// Scalars as serde_json writes them: Go's default escaping of <, > and &
		// would change the hash of any command with a redirect or &&.
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		enc.Encode(t)
		buf.Truncate(buf.Len() - 1) // Encode's newline
	}
}

// snakeCase turns "PostToolUse" into "post_tool_use".
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// TrustedHashes reads the trust records in a config.toml: key → trusted_hash.
// A missing file has none.
func TrustedHashes(configPath string) (map[string]string, error) {
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseTrust(data, configPath)
}

func parseTrust(data []byte, name string) (map[string]string, error) {
	var doc struct {
		Hooks struct {
			State map[string]struct {
				TrustedHash string `toml:"trusted_hash"`
			} `toml:"state"`
		} `toml:"hooks"`
	}
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	out := map[string]string{}
	for k, v := range doc.Hooks.State {
		out[k] = v.TrustedHash
	}
	return out, nil
}

// Trust records trusted hashes (key → hash) in a config.toml, as approving the
// hooks with /hooks would, and returns how many records it added or changed.
// It edits the file as text, so everything else in it (comments, layout) is
// kept, and checks the result parses with exactly the intended records before
// replacing the file.
func Trust(configPath string, want map[string]string) (int, error) {
	data, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	have, err := parseTrust(data, configPath)
	if err != nil {
		return 0, err
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	text := string(data)
	changed := 0
	for _, k := range keys {
		hash := want[k]
		old, ok := have[k]
		if ok && old == hash {
			continue
		}
		changed++
		header := "[hooks.state." + tomlString(k) + "]"
		line := "trusted_hash = " + tomlString(hash)
		if !ok {
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			if text != "" {
				text += "\n"
			}
			text += header + "\n" + line + "\n"
			continue
		}
		// A stale record: replace its hash in place.
		old = "trusted_hash = " + tomlString(old)
		at := strings.Index(text, header+"\n")
		end := -1
		if at >= 0 {
			end = strings.Index(text[at:], old)
		}
		if end < 0 {
			return 0, fmt.Errorf("%s records trust for %s in a form swarmia-cli can't edit; approve the hooks with /hooks in Codex instead", configPath, k)
		}
		end += at
		text = text[:end] + line + text[end+len(old):]
	}
	if changed == 0 {
		return 0, nil
	}

	check, err := parseTrust([]byte(text), configPath)
	if err != nil {
		return 0, fmt.Errorf("refusing to write %s: the edit would not parse: %w", configPath, err)
	}
	for k, hash := range want {
		if check[k] != hash {
			return 0, fmt.Errorf("refusing to write %s: the edit did not record %s", configPath, k)
		}
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return 0, err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(configPath); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(configPath), ".config.toml.*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return 0, err
	}
	return changed, os.Rename(tmp.Name(), configPath)
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
