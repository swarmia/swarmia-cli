package redact

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// An agent's replies routinely quote code: snippets, diffs, lines of files it
// read. Response keeps the prose — what the agent says it did, found or
// suggests — and drops the code, so the reply can be shared without the source
// it talks about.

// fence matches a Markdown code fence line (``` or ~~~, three or more,
// indented or not, e.g. inside a list item) and captures its marker and info
// string (the language).
var fence = regexp.MustCompile("^\\s*(`{3,}|~{3,})\\s*([^`\\s]*)")

// inlineCode matches a Markdown code span of one or more backticks.
var inlineCode = regexp.MustCompile("(`+)[^`]+?(`+)")

const (
	// maxInlineCode is the longest code span kept: shorter ones are names
	// (a file, a function, a command), longer ones are code.
	maxInlineCode = maxQuoted
	// maxResponse caps a reply's length after stripping.
	maxResponse = 8000
)

// Response returns an agent's reply with fenced code blocks and long code
// spans replaced by short placeholders, then redacted (see Text), and capped
// in length.
func Response(text string) string {
	text = stripFences(text)
	text = inlineCode.ReplaceAllStringFunc(text, func(m string) string {
		sub := inlineCode.FindStringSubmatch(m)
		if sub[1] != sub[2] || utf8.RuneCountInString(m) <= maxInlineCode {
			return m
		}
		inner := utf8.RuneCountInString(m) - len(sub[1]) - len(sub[2])
		return fmt.Sprintf("`[%d chars omitted]`", inner)
	})
	text = Text(text)
	return truncate(text, maxResponse)
}

// stripFences replaces each fenced code block, fences included, with a line
// saying what was there. A block left open runs to the end of the text, as
// Markdown renders it.
func stripFences(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		m := fence.FindStringSubmatch(lines[i])
		if m == nil {
			out = append(out, lines[i])
			continue
		}
		marker, lang := m[1], m[2]
		indent := lines[i][:strings.Index(lines[i], marker)]
		n := 0
		for i++; i < len(lines); i++ {
			if closing(lines[i], marker) {
				break
			}
			n++
		}
		what := "code"
		if lang != "" && len(lang) <= 20 {
			what = lang + " code"
		}
		out = append(out, fmt.Sprintf("%s[%s omitted: %d %s]", indent, what, n, plural(n, "line")))
	}
	return strings.Join(out, "\n")
}

// closing reports whether line closes a block opened with marker: the same
// character, at least as many, and nothing else on the line.
func closing(line, marker string) bool {
	t := strings.TrimSpace(line)
	return len(t) >= len(marker) && strings.Trim(t, marker[:1]) == ""
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// truncate cuts s to at most max runes, marking the cut.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}
