# swarmia-cli

The Swarmia Agent CLI captures local AI-agent activity so that AI-generated code
can be reliably attributed to the commits it ends up in.

Coding agents like Claude Code, Codex, and Copilot expose activity through
hooks. `swarmia-cli` registers as a hook, records what the agent does into a
local log, and (in future increments) links that activity to your git history.

> **Status:** early. Today the CLI can set up its hooks (`install`), log Claude
> Code hook events to a local JSONL file (`log-hook`), and store them per session
> in git refs (`process-logs`). Encryption, remote sync automation, and commit
> matching are on the roadmap.

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
swarmia-cli install            # add hooks to detected agents
swarmia-cli install --dry-run  # preview without writing
```

It adds the same wiring this repo uses — `log-hook` on prompts, all tool use, and
session lifecycle, plus `process-logs` on `SessionEnd`. It is **idempotent** (hooks
already present are left alone) and preserves any unrelated settings and hooks.

The command the hooks use to invoke `swarmia-cli` is auto-detected, in order:

1. a `swarmia-cli` binary at the repo root → `${CLAUDE_PROJECT_DIR}/swarmia-cli`
   (handy during local development)
2. an npm dependency at `node_modules/.bin/swarmia-cli` →
   `${CLAUDE_PROJECT_DIR}/node_modules/.bin/swarmia-cli`
3. otherwise a `swarmia-cli` on your `$PATH` → bare `swarmia-cli`

Override it with `--binary`, and target another directory with `--dir`. After
installing, reload hooks with `/hooks` in Claude Code (or start a fresh session).

### `log-hook`

Reads a Claude Code hook payload from stdin and appends it to a JSONL log, one
event per line, wrapped with a capture timestamp:

```json
{"logged_at":"2026-07-10T19:02:37Z","event":{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"npm test"}}}
```

It is a **passive** logger: it always exits 0 and writes nothing to stdout, so it
can never block or interfere with the tool call that triggered it. Concurrent
invocations are serialized with a file lock, so parallel tool calls and
subagents can't corrupt the log.

The log file is chosen in this order:

1. `--file <path>`
2. `$SWARMIA_LOG_FILE`
3. `$CLAUDE_PROJECT_DIR/.swarmia/events.jsonl` (falls back to the current
   directory if `$CLAUDE_PROJECT_DIR` is unset)

### `process-logs`

Takes the events captured by `log-hook`, groups them by session, and stores each
session's events into a git ref named `refs/ai-sessions/<session_id>`:

```sh
swarmia-cli process-logs
```

- **Git plumbing only.** It writes objects and updates refs directly; it never
  touches the working tree, index, or `HEAD`, so nothing shows up in `git status`.
- **One ref per session.** The server enumerates `refs/ai-sessions/*`. Inspect one
  with `git cat-file -p refs/ai-sessions/<session_id>:events.jsonl`.
- **Accumulates.** Each run appends the new events to the session's ref (a new
  commit chained onto the previous one), so a session that keeps going after its
  logs were shipped keeps updating the same ref.
- **Rotates.** After a successful store the local `events.jsonl` is emptied; if a
  store fails, the unstored events are put back so the next run retries.
- **Never blocks.** Like `log-hook` it always exits 0, so it is safe to wire into a
  hook. It is meant for a periodic or lifecycle hook (e.g. session end), not every
  tool call.
- **Does not push by default.** Pushing the refs to a remote is left to you (run
  `git push` yourself). Pass `--push` (with `--remote`, default `origin`) to also
  push each stored ref; push failures are logged and non-fatal.

Flags: `--file` (input path, same resolution as `log-hook`), `--push`, `--remote`,
`--keep` (don't empty the log after storing — for debugging).

## Setting up the Claude Code hook

`swarmia-cli` ships a ready-made hook configuration in
[`.claude/settings.json`](.claude/settings.json). It registers `log-hook` for
prompts, all tool use, and session lifecycle events, and runs `process-logs` on
`SessionEnd` to store the session's events into `refs/ai-sessions/*`. (Because the
log persists across sessions and `process-logs` groups by session, one session's
`SessionEnd` also flushes any events a previous session left behind.)

**Build the binary first** — the config invokes `${CLAUDE_PROJECT_DIR}/swarmia-cli`,
so the compiled binary must exist at the repo root:

```sh
make build
```

Then reload hooks with `/hooks` inside Claude Code (or start a fresh session).
Events will stream into `.swarmia/events.jsonl`:

```sh
tail -f .swarmia/events.jsonl
```

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
  processlogs.go   the process-logs command (stores events in refs/ai-sessions/*)
  paths.go         shared log-file / project-dir resolution
  version.go       version command + build metadata
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
