package redact

import (
	"fmt"
	"regexp"
	"strings"
)

// Shell commands can carry source code: agents write files with heredocs and
// run inline scripts. Command keeps a command's shape — the program, flags,
// paths, pipes — and drops those bodies, so `go test ./...` or `npm run build`
// survive for analysis but no code leaves the machine.

// heredocStart matches a heredoc operator and captures its delimiter.
var heredocStart = regexp.MustCompile(`<<(-?)\s*(?:'([^']+)'|"([^"]+)"|\\?([A-Za-z_][A-Za-z0-9_]*))`)

// inlineScript matches an interpreter given its program as an argument, e.g.
// python3 -c '...', node -e "...", and captures the quoted program.
var inlineScript = regexp.MustCompile(`\b((?:python[0-9.]*|node|deno|bun|ruby|perl|php|osascript)\s+(?:-[A-Za-z]*[ce][A-Za-z]*|--eval)\s+)('(?:[^']*)'|"(?:[^"\\]|\\.)*")`)

// quoted matches single- or double-quoted strings.
var quoted = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"`)

const (
	// maxQuoted is the longest quoted argument kept: longer ones are content
	// (echo'd files, printf'd code), not names or messages.
	maxQuoted = 120
	// maxCommand caps a command's length after stripping.
	maxCommand = 1000
)

// Command returns cmd with heredoc bodies, inline scripts and long quoted
// strings replaced by short placeholders, then redacted (see Text).
func Command(cmd string) string {
	cmd = stripHeredocs(cmd)
	cmd = inlineScript.ReplaceAllStringFunc(cmd, func(m string) string {
		sub := inlineScript.FindStringSubmatch(m)
		return sub[1] + "'[script omitted]'"
	})
	cmd = quoted.ReplaceAllStringFunc(cmd, func(q string) string {
		if len(q) <= maxQuoted {
			return q
		}
		return fmt.Sprintf("%c[%d chars omitted]%c", q[0], len(q)-2, q[0])
	})
	cmd = Text(cmd)
	if len(cmd) > maxCommand {
		cmd = cmd[:maxCommand] + "…"
	}
	return cmd
}

// stripHeredocs replaces each heredoc body with a line count, keeping the
// operator line and the closing delimiter.
func stripHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		var pending []struct {
			delim     string
			stripTabs bool
		}
		for _, m := range heredocStart.FindAllStringSubmatch(lines[i], -1) {
			delim := m[2] + m[3] + m[4]
			pending = append(pending, struct {
				delim     string
				stripTabs bool
			}{delim, m[1] == "-"})
		}
		for _, h := range pending {
			body := 0
			for i+1 < len(lines) {
				i++
				l := lines[i]
				if h.stripTabs {
					l = strings.TrimLeft(l, "\t")
				}
				if l == h.delim {
					break
				}
				body++
			}
			out = append(out, fmt.Sprintf("[%d lines omitted]", body), h.delim)
		}
	}
	return strings.Join(out, "\n")
}
