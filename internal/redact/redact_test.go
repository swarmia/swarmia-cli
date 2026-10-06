package redact

import (
	"strings"
	"testing"
)

func TestRedactsSecrets(t *testing.T) {
	for _, tc := range []struct{ in, kind string }{
		{"use ghp_" + strings.Repeat("a1B2", 9) + " for the API", "github-token"},
		{"token github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz", "github-token"},
		{"export GITLAB=glpat-abcdefghij0123456789", "gitlab-token"},
		{"xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx", "slack-token"},
		{"post to https://hooks.slack.com/services/T000/B000/XXXXXXXX", "slack-webhook"},
		{"aws key AKIAIOSFODNN7EXAMPL3", "aws-access-key"},
		{"AIzaSyA1234567890abcdefghijklmnopqrstuv", "google-api-key"},
		{"sk_live_51H8abcdefghijklmnop", "stripe-key"},
		{"ANTHROPIC_KEY sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "anthropic-key"},
		{"key sk-proj-abcdefghijklmnopqrstuvwx", "openai-key"},
		{"npm_abcdefghijklmnopqrstuvwxyz0123456789", "npm-token"},
		{"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "jwt"},
		{"connect to postgres://admin:hunter2pass@db.internal:5432/app", "password"},
		{"Server=db;User Id=sa;Password=Sup3rS3cret;", "password"},
		{"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "secret"},
		{`{"api_key": "abc123def456"}`, "secret"},
		{"db_password: correcthorse", "secret"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "private-key"},
		{"random value Xk9mQ2pL7vR4tN8wZ1cB6hJ3fG5dS0aE", "high-entropy"},
	} {
		got := Text(tc.in)
		if !strings.Contains(got, "[REDACTED:"+tc.kind+"]") {
			t.Errorf("Text(%q) = %q, want a %s redaction", tc.in, got, tc.kind)
		}
	}
}

// Ordinary prompts and commands must come through untouched.
func TestLeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		"please fix the failing test in report_test.go and run go test ./...",
		"git show 59e69c61608f8b15237770925b6daafd7496f7eb:internal/attribution/align.go",
		"cd /private/tmp/claude-501/-Users-mutru-swarmia-src-swarmia-cli/7999d991-42b2-47e9-932e-224855021364/scratchpad",
		"func TestBuildAttributesSessionToCommits(t *testing.T) {",
		"session 7999d991-42b2-47e9-932e-224855021364 ended",
		"token := os.Getenv(\"SWARMIA_TOKEN\")",
		"export GITHUB_TOKEN=$GITHUB_TOKEN",
		"curl -H \"Authorization: Bearer ${API_TOKEN}\" https://api.example.com",
		"the token budget is fine, keep going",
		"npm install --save-dev @swarmia/cli",
		"github.com/swarmia/swarmia-cli/internal/attribution",
		"maxTokensPerRequestLimitExceeded2",
		"jq 'select(.event.tool_use_id==\"toolu_01MoKWsX6PryLphu2XpUjMKB\")'",
	} {
		if got := Text(in); got != in {
			t.Errorf("Text(%q) = %q, want unchanged", in, got)
		}
	}
}
