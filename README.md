# swarmia-cli

The Swarmia Agent CLI captures local AI-agent activity so that AI-generated code
can be reliably attributed to the commits it ends up in.

Coding agents like Claude Code, Codex, and Copilot expose activity through
hooks. `swarmia-cli` registers as a hook, records what the agent does into a
local log, and (in future increments) links that activity to your git history.

> **Status:** early. Today the CLI can log Claude Code hook events to a local
> JSONL file. Encryption, log rotation, git-ref sync, and commit matching are on
> the roadmap.

## Installation

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
Events will stream into `.swarmia/events.jsonl`:

```sh
tail -f .swarmia/events.jsonl
```

To use the hook in another repository, point the `command` at an installed
binary (e.g. `swarmia-cli log-hook` once it's on your `$PATH`) and add the same
`hooks` block to that project's `.claude/settings.json`.

## Development

```sh
make build    # compile ./swarmia-cli
make test     # run tests
make vet      # go vet
make fmt      # gofmt -w
make tidy     # go mod tidy
make clean    # remove the binary
```

Project layout:

```
main.go            thin entrypoint → cmd.Execute()
cmd/
  root.go          cobra root command
  loghook.go       the log-hook command
  version.go       version command + build metadata
```

## License

Not yet determined.
