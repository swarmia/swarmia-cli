package redact

import (
	"strings"
	"testing"
)

func TestCommandStripsCode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{
			"cat > internal/x.go <<'EOF'\npackage x\n\nfunc Secret() {}\nEOF\ngo test ./...",
			"cat > internal/x.go <<'EOF'\n[3 lines omitted]\nEOF\ngo test ./...",
		},
		{
			"python3 - <<EOF\nprint('hi')\nEOF",
			"python3 - <<EOF\n[1 lines omitted]\nEOF",
		},
		{
			"cat <<-END\n\tindented\n\tEND\necho done",
			"cat <<-END\n[1 lines omitted]\nEND\necho done",
		},
		{
			`python3 -c 'import os; print(os.listdir("."))' && node -e "console.log(1)"`,
			`python3 -c '[script omitted]' && node -e '[script omitted]'`,
		},
		{
			`printf '` + strings.Repeat("x", 200) + `' > gen.go`,
			`printf '[200 chars omitted]' > gen.go`,
		},
		{
			`git commit -m "Fix the matcher for wrapped lines" && go test ./internal/... 2>&1 | tail -5`,
			`git commit -m "Fix the matcher for wrapped lines" && go test ./internal/... 2>&1 | tail -5`,
		},
		{
			"curl -u admin:hunter2pass https://x && export API_TOKEN=abcdef123456",
			"curl -u admin:[REDACTED:password] https://x && export API_TOKEN=[REDACTED:secret]",
		},
	} {
		if got := Command(tc.in); got != tc.want {
			t.Errorf("Command(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestCommandCapsLength(t *testing.T) {
	got := Command("echo " + strings.Repeat("word ", 500))
	if len(got) > maxCommand+len("…") {
		t.Errorf("length %d exceeds cap", len(got))
	}
}
