package jsonedit

import (
	"strings"
	"testing"
)

func roundTrip(t *testing.T, src string, edit func(doc map[string]any), keyOrder ...string) string {
	t.Helper()
	v, layout, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(v.(map[string]any))
	}
	out, err := Marshal(v, layout, keyOrder)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// An unedited document is written back exactly, however it was formatted.
func TestUneditedIsIdentical(t *testing.T) {
	for name, src := range map[string]string{
		"two spaces":    "{\n  \"b\": 1,\n  \"a\": [\"x\", \"y\"]\n}\n",
		"tabs":          "{\n\t\"z\": {\"k\": true},\n\t\"a\": null\n}\n",
		"compact":       `{"b":1,"a":{"c":[1,2,{"d":"e"}]}}`,
		"escapes":       "{\"cmd\": \"a \\u0026\\u0026 b\", \"u\": \"\\u00e9\", \"n\": 1.50, \"e\": 1e3}\n",
		"no newline":    "{\n    \"x\": []\n}",
		"empty":         "{}",
		"crlf and odds": "{\r\n  \"a\" :  [ ] ,\r\n  \"b\":{ }\r\n}\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := roundTrip(t, src, nil); got != src {
				t.Errorf("got\n%s\nwant\n%s", got, src)
			}
		})
	}
}

// Editing one member rewrites only its enclosing containers: other members
// keep their order and formatting, and new keys come after existing ones.
func TestEditKeepsOrderAndFormatting(t *testing.T) {
	src := `{
    "permissions": {"allow": ["Bash(git*)", "Read"]},
    "hooks": {
        "PreToolUse": [
            {"matcher": "Bash", "hooks": [{"type": "command", "command": "a && b"}]}
        ]
    },
    "env": {"FOO": "bar"}
}
`
	got := roundTrip(t, src, func(doc map[string]any) {
		hooks := doc["hooks"].(map[string]any)
		hooks["Stop"] = []any{map[string]any{"hooks": []any{map[string]any{"command": "x && y", "type": "command", "timeout": 30}}}}
		hooks["SessionStart"] = []any{}
	}, "type", "command", "timeout", "SessionStart", "Stop")
	want := `{
    "permissions": {"allow": ["Bash(git*)", "Read"]},
    "hooks": {
        "PreToolUse": [
            {"matcher": "Bash", "hooks": [{"type": "command", "command": "a && b"}]}
        ],
        "SessionStart": [],
        "Stop": [
            {
                "hooks": [
                    {
                        "type": "command",
                        "command": "x && y",
                        "timeout": 30
                    }
                ]
            }
        ]
    },
    "env": {"FOO": "bar"}
}
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// Appending to an array keeps its existing elements as they were; removing
// one keeps the others, even though their indexes shift.
func TestArrayEdits(t *testing.T) {
	src := "{\n  \"a\": [\n    {\"k\": 1},\n    {\"k\": 2},\n    {\"z\": 3, \"k\": 3}\n  ]\n}\n"
	got := roundTrip(t, src, func(doc map[string]any) {
		arr := doc["a"].([]any)
		doc["a"] = append(arr[1:], "new")
	})
	want := "{\n  \"a\": [\n    {\"k\": 2},\n    {\"z\": 3, \"k\": 3},\n    \"new\"\n  ]\n}\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	// An element edited in place keeps its key order.
	got = roundTrip(t, src, func(doc map[string]any) {
		doc["a"].([]any)[2].(map[string]any)["k"] = 4
	})
	if !strings.Contains(got, "{\n      \"z\": 3,\n      \"k\": 4\n    }") {
		t.Errorf("edited element lost its key order:\n%s", got)
	}
}

// Without a layout (a new file), output is indented with two spaces and
// keys are ordered by keyOrder, then alphabetically.
func TestNewDocument(t *testing.T) {
	out, err := Marshal(map[string]any{"b": "<&>", "a": 1, "z": true}, nil, []string{"z"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"z\": true,\n  \"a\": 1,\n  \"b\": \"<&>\"\n}\n"
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// Edits to the parsed value don't leak into the layout it's compared with.
func TestValueIndependentOfLayout(t *testing.T) {
	src := `{"a": {"b": 1}}`
	got := roundTrip(t, src, func(doc map[string]any) {
		doc["a"].(map[string]any)["b"] = 2
	})
	if want := "{\n  \"a\": {\n    \"b\": 2\n  }\n}"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{`{"a": }`, `{"a": 1} {}`, `[1,`} {
		if _, _, err := Parse([]byte(src)); err == nil {
			t.Errorf("Parse(%q): want an error", src)
		}
	}
}
