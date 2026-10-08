package snapshot

import (
	"regexp"
	"strings"
)

// ReadOnly reports whether a shell command certainly leaves the worktree's
// files alone, so snapshotting around it can be skipped: agents run many
// commands that only read (sed -n, grep, git log, gh run view), and each
// snapshot costs a git status. It is deliberately conservative: anything it
// doesn't recognize — interpreters, build tools, tests, redirections to files,
// command substitution, subshells — is not read-only. A command it wrongly
// called read-only would lose its edits (they'd be unknown, never
// misattributed).
func ReadOnly(command string) bool {
	commands, ok := splitCommands(command)
	if !ok || len(commands) == 0 { // an empty command may be one we couldn't read
		return false
	}
	for _, words := range commands {
		if !readOnlyCommand(words) {
			return false
		}
	}
	return true
}

// splitCommands splits a command line into simple commands (at |, ||, &&, ;
// and newlines), each as its words with quotes and escapes removed. ok is
// false for anything that could run or write beyond those commands: command
// or process substitution, subshells, here-documents, background jobs and
// redirections other than to /dev/null or between file descriptors.
func splitCommands(s string) (commands [][]string, ok bool) {
	var words []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			commands = append(commands, words)
			words = nil
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			if s[i] != '\n' {
				word.WriteByte(s[i])
				inWord = true
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			word.WriteString(s[i+1 : i+1+end])
			inWord = true
			i += end + 1
		case c == '"':
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				switch {
				case s[j] == '`' || s[j] == '$' && j+1 < len(s) && s[j+1] == '(':
					return nil, false
				case s[j] == '\\' && j+1 < len(s):
					j++
				}
				word.WriteByte(s[j])
			}
			if j == len(s) {
				return nil, false
			}
			inWord = true
			i = j
		case c == '`', c == '(', c == ')':
			return nil, false // command substitution or a subshell
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			return nil, false
		case c == '<':
			if i+1 < len(s) && strings.IndexByte("<(>", s[i+1]) >= 0 {
				return nil, false // here-document, process substitution, read-write
			}
			endWord() // input from a file: the file is just read
		case c == '>' || c == '&' && i+1 < len(s) && s[i+1] == '>':
			// Output redirection: only to /dev/null or another descriptor.
			// A descriptor number before it (2>) isn't a word.
			if c == '&' {
				i++
			}
			if inWord && strings.Trim(word.String(), "0123456789") == "" {
				word.Reset()
				inWord = false
			}
			j := i + 1
			if j < len(s) && s[j] == '>' {
				j++ // appending
			}
			if j+1 < len(s) && s[j] == '&' && s[j+1] >= '0' && s[j+1] <= '9' {
				i = j + 1 // >&2
				continue
			}
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			end := j + len("/dev/null")
			if !strings.HasPrefix(s[j:], "/dev/null") || end < len(s) && strings.IndexByte(" \t\n;|&", s[end]) < 0 {
				return nil, false
			}
			i = end - 1
		case c == '&' && i+1 < len(s) && s[i+1] == '&', c == '|' && i+1 < len(s) && s[i+1] == '|':
			endCommand()
			i++
		case c == '|' && i+1 < len(s) && s[i+1] == '&':
			return nil, false
		case c == '|', c == ';', c == '\n':
			endCommand()
		case c == '&':
			return nil, false // a background job may write after the command returns
		case c == '#' && !inWord:
			for i < len(s) && s[i] != '\n' { // a comment
				i++
			}
			endCommand()
		case c == ' ', c == '\t':
			endWord()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCommand()
	return commands, true
}

var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// sedRange is a sed script printing a line range, as in sed -n 40,80p.
var sedRange = regexp.MustCompile(`^[0-9$]+(,[0-9$]+)?p$`)

// readOnlyCommands only read files or print, whatever their arguments.
var readOnlyCommands = map[string]bool{
	"basename": true, "cat": true, "cd": true, "cmp": true, "column": true,
	"comm": true, "cut": true, "date": true, "df": true, "diff": true,
	"dirname": true, "du": true, "echo": true, "egrep": true, "export": true,
	"false": true, "fgrep": true, "file": true, "grep": true, "head": true,
	"jq": true, "ls": true, "md5": true, "md5sum": true, "nl": true,
	"printf": true, "pwd": true, "readlink": true, "realpath": true,
	"set": true, "sha256sum": true, "shasum": true, "sleep": true,
	"stat": true, "tail": true, "test": true, "tr": true, "true": true,
	"type": true, "wc": true, "which": true, "[": true,
}

// readOnlyGit are git subcommands that don't touch the worktree. (commit
// isn't one: its hooks may run formatters.)
var readOnlyGit = map[string]bool{
	"blame": true, "cat-file": true, "describe": true, "diff": true,
	"for-each-ref": true, "grep": true, "log": true, "ls-files": true,
	"ls-tree": true, "merge-base": true, "name-rev": true, "reflog": true,
	"rev-list": true, "rev-parse": true, "shortlog": true, "show": true,
	"show-ref": true, "status": true,
}

// readOnlyGH are gh subcommands that only print.
var readOnlyGH = map[string]bool{"checks": true, "diff": true, "list": true, "status": true, "view": true}

func readOnlyCommand(words []string) bool {
	for len(words) > 0 && envAssignment.MatchString(words[0]) {
		words = words[1:] // FOO=bar, alone or before a command
	}
	if len(words) == 0 {
		return true
	}
	args := words[1:]
	switch name := words[0]; {
	case readOnlyCommands[name]:
		return true
	case name == "rg":
		return !hasArg(args, "--pre")
	case name == "awk": // its program can redirect, pipe and run commands
		for _, a := range args {
			if strings.ContainsAny(a, ">|") || strings.Contains(a, "system") || strings.Contains(a, "inplace") {
				return false
			}
		}
		return true
	case name == "find":
		return !hasArg(args, "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls")
	case name == "sort":
		for _, a := range args {
			if strings.HasPrefix(a, "--output") || !strings.HasPrefix(a, "--") && strings.HasPrefix(a, "-") && strings.Contains(a, "o") {
				return false
			}
		}
		return true
	case name == "uniq": // uniq IN OUT writes OUT
		n := 0
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				n++
			}
		}
		return n <= 1
	case name == "sed":
		if len(args) < 2 || args[0] != "-n" || !sedRange.MatchString(args[1]) {
			return false
		}
		for _, a := range args[2:] {
			if strings.HasPrefix(a, "-") {
				return false
			}
		}
		return true
	case name == "git":
		return readOnlyGitArgs(args)
	case name == "gh":
		return len(args) >= 1 && args[0] == "api" ||
			len(args) >= 2 && readOnlyGH[args[1]]
	}
	return false
}

func readOnlyGitArgs(args []string) bool {
	for len(args) > 0 {
		switch args[0] {
		case "-C", "-c":
			if len(args) < 2 {
				return false
			}
			args = args[2:]
		case "--no-pager", "--no-optional-locks", "-P":
			args = args[1:]
		default:
			if hasArg(args, "--output") {
				return false
			}
			return readOnlyGit[args[0]]
		}
	}
	return false
}

func hasArg(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}
