# Golden attribution cases

Each directory holds one agent edit and the file it later ended up in:

- `event.json` — a Claude Code `PostToolUse` payload, trimmed to the tool input
  and the response's `originalFile`. Paths are rewritten under `/repo`.
- `committed` — the file as committed.
- `want.txt` — the expected attribution, rendered as an annotated listing.

The cases are real edits recorded while this repository was built (July 2026),
matched against the commits they landed in — except `write-before-formatter`,
which takes a real `Write` of `cmd/paths.go` and re-indents it with spaces to
stand in for code a formatter (gofmt) rewrote before commit.

| Case | What it covers |
|---|---|
| `write-update-tabs` | `Write` over an existing tab-indented file; exact despite Claude Code's `structuredPatch` rendering tabs as spaces |
| `edit-go` | `Edit` located via its surrounding context |
| `edit-makefile-partly-rewritten` | `Edit` whose lines were partly changed by a later edit |
| `edit-line-later-rewritten` | a one-line edit whose line was rewritten afterwards → `modified` |
| `write-create-later-edited` | new file; one line differs only by a trailing comma → `normalized` |
| `write-before-formatter` | every indented line reformatted → `normalized` |

To accept intentional changes: `go test ./internal/attribution -run Golden -update`,
then review the `want.txt` diffs before committing.
