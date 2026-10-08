package snapshot

import "testing"

func TestReadOnly(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{"sed -n '140,320p' internal/snapshot/snapshot.go", true},
		{"sed -n 1,80p a.go b.go", true},
		{"grep -rn foo cmd internal | grep -v _test | head -20", true},
		{"cd /repo && git log --oneline -5", true},
		{"git -C /repo --no-pager diff HEAD~1 -- a.go", true},
		{"git status --porcelain | wc -l", true},
		{"S=/tmp/x; jq '.sessions' $S/a.json", true},
		{"ls -la ~/Library/Application\\ Support/swarmia 2>/dev/null", true},
		{"cat a.txt 2>&1 | tail -5", true},
		{"gh run view 123 --log-failed | tail -50", true},
		{"gh api repos/o/r/pulls", true},
		{"find . -name '*.go' | wc -l", true},
		{"sort -u a.txt | uniq -c", true},
		{"# look around\nls\nwc -l a.go", true},
		{"FOO=1 grep x a.go", true},

		{"go test ./...", false},
		{"python3 - <<'EOF'\nprint(1)\nEOF", false},
		{"cat > a.go <<EOF\npackage a\nEOF", false},
		{"grep x a.go > out.txt", false},
		{"echo hi >> notes.md", false},
		{"sed -i 's/a/b/' a.go", false},
		{"sed -n 's/a/b/w out' a.go", false},
		{"find . -name '*.orig' -delete", false},
		{"find . -exec rm {} ;", false},
		{"sort -o out.txt in.txt", false},
		{"uniq in.txt out.txt", false},
		{"awk '{system(\"rm x\")}' a.txt", false},
		{"git checkout main", false},
		{"git commit -m x", false},
		{"git diff --output=patch.diff", false},
		{"gh pr checkout 12", false},
		{"gh run download 123", false},
		{"cat $(ls)", false},
		{"cat `ls`", false},
		{"ls &", false},
		{"ls | xargs rm", false},
		{"ls | tee out.txt", false},
		{"grep -n 'func a\\|func b' a.go | head", true},
		{"grep -n \"a|b\" a.go 2> /dev/null", true},
		{"wc -l < a.txt", true},
		{"git log --format='%h > %s' -3", true}, // > is quoted
		{"echo done >&2", true},
		{"go version >/dev/null 2>&1 || echo missing", false}, // go isn't known
		{"grep x a.go >/dev/nullx", false},
		{"cat <(ls)", false},
		{"echo \"$(rm x)\"", false},
		{"(cd x && rm y)", false},
		{"awk '{print > \"out\"}' a.txt", false},
		{"grep 'unterminated", false},
		{"./gen.sh", false},
		{"make", false},
		{"", false},
	} {
		if got := ReadOnly(tc.command); got != tc.want {
			t.Errorf("ReadOnly(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}
