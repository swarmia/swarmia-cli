package redact

import (
	"fmt"
	"regexp"
	"strings"
)

// Coding agents wrap text the developer pasted into a prompt in
// <pasted_content id="…"> … </pasted_content id="…"> tags. What gets pasted is
// mostly logs, stack traces, code and data: the material least safe to share
// and least telling about what the developer asked for. Prompt keeps the words
// the developer typed and drops the paste.

// pasteOpen matches an opening paste tag and captures its id, if any. Tags
// nested inside a paste arrive escaped (<\pasted_content …>), so they never
// match and go with the paste that holds them.
var pasteOpen = regexp.MustCompile(`<pasted_content(?:\s+id="([^"]*)")?\s*>`)

// Prompt returns a developer's prompt with pasted content replaced by a line
// saying how much there was, then redacted (see Text).
func Prompt(text string) string {
	return Text(stripPastes(text))
}

// stripPastes replaces each paste, tags included, with a placeholder. A paste
// is closed by the closing tag with the same id (or one without an id); one
// left open runs to the end of the text.
func stripPastes(text string) string {
	var b strings.Builder
	for {
		m := pasteOpen.FindStringSubmatchIndex(text)
		if m == nil {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:m[0]])
		rest := text[m[1]:]
		id := ""
		if m[2] >= 0 {
			id = text[m[2]:m[3]]
		}
		body, after := rest, ""
		if end, n := pasteClose(rest, id); end >= 0 {
			body, after = rest[:end], rest[end+n:]
		}
		lines := 0
		if t := strings.Trim(body, "\r\n"); t != "" {
			lines = strings.Count(t, "\n") + 1
		}
		fmt.Fprintf(&b, "[pasted content omitted: %d %s]", lines, plural(lines, "line"))
		text = after
	}
}

// pasteClose finds the tag closing a paste with id in s, returning its
// position and length, or -1.
func pasteClose(s, id string) (int, int) {
	tags := []string{"</pasted_content>"}
	if id != "" {
		tags = append([]string{`</pasted_content id="` + id + `">`}, tags...)
	}
	for _, tag := range tags {
		if i := strings.Index(s, tag); i >= 0 {
			return i, len(tag)
		}
	}
	return -1, 0
}
