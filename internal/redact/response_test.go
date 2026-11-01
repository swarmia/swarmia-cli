package redact

import (
	"strings"
	"testing"
)

func TestResponse(t *testing.T) {
	long := strings.Repeat("x", 130)
	cases := []struct {
		name, in, want string
	}{
		{"prose survives", "Fixed the off-by-one in `parseRange` and ran `go test ./...`.",
			"Fixed the off-by-one in `parseRange` and ran `go test ./...`."},
		{"fenced block", "Here's the fix:\n\n```go\nfunc f() {\n\treturn 1\n}\n```\n\nDone.",
			"Here's the fix:\n\n[go code omitted: 3 lines]\n\nDone."},
		{"fence without language", "Run:\n```\nmake test\n```", "Run:\n[code omitted: 1 line]"},
		{"tilde fence", "~~~\na\nb\n~~~\nafter", "[code omitted: 2 lines]\nafter"},
		{"indented in a list", "1. Change it:\n   ```diff\n   -a\n   +b\n   ```\n2. Test",
			"1. Change it:\n   [diff code omitted: 2 lines]\n2. Test"},
		{"longer fence holds a shorter one", "````md\n```js\nx\n```\n````\nend",
			"[md code omitted: 3 lines]\nend"},
		{"unclosed fence runs to the end", "Start\n```python\nprint(1)\nprint(2)", "Start\n[python code omitted: 2 lines]"},
		{"long inline code", "It was `" + long + "` here.", "It was `[130 chars omitted]` here."},
		{"secrets redacted", "Set it to ghp_" + strings.Repeat("a", 36) + " and retry.", "Set it to [REDACTED:github-token] and retry."},
		{"secret inside code is gone with it", "```\nAKIAABCDEFGHIJKLMNOP\n```", "[code omitted: 1 line]"},
	}
	for _, c := range cases {
		if got := Response(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestResponseTruncates(t *testing.T) {
	got := Response(strings.Repeat("word ", 3000))
	if n := len([]rune(got)); n != maxResponse+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("got %d runes ending %q, want %d and an ellipsis", n, got[len(got)-5:], maxResponse+1)
	}
	if got := Response(strings.Repeat("ä", maxResponse+10)); !strings.HasSuffix(got, "ä…") {
		t.Errorf("cut inside a rune: %q", got[len(got)-6:])
	}
}
