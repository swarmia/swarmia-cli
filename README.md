# swarmia-cli

The Swarmia Agent CLI captures local AI-agent activity so that AI-generated code
can be reliably attributed to the commits it ends up in.

Coding agents like Claude Code, Codex, and Copilot expose activity through
hooks. `swarmia-cli` registers as a hook, records what the agent does into a
local log, and (in future increments) links that activity to your git history.

> **Status:** early. Today the CLI can set up its hooks (`install`), log Claude
> Code hook events locally (`log-hook`), and show which committed lines came
> from agent sessions (`report`). Delivery to Swarmia is on the roadmap; nothing
> is sent anywhere yet.

## Installation

### npm (as a dev dependency)

```sh
npm install --save-dev @swarmia/cli
```

This gives the repo a `swarmia-cli` at `node_modules/.bin/swarmia-cli` — no global
install needed, and `swarmia-cli install` detects it automatically. The package
ships a tiny launcher plus a per-platform binary delivered as an optional
dependency (macOS and Linux, arm64/x64); the right one installs for your host.

### `go install`

```sh
go install github.com/swarmia/swarmia-cli@latest
```

This drops the `swarmia-cli` binary in `$(go env GOPATH)/bin` (usually
`~/go/bin`). Make sure that directory is on your `$PATH`.

### From source

```sh
git clone https://github.com/swarmia/swarmia-cli.git
cd swarmia-cli
make build      # produces ./swarmia-cli with version metadata baked in
```

Requires Go 1.26+. `make install` installs it onto your `$PATH` instead.

## Usage

```sh
swarmia-cli --help
swarmia-cli version
```

### `install`

Sets up the Swarmia hooks in whatever supported coding agents are configured in
the current repository (currently Claude Code — detected by a `.claude`
directory):

```sh
swarmia-cli install                 # add hooks to detected agents
swarmia-cli install --dry-run       # preview without writing
swarmia-cli install --scope user    # just for you, in every project
```

`--scope` picks where the hooks go:

| Scope | File | Who / where |
|---|---|---|
| `project` (default) | `.claude/settings.json` | everyone working in the repo (it's committed) |
| `local` | `.claude/settings.local.json` | just you, in this checkout (gitignored by Claude Code convention) |
| `user` | `~/.claude/settings.json` (or `$CLAUDE_CONFIG_DIR`) | just you, in every project — including new git worktrees, which don't get a checkout's untracked `settings.local.json` |

User-scope hooks run `log-hook --skip-if-project-hooks`, so in a repository
that wires swarmia-cli itself (like this one) events aren't logged twice.

It adds the same wiring this repo uses — `log-hook` on prompts, all tool use, and
session lifecycle. It is **idempotent** (hooks already present are left alone) and
preserves any unrelated settings and hooks. Hooks for the removed `process-logs`
subcommand, written by earlier versions, are cleaned up.

For `local` and `user` scope, hooks invoke the swarmia-cli that ran `install`, by
absolute path — handy for trying a development build in another repository:

```sh
make build && ./swarmia-cli install --dir ../other-repo --scope local
```

For `project` scope the command is auto-detected, in order:

1. a `swarmia-cli` binary at the repo root → `${CLAUDE_PROJECT_DIR}/swarmia-cli`
   (handy during local development)
2. an npm dependency at `node_modules/.bin/swarmia-cli` →
   `${CLAUDE_PROJECT_DIR}/node_modules/.bin/swarmia-cli`
3. otherwise a `swarmia-cli` on your `$PATH` → bare `swarmia-cli`

Override it with `--binary`, and target another directory with `--dir`. After
installing, reload hooks with `/hooks` in Claude Code (or start a fresh session).

### `log-hook`

Reads a Claude Code hook payload from stdin and appends it to a JSONL log, one
event per line, wrapped with a capture timestamp and the project's git position
(worktree root, HEAD commit and branch) at that moment:

```json
{"logged_at":"2026-07-10T19:02:37Z","git":{"root":"/src/app","head":"ec37b5d…","branch":"main"},"event":{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"npm test"}}}
```

`git` is omitted outside a repository; `head` is empty on a branch with no
commits and `branch` is empty when HEAD is detached. The git state is read from
`.git` files directly (no `git` process), so logging stays at a few milliseconds
per event.

It is a **passive** logger: it always exits 0 and writes nothing to stdout, so it
can never block or interfere with the tool call that triggered it.

**Shell commands.** Agents also change files through Bash (`sed`, codegen,
scripts, heredocs), which their edit-tool payloads don't show. So around each
Bash call `log-hook` snapshots the worktree: before the command it records the
dirty files (copying their content into the state directory), and when the
command finishes — `PostToolUse` or `PostToolUseFailure` — it diffs what changed
and stores the result as `changes` on that event's record. Only lines the
command *authored* are kept: lines it merely reformatted (`prettier --write`) or
restored from git (`git stash`, `checkout`, `pull`) are excluded, by requiring a
line's normalized text to occur more often afterwards than in the file before
the command or in HEAD. This costs one `git status` per hook (≈70 ms in a
14k-file repository); files larger than 1 MB and binary files are skipped.
Human edits made while a command runs are indistinguishable from the command's. Concurrent
invocations are serialized with a file lock, so parallel tool calls and
subagents can't corrupt the log.

The log file is chosen in this order:

1. `--file <path>`
2. `$SWARMIA_LOG_FILE`
3. `events.jsonl` in the repository's [state directory](#state-dir)

### `report`

Matches recent agent sessions in the event log to the commits their edits
landed in, and prints what each session contributed:

```sh
swarmia-cli report               # the 5 most recent sessions
swarmia-cli report --limit 0     # all of them
swarmia-cli report --session 7999d991
swarmia-cli report --json        # the draft payload delivery would send
```

```
Session 7999d991  claude-opus-4-8  2026-10-06 16:43 → 17:00
  2 prompt(s), 10 edit(s), 1698 AI line(s) at session end
  59e69c6  local   Add line-level matcher for attributing agent edits to commi…
      internal/attribution/align.go                 339 line(s)  (318 exact, 2 normalized, 19 modified)
      internal/attribution/normalize.go              77 line(s)
  960 of 1698 AI line(s) committed unchanged or reformatted (56%)
```

How it decides:

- **Candidate commits** are non-merge commits on any branch, made after the
  session's edits, that touch an edited file and are authored by your git
  `user.email` (`--all-authors` lifts that filter).
- **A line counts for a commit only if that commit introduced it** (relative to
  its parent), so later commits that merely carry the agent's lines along get
  nothing.
- **Classes:** `exact`; `normalized` — only a formatter changed it (whitespace,
  quotes, trailing commas, line wrapping); `modified` — recognisably the agent's
  line, edited by a human.
- **AI lines at session end** counts the agent's non-blank lines still present
  after its last edit to each file; lines it wrote and then replaced itself
  don't count.
- **pushed / local** shows whether a remote-tracking branch contains the commit.
  Only pushed commits will ever be sent.

Edits include changes shell commands made (shown as "via shell"; see
`log-hook`). Not yet covered: lines that moved to another file (renames).

### `state-dir`

Local state lives **outside the repository**, so it never shows up in
`git status` and survives `git clean -fdx`. Each repository gets its own
directory, keyed by its git common dir, so all worktrees of a clone share one:

| OS | Location |
|---|---|
| macOS | `~/Library/Application Support/swarmia/repos/<id>/` |
| Linux | `$XDG_STATE_HOME/swarmia/repos/<id>/` (default `~/.local/state/…`) |
| Windows (not shipped yet) | `%LocalAppData%\swarmia\repos\<id>\` |

`<id>` is a short hash; a `repo.json` inside names the repository. Files are
owner-only (`0600`), since they contain prompts and code. Override the base
directory with `$SWARMIA_STATE_DIR`. Print the current repository's directory
with:

```sh
swarmia-cli state-dir
```

## Setting up the Claude Code hook

`swarmia-cli` ships a ready-made hook configuration in
[`.claude/settings.json`](.claude/settings.json). It registers `log-hook` for
prompts, all tool use, and session lifecycle events.

**Build the binary first** — the config invokes `${CLAUDE_PROJECT_DIR}/swarmia-cli`,
so the compiled binary must exist at the repo root:

```sh
make build
```

Then reload hooks with `/hooks` inside Claude Code (or start a fresh session).
Events will stream into the repository's state directory:

```sh
tail -f "$(swarmia-cli state-dir)/events.jsonl"
```

Earlier versions logged to `.swarmia/events.jsonl` inside the repository; that
directory is no longer used and can be deleted.

To set this up in another repository, run [`swarmia-cli install`](#install) there
instead of copying the block by hand — it detects the agent and writes the right
`hooks`, choosing an appropriate command reference automatically.

## Development

```sh
make build         # compile ./swarmia-cli
make test          # run tests
make vet           # go vet
make fmt           # gofmt -w
make tidy          # go mod tidy
make npm-binaries  # cross-compile per-platform binaries into the npm packages
make clean         # remove build artifacts
```

Project layout:

```
main.go            thin entrypoint → cmd.Execute()
cmd/
  root.go          cobra root command
  install.go       the install command (wires hooks into detected agents)
  loghook.go       the log-hook command (captures hook payloads)
  paths.go         project dir, state dir and log-file resolution
  statedir.go      the state-dir command
  report.go        the report command
  lock_*.go        per-OS exclusive file locking
  version.go       version command + build metadata
internal/
  events/          the local event log format (written by log-hook)
  gitrepo/         finds the enclosing repo and reads HEAD without spawning git
  attribution/     matches agent edits to committed files, line by line
                   (golden cases from real sessions in testdata/golden)
  report/          groups the log into sessions and attributes them to commits
  snapshot/        captures what shell commands changed in the worktree
npm/
  cli/             the @swarmia/cli package (JS launcher + package.json)
  platforms/       one binary-only package per os/arch (@swarmia/cli-<os>-<arch>)
```

### npm packaging

`@swarmia/cli` follows the esbuild model: a small JS launcher
(`npm/cli/bin/swarmia-cli`) plus one binary-only package per platform, published
under `optionalDependencies` so npm installs only the host's binary. The launcher
resolves and execs it, passing stdin/stdout/stderr and the exit code through
unchanged.

To cut a release: bump the version in every `npm/**/package.json` in lockstep, run
`make npm-binaries` (fills each `npm/platforms/*/bin/`), then `npm publish` each
platform package followed by `npm/cli`. The binaries are build artifacts and are
gitignored. Windows is not packaged yet — the log-file locking is POSIX-only
(`syscall.Flock`) and needs a `LockFileEx` implementation first.

## License

Not yet determined.
