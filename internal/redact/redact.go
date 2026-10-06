// Package redact removes secrets from free text (prompts, shell commands)
// before it leaves the machine. It layers several detectors, each replacing
// what it finds with a [REDACTED:<kind>] marker:
//
//  1. private key blocks;
//  2. well-known provider token formats (Swarmia, GitHub, GitLab, Slack, AWS,
//     Google, Stripe, OpenAI, Anthropic, npm, SendGrid, JWTs);
//  3. passwords in URLs and connection strings;
//  4. values assigned to secret-looking names (password=, api_key: …);
//  5. long random-looking strings (high Shannon entropy).
//
// It errs towards redacting: a false positive costs a little context, a false
// negative leaks a credential.
package redact

import (
	"math"
	"regexp"
	"strings"
	"unicode"
)

type pattern struct {
	kind string
	re   *regexp.Regexp
}

var privateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----.*?(-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----|$)`)

// providerTokens are formats specific enough to redact on sight.
var providerTokens = []pattern{
	{"swarmia-token", regexp.MustCompile(`\bswm_cli_[0-9a-f]{32,}`)},
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,255}\b`)},
	{"github-token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,255}\b`)},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"slack-webhook", regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"stripe-key", regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[0-9A-Za-z]{16,}`)},
	{"anthropic-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"openai-key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`)},
	{"npm-token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{"sendgrid-key", regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
}

// urlPassword matches scheme://user:password@ and captures the password.
var urlPassword = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s:/@]*:)([^\s@/]+)(@)`)

// userFlagPassword matches `-u user:password` / `--user user:password` (curl,
// httpie and friends) and captures the password.
var userFlagPassword = regexp.MustCompile(`((?:^|\s)(?:-u|--user)[\s=]+["']?[^\s:"']+:)([^\s"'@]+)`)

// connPassword matches password fields in key=value connection strings
// (JDBC, ODBC, libpq keyword DSNs).
var connPassword = regexp.MustCompile(`(?i)\b((?:password|pwd)\s*=\s*)([^\s;'"]+)`)

// secretAssignment matches a secret-looking name assigned a value, in shell,
// env, YAML, JSON or code: NAME=value, name: value, "name": "value".
var secretAssignment = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|apikey|access[_-]?key|auth[_-]?key|credentials?|private[_-]?key)[A-Za-z0-9_.-]*["']?\s*(?::=|[:=])\s*["']?)([^\s"'` + "`" + `,;(){}<>]{6,})`)

// candidate matches tokens worth an entropy check.
var candidate = regexp.MustCompile(`[A-Za-z0-9+_=-]{24,}`)

// minEntropy is the Shannon entropy (bits per character) above which a long
// mixed token looks random. Identifiers, words and UUIDs stay below it; keys
// generated from a 62+ character alphabet sit well above.
const minEntropy = 4.2

// Text returns s with secrets replaced by [REDACTED:<kind>] markers.
func Text(s string) string {
	s = privateKey.ReplaceAllString(s, marker("private-key"))
	for _, p := range providerTokens {
		s = p.re.ReplaceAllString(s, marker(p.kind))
	}
	s = urlPassword.ReplaceAllString(s, "${1}"+marker("password")+"${3}")
	s = connPassword.ReplaceAllString(s, "${1}"+marker("password"))
	s = userFlagPassword.ReplaceAllString(s, "${1}"+marker("password"))
	s = replaceSubmatch(secretAssignment, s, 2, func(v, rest string) (string, bool) {
		if isReference(v) || strings.HasPrefix(v, "[REDACTED") || strings.HasPrefix(rest, "(") {
			return "", false // a reference, already redacted, or a function call
		}
		return marker("secret"), true
	})
	s = candidate.ReplaceAllStringFunc(s, func(tok string) string {
		if looksRandom(tok) {
			return marker("high-entropy")
		}
		return tok
	})
	return s
}

func marker(kind string) string { return "[REDACTED:" + kind + "]" }

// isReference reports values that point at a secret rather than being one:
// $VAR, ${VAR}, %VAR%, {{ template }}, or obvious placeholders.
func isReference(v string) bool {
	if strings.HasPrefix(v, "$") || strings.HasPrefix(v, "%") || strings.HasPrefix(v, "{{") {
		return true
	}
	switch strings.ToLower(strings.Trim(v, "*.x")) {
	case "", "redacted", "changeme", "placeholder", "example", "required", "optional", "string", "true", "false", "null", "none", "undefined":
		return true
	}
	return false
}

// idPrefixes mark random-looking identifiers that aren't secrets: Claude tool
// call, message and request ids show up in commands and prompts.
var idPrefixes = []string{"toolu_", "msg_", "req_"}

// looksRandom flags long tokens mixing letters and digits with high entropy.
// Pure hex is left alone: git SHAs are everywhere in commands and prompts, and
// matter for analysis.
func looksRandom(tok string) bool {
	for _, p := range idPrefixes {
		if strings.HasPrefix(tok, p) {
			return false
		}
	}
	var lower, upper, digit bool
	hex := true
	for _, r := range tok {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			hex = false
		}
	}
	if hex || !digit || !(lower || upper) {
		return false
	}
	if !(lower && upper) && len(tok) < 32 {
		return false
	}
	return entropy(tok) >= minEntropy
}

func entropy(s string) float64 {
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len([]rune(s)))
	h := 0.0
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// replaceSubmatch replaces capture group g of each match of re with repl's
// result (when it returns ok), keeping the rest of the match. repl also sees
// the text following the group.
func replaceSubmatch(re *regexp.Regexp, s string, g int, repl func(group, rest string) (string, bool)) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[2*g], m[2*g+1]
		if start < 0 {
			continue
		}
		r, ok := repl(s[start:end], s[end:])
		if !ok {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(r)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}
