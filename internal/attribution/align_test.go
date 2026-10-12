package attribution

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// added builds a hunk of lines the agent wrote; ctx marks context lines with a
// leading "= " (stripped).
func hunk(lines ...string) Hunk {
	var h Hunk
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "= "); ok {
			h.Lines = append(h.Lines, HunkLine{Text: rest})
		} else {
			h.Lines = append(h.Lines, HunkLine{Text: l, Added: true})
		}
	}
	return h
}

func file(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// classes renders a match as "line:class" strings for compact comparison.
func classes(m Match) []string {
	var out []string
	for _, l := range m.Lines {
		out = append(out, fmt.Sprintf("%d:%s", l.Line, l.Class))
	}
	return out
}

// checkFrom compares, per committed line, the hunk lines it came from.
func checkFrom(t *testing.T, m Match, want map[int][]int) {
	t.Helper()
	got := map[int][]int{}
	for _, l := range m.Lines {
		got[l.Line] = l.From
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("from = %v, want %v", got, want)
	}
}

func check(t *testing.T, m Match, want []string, survived int) {
	t.Helper()
	if got := classes(m); !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	if m.Survived != survived {
		t.Errorf("survived = %d, want %d (of %d)", m.Survived, survived, m.AddedLines)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		`	foo(a,  b);`:          `foo(a,b)`,
		`foo( a, b, )`:          `foo(a,b)`,
		`x = {"k": [1, 2,],}`:   `x={'k':[1,2]}`,
		"const s = `hi`;":       `consts='hi'`,
		`  "key": "value",    `: `'key':'value'`,
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	for s, want := range map[string]bool{"}": true, "});": true, "fi": true, "end": false, "x := 1": true, "return nil": false} {
		if got := isTrivial(normalize(s)); got != want {
			t.Errorf("isTrivial(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestMatchExactCopyAmongOtherCode(t *testing.T) {
	h := hunk(
		"func add(a, b int) int {",
		"	return a + b",
		"}",
	)
	f := file(
		"package calc",
		"",
		"func sub(a, b int) int {",
		"	return a - b",
		"}",
		"",
		"func add(a, b int) int {",
		"	return a + b",
		"}",
	)
	check(t, MatchHunk(h, f), []string{"7:exact", "8:exact", "9:exact"}, 3)
}

// Reindenting (spaces → tabs) and gofmt-style alignment are formatting only.
func TestMatchFormatterReindentIsNormalized(t *testing.T) {
	h := hunk(
		"type config struct {",
		"    name string // display name",
		"    verbose bool // chatty output",
		"}",
	)
	f := file(
		"type config struct {",
		"	name    string // display name",
		"	verbose bool   // chatty output",
		"}",
	)
	check(t, MatchHunk(h, f), []string{"1:exact", "2:normalized", "3:normalized", "4:exact"}, 4)
}

// prettier wraps a long line into several and changes quotes and trailing
// commas; the wrapped lines all derive from the agent's one line.
func TestMatchLineWrappedByFormatter(t *testing.T) {
	h := hunk(
		`const result = await fetchUser(userId, { includeTeams: true, includeRoles: true });`,
		`console.log("done");`,
	)
	f := file(
		`const result = await fetchUser(userId, {`,
		`  includeTeams: true,`,
		`  includeRoles: true,`,
		`});`,
		`console.log('done');`,
	)
	m := MatchHunk(h, f)
	check(t, m, []string{"1:normalized", "2:normalized", "3:normalized", "4:normalized", "5:normalized"}, 2)
	checkFrom(t, m, map[int][]int{1: {0}, 2: {0}, 3: {0}, 4: {0}, 5: {1}})
}

// The reverse: a formatter joins lines the agent wrote separately.
func TestMatchLinesJoinedByFormatter(t *testing.T) {
	h := hunk(
		"items := []string{",
		`	"alpha",`,
		`	"beta",`,
		"}",
		"sort.Strings(items)",
	)
	f := file(
		`items := []string{"alpha", "beta"}`,
		"sort.Strings(items)",
	)
	m := MatchHunk(h, f)
	check(t, m, []string{"1:normalized", "2:exact"}, 5)
	checkFrom(t, m, map[int][]int{1: {0, 1, 2, 3}, 2: {4}})
}

// A formatter sorts the agent's two imports: both survived unchanged, though an
// order-preserving alignment would pair each with the other as `modified`.
func TestMatchLinesReorderedByFormatter(t *testing.T) {
	h := hunk(
		"= import { CreateOrganizationFactory } from 'src/factory/Organization.js'",
		"import { randomName } from 'src/lib/testing/random.js'",
		"import { setMockAuditedOperationContext } from 'src/lib/testing/auditLogTestHelpers.js'",
		"= ",
		"= describe('findExistingOrganization', () => {",
	)
	f := file(
		"import { CreateOrganizationFactory } from 'src/factory/Organization.js'",
		"import { setMockAuditedOperationContext } from 'src/lib/testing/auditLogTestHelpers.js'",
		"import { randomName } from 'src/lib/testing/random.js'",
		"",
		"describe('findExistingOrganization', () => {",
	)
	m := MatchHunk(h, f)
	check(t, m, []string{"2:exact", "3:exact"}, 2)
	checkFrom(t, m, map[int][]int{2: {2}, 3: {1}})
}

// From a real session: oxfmt sorted the agent's imports and wrapped the long
// one, moving an import that was already there (context in the hunk) in among
// them. That line isn't the agent's, so it's not attributed at all.
func TestMatchPreexistingLineMovedAmongAgentLines(t *testing.T) {
	h := hunk(
		"=   SwarmiaIssueStatus,",
		"= } from 'src/graphql/generated/types.js'",
		"= import { simulateMaterialization } from 'src/lib/testing/materializationHelpers.js'",
		"import { createJiraIssueStatusMappings, swarmiaStatusToJiraValue } from 'src/lib/testing/jiraTestHelpers.js'",
		"import { createTestOnPremiseGithubInstallation } from 'src/lib/testing/githubInstallationTestHelpers.js'",
		"import { randomName } from 'src/lib/testing/random.js'",
		"= ",
		"= describe('JiraIssueChangelogItem', () => {",
		"=   let organizationId: string",
	)
	f := file(
		"  SwarmiaIssueStatus,",
		"} from 'src/graphql/generated/types.js'",
		"import { createTestOnPremiseGithubInstallation } from 'src/lib/testing/githubInstallationTestHelpers.js'",
		"import {",
		"  createJiraIssueStatusMappings,",
		"  swarmiaStatusToJiraValue,",
		"} from 'src/lib/testing/jiraTestHelpers.js'",
		"import { simulateMaterialization } from 'src/lib/testing/materializationHelpers.js'",
		"import { randomName } from 'src/lib/testing/random.js'",
		"",
		"describe('JiraIssueChangelogItem', () => {",
		"  let organizationId: string",
	)
	m := MatchHunk(h, f)
	check(t, m, []string{"3:exact", "4:normalized", "5:normalized", "6:normalized", "7:normalized", "9:exact"}, 3)
}

// A human edits one of the agent's lines, inserts a line of their own, and
// deletes another of the agent's lines.
func TestMatchHumanEdits(t *testing.T) {
	h := hunk(
		"func greet(name string) string {",
		`	prefix := "Hello"`,
		`	log.Printf("greeting %s", name)`,
		`	return prefix + ", " + name`,
		"}",
	)
	f := file(
		"func greet(name string) string {",
		`	prefix := "Hello there"`,        // edited
		`	name = strings.TrimSpace(name)`, // human-only
		`	return prefix + ", " + name`,
		"}",
	)
	check(t, MatchHunk(h, f), []string{"1:exact", "2:modified", "4:exact", "5:exact"}, 3)
}

// Context lines locate a one-line change but are never attributed.
func TestMatchContextIsNotAttributed(t *testing.T) {
	h := hunk(
		"= func handler(w http.ResponseWriter, r *http.Request) {",
		"= 	ctx := r.Context()",
		"	timeout := 30 * time.Second",
		"= 	user := currentUser(ctx)",
	)
	f := file(
		"func handler(w http.ResponseWriter, r *http.Request) {",
		"	ctx := r.Context()",
		"	timeout := 30 * time.Second",
		"	user := currentUser(ctx)",
	)
	m := MatchHunk(h, f)
	check(t, m, []string{"3:exact"}, 1)
	checkFrom(t, m, map[int][]int{3: {2}}) // index in the hunk, context included
}

// A human then tweaks that single line: context pins it, so it's modified.
func TestMatchTweakedOneLineEditIsModified(t *testing.T) {
	h := hunk(
		"= func handler(w http.ResponseWriter, r *http.Request) {",
		"= 	ctx := r.Context()",
		"	timeout := 30 * time.Second",
		"= 	user := currentUser(ctx)",
	)
	f := file(
		"func handler(w http.ResponseWriter, r *http.Request) {",
		"	ctx := r.Context()",
		"	timeout := 45 * time.Second",
		"	user := currentUser(ctx)",
	)
	check(t, MatchHunk(h, f), []string{"3:modified"}, 0)
}

// If the agent's lines were deleted, surviving context alone attributes nothing.
func TestMatchDeletedAddedLinesYieldNothing(t *testing.T) {
	h := hunk(
		"= func handler(w http.ResponseWriter, r *http.Request) {",
		"= 	ctx := r.Context()",
		"	metrics.Inc(\"handler_calls\")",
		"= 	user := currentUser(ctx)",
	)
	f := file(
		"func handler(w http.ResponseWriter, r *http.Request) {",
		"	ctx := r.Context()",
		"	user := currentUser(ctx)",
	)
	check(t, MatchHunk(h, f), nil, 0)
}

// Lines like `}` can't anchor an alignment on their own.
func TestMatchTrivialOnlyHunkYieldsNothing(t *testing.T) {
	check(t, MatchHunk(hunk("}", "})", ")"), file("}", "})", ")")), nil, 0)
}

func TestMatchUnrelatedFileYieldsNothing(t *testing.T) {
	h := hunk(
		"func parseConfig(path string) (*Config, error) {",
		"	data, err := os.ReadFile(path)",
		"	if err != nil {",
		"		return nil, err",
		"	}",
	)
	f := file(
		"import { render } from 'react-dom'",
		"render(<App />, document.getElementById('root'))",
	)
	check(t, MatchHunk(h, f), nil, 0)
}

// Two functions the agent wrote together, reordered by a human with code in
// between: both pieces are found.
func TestMatchMovedBlocks(t *testing.T) {
	h := hunk(
		"func first() string {",
		`	return "first value"`,
		"}",
		"",
		"func second() string {",
		`	return "second value"`,
		"}",
	)
	f := file(
		"func second() string {",
		`	return "second value"`,
		"}",
		"",
		"func unrelated() int {",
		"	return 42",
		"}",
		"",
		"func first() string {",
		`	return "first value"`,
		"}",
	)
	check(t, MatchHunk(h, f), []string{"1:exact", "2:exact", "3:exact", "9:exact", "10:exact", "11:exact"}, 6)
}

// Very large inputs are narrowed to a window around the hunk instead of
// building a huge matrix; the result is the same.
func TestMatchLargeFileIsNarrowed(t *testing.T) {
	var lines []string
	for i := range 20_000 {
		lines = append(lines, fmt.Sprintf("var filler%d = computeFiller(%d)", i, i))
	}
	at := len(lines)
	lines = append(lines, "func target() error {", "	return errors.New(\"target reached\")", "}")
	var hl []string
	for i := range 400 {
		hl = append(hl, fmt.Sprintf("var filler%d = computeFiller(%d)", 19_600+i, 19_600+i))
	}
	hl = append(hl, "func target() error {", "	return errors.New(\"target reached\")", "}")

	m := MatchHunk(hunk(hl...), file(lines...))
	if m.Survived != len(hl) || m.Lines[len(m.Lines)-1].Line != at+3 {
		t.Errorf("survived %d/%d, last line %v; want all, ending at %d", m.Survived, len(hl), m.Lines[len(m.Lines)-1], at+3)
	}
}
