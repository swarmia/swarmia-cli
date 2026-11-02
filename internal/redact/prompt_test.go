package redact

import (
	"strings"
	"testing"
)

func TestPrompt(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no paste", "Add retries to the webhook sender.", "Add retries to the webhook sender."},
		{"paste around typed words",
			"<pasted_content id=\"a1\">\nline one\nline two\nline three\n</pasted_content id=\"a1\">\n\nWhy did this fail?",
			"[pasted content omitted: 3 lines]\n\nWhy did this fail?"},
		{"two pastes", "Compare <pasted_content id=\"x\">a</pasted_content id=\"x\"> with <pasted_content id=\"y\">b\nc</pasted_content id=\"y\">.",
			"Compare [pasted content omitted: 1 line] with [pasted content omitted: 2 lines]."},
		{"escaped nested paste goes with its parent",
			"<pasted_content id=\"out\">\n<\\pasted_content id=\"in\">\nDeploy failed\nerror: push rejected\n<\\/pasted_content id=\"in\">\n</pasted_content id=\"out\">\n\nFix it",
			"[pasted content omitted: 4 lines]\n\nFix it"},
		{"closing tag without id", "<pasted_content id=\"z\">x\ny</pasted_content> ok", "[pasted content omitted: 2 lines] ok"},
		{"no id at all", "<pasted_content>secret stuff</pasted_content> ok", "[pasted content omitted: 1 line] ok"},
		{"unclosed paste runs to the end", "Look: <pasted_content id=\"q\">\na\nb", "Look: [pasted content omitted: 2 lines]"},
		{"empty paste", "<pasted_content id=\"e\"></pasted_content id=\"e\">hi", "[pasted content omitted: 0 lines]hi"},
		{"secret in paste is gone with it", "<pasted_content id=\"s\">AKIAABCDEFGHIJKLMNOP</pasted_content id=\"s\">", "[pasted content omitted: 1 line]"},
		{"secret outside paste is redacted", "Use ghp_" + strings.Repeat("a", 36) + " <pasted_content id=\"s\">x</pasted_content id=\"s\">",
			"Use [REDACTED:github-token] [pasted content omitted: 1 line]"},
	}
	for _, c := range cases {
		if got := Prompt(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
