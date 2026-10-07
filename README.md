# swarmia-cli

The Swarmia Agent CLI answers a question coding-agent analytics can't: **which
lines of which commits came from an AI coding agent, and from which session?**

It hooks into your coding agent (Claude Code or Codex), records what the agent
writes, works out on your machine which committed lines those edits became,
and — once configured — sends Swarmia line numbers and session metadata.
Never your source code.

> **Status:** early, but end-to-end tested against real Claude Code and Codex
> releases. Cursor, Copilot CLI and Gemini CLI are planned. Without a token
> configured, nothing leaves your machine.

## How it works

```
coding agent ──hooks──▶ swarmia-cli log-hook ──▶ local event log
                                                      │  (session end, in the background)
                                                      ▼
                     your git history ──▶ swarmia-cli process ──▶ per-session attribution
                                                      │  (only with a token)
                                                      ▼
                                          Swarmia: line ranges + session metadata
```

1. **Capture.** On every prompt, tool call and session start/end, the agent
   runs `swarmia-cli log-hook`, which appends the event to a log in a per-user
   state directory. Edits made through the agent's edit tools are in the
   events; edits made through shell commands (`sed`, codegen, scripts) are
   found by snapshotting the working tree around each command.
2. **Attribute.** When a session ends, a background `swarmia-cli process` folds
   the log into a compact per-session record and matches the session's edits to
   your commits, line by line: each committed line it introduced is `exact`,
   `normalized` (only a formatter touched it) or `modified` (edited by a human).
3. **Deliver.** If a Swarmia token is configured, changed sessions are sent:
   line ranges in *pushed* commits, prompts (redacted), tool names and commands
   (stripped of code), and token usage.

`swarmia-cli report` shows all of this locally, at any time.

## Design principles

### Privacy

- **No source code leaves your machine.** Attribution is line numbers in
  pushed commits; where code needs to be shown, Swarmia can read it from your
  Git provider, which it's already connected to. Shell commands are sent with heredoc bodies, inline scripts and
  long quoted strings removed. Tool output is never sent.
- **Secrets are redacted** from prompts and commands before sending (provider
  tokens, private keys, passwords in URLs and connection strings, secret-looking
  assignments, random-looking strings), each becoming `[REDACTED:<kind>]`.
- **Local details are masked:** the repository root becomes `<repo>`, your home
  directory `~`, your user name `<user>`; file paths are repository-relative.
- **You can withhold your prompts** (`SWARMIA_PROMPTS=0`), whatever the team's
  configuration says.
- **Nothing is sent without a token**, and only pushed commits are ever sent.
  Local state lives outside your repositories, readable only by you.

Prompts can still contain code you paste into them; redaction can't tell.

### Never in the way

The agent waits for each hook before it continues, so every millisecond a hook
takes is a millisecond added to your session. swarmia-cli treats hook latency
as a budget:

| Hook | Typical cost |
|---|---|
| Prompts, edits, reads and other tool calls | ~5–7 ms per event: an append to a local file and a read of `.git/HEAD`, with no `git` process |
| Shell commands (Bash) | one `git status` before and after, for the snapshot: ~35 ms per command in a small repository, ~140 ms in a 14k-file one |
| Session end | starts the background run and returns immediately |

- Anything heavier — folding the log, matching edits to commits, delivery —
  runs in a detached background process after the session ends, never while the
  agent waits.
- Hooks are passive: they always exit 0 and never print to the agent, so a
  failure can't block or derail a tool call.
- Concurrent hooks are serialized with file locks; nothing is written into your
  repository, git config or git hooks.
- Delivery failures back off and retry; nothing is lost while the network or
  Swarmia is unavailable.

Numbers are from a MacBook (Apple Silicon); `git status` cost grows with the
size of the repository's working tree.

### Accurate, and honest about what it doesn't know

- Matching runs locally, where both the agent's exact output and your committed
  files are available. It survives formatters, line wrapping, small human
  edits, moved code, amends, squashes and rebases.
- A commit is only credited with lines **it introduced**, so later commits
  that carry the agent's lines along get nothing.
- Lines the CLI can't attribute are **unknown, not human**: a person may have
  written them — or an agent session that wasn't recorded.

### Tested against reality

- **Unit and golden tests**, including cases recorded from real agent sessions.
- **Two fuzzers** that track every line's true origin: one for the matcher
  (human edits, formatter runs, moved code), one for the whole pipeline in real
  git repositories (partial commits, amends, squashes, rebases onto a
  teammate's commits). CI enforces minimum precision and recall.
- **An end-to-end suite** ([docs/e2e.md](docs/e2e.md)) that runs real Claude
  Code and Codex releases (each on two release channels) nightly on an
  open-source TypeScript repository: the agent
  writes code, a script formats, tweaks, commits, rebases, squashes and
  pushes, and the suite checks capture, attribution, token usage and payload
  privacy against an independent oracle.

## Using it

### 1. Install the binary

As a devDependency of the repository, so everyone working in it gets the same
version:

```sh
pnpm add -D @swarmia/cli      # or: npm install -D @swarmia/cli
```

Or for yourself, without npm:

```sh
go install github.com/swarmia/swarmia-cli@latest     # puts swarmia-cli in ~/go/bin
```

Binaries are also attached to each [GitHub
release](https://github.com/swarmia/swarmia-cli/releases), or build from source:
`git clone https://github.com/swarmia/swarmia-cli && cd swarmia-cli && make
build` (Go 1.26+). macOS and Linux are supported, on arm64 and x64.

Then run it as `pnpm exec swarmia-cli` (or `npx swarmia-cli`) if it's a
devDependency, or as `swarmia-cli`.

### 2. Install the hooks

`swarmia-cli install` adds its hooks to the coding agents configured in a
repository (Claude Code: a `.claude` directory; Codex: a `.codex` directory).
Pick a **scope**:

| Scope | Claude Code | Codex | Use it when |
|---|---|---|---|
| `project` (default) | `.claude/settings.json` (committed) | `.codex/hooks.json` (committed) | the whole team should be tracked in this repository |
| `local` | `.claude/settings.local.json` (gitignored) | — (Codex has no such file) | you want it for yourself in one repository |
| `user` | `~/.claude/settings.json` | `~/.codex/hooks.json` | you want it in every repository on this machine |

```sh
swarmia-cli install                              # project scope, in the current repository
swarmia-cli install --scope local                # just you, here
swarmia-cli install --scope local --dir ../app   # just you, in another repository
swarmia-cli install --dry-run                    # preview
swarmia-cli install --trust                      # also trust the Codex hooks for yourself
```

Things to know:

- **`project`** hooks reference the CLI the way the repository provides it: a
  `swarmia-cli` binary at the repository root (development), the npm
  devDependency's launcher (`node_modules/@swarmia/cli/bin/swarmia-cli`), or one
  on `$PATH`. Until the repository's one exists — in a fresh clone or worktree,
  before `make build` or `pnpm install` — the hooks quietly do nothing.
- **`local`** and **`user`** hooks point at the binary that ran `install`, by
  absolute path — rebuilding it updates them. Run from the repository's npm
  devDependency (`pnpm exec swarmia-cli install --scope local`), they point at
  its launcher instead, so upgrading the dependency doesn't break them.
- **`local`** settings live in one checkout: new git worktrees don't get them,
  so run `install --scope local --dir <worktree>` there too.
- **`user`** scope captures every repository you open a coding agent in,
  personal projects included. That stays local unless a token applies there —
  but a token in your *user* config applies everywhere, and Swarmia only drops
  repositories outside your organization after they've been sent. Prefer
  `project` or `local` scope for work repositories.
- `install` is idempotent, keeps your other settings and hooks, and warns if a
  repository would end up with hooks in both of its settings files (every
  event would be logged twice).

Then activate them:

- **Claude Code:** reload hooks with `/hooks`, or start a new session.
- **Codex** runs a hook only once *you* have trusted it — each developer, for
  project hooks too — and skips the others silently. Run `/hooks` in Codex and
  approve swarmia-cli's six hooks, or let `swarmia-cli install --trust` record
  that approval in your `~/.codex/config.toml` (it adds trust entries for
  swarmia-cli's hooks and leaves the rest of the file as it was). Codex also
  reads a project's `.codex/` only once you've trusted the project folder,
  which it asks about the first time. `swarmia-cli config` warns about hooks
  Codex won't run. Changing a hook's command (say, reinstalling with another
  binary) needs trusting again.

### 3. See what it captured

```sh
swarmia-cli report                 # the 5 most recent sessions
swarmia-cli report --limit 0       # all of them
swarmia-cli report --json          # exactly what would be sent
```

```
Session 7999d991  claude-opus-4-8  2026-10-06 16:43 → 17:00
  2 prompt(s), 10 edit(s) (3 via shell), 1698 AI line(s) at session end
  277 model call(s): 100.3M input tokens (98% cache reads), 408k output
  59e69c6  pushed  Add line-level matcher for attributing agent edits to commi…
      internal/attribution/align.go                 339 line(s)  (318 exact, 2 normalized, 19 modified)
      internal/attribution/normalize.go              77 line(s)
  960 of 1698 AI line(s) committed unchanged or reformatted (56%)
```

### 4. Send to Swarmia

Configure a token (from Swarmia; it looks like `swm_cli_…`). It is
**write-only** — it can submit sessions for its organization and nothing else —
so a team can commit it in `.swarmia.json` at the repository root, or you can
keep it in `~/.config/swarmia/config.json`:

```json
{ "token": "swm_cli_…" }
```

Settings can also come from the environment: `SWARMIA_TOKEN`,
`SWARMIA_ENDPOINT` (default `https://hook.swarmia.com/cli`), and
`SWARMIA_PROMPTS=0` to withhold your prompts. The environment wins over your
user file, which wins over the repository's. Check what applies:

```sh
swarmia-cli config
```

From then on, sessions are sent in the background when they end, and again
when they change (for instance when you push a commit). `swarmia-cli send`
sends right away; `send --dry-run` shows how many would go.

### Commands

| Command | What it does |
|---|---|
| `install` | add hooks to the coding agents in a repository (`--trust`: trust them in Codex) |
| `report` | show recent sessions and the committed lines they produced |
| `config` | show the effective configuration and where it comes from; warn about untrusted Codex hooks |
| `send` | send changed sessions to Swarmia now |
| `process` | fold the event log and attribute sessions (runs automatically) |
| `state-dir` | print where this repository's local state lives |
| `log-hook` | the hook itself; agents call it |
| `version` | print version and build information |

## Internals

### Capture: `log-hook`

Each hook payload is appended to `events.jsonl` in the repository's state
directory, one record per line, with a timestamp and the repository's position
— worktree root, HEAD commit and branch, read from `.git` files without
spawning git:

```json
{"logged_at":"2026-07-10T19:02:37Z","git":{"root":"/src/app","head":"ec37b5d…","branch":"main"},"event":{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"npm test"}}}
```

Hooks call `log-hook --agent codex` for Codex (Claude Code is the default),
and the agent is recorded with each event. Codex runs hooks in the session's
working directory and passes no project directory, so its project hooks walk
up to the repository's `swarmia-cli` in `sh` (well under a millisecond; `git
rev-parse` would cost ~10 ms per hook). Codex edits files with `apply_patch`;
each patch chunk's lines before and after are diffed like an Edit tool's old
and new strings. Its `SessionEnd` hook gets at most 3 seconds, which is
enough: the processing it triggers runs detached.

Around each Bash call, `log-hook` snapshots the worktree's dirty files before
the command and, when it finishes (`PostToolUse` or `PostToolUseFailure`),
diffs what changed and stores it as `changes` on that record. Only lines the
command *authored* count: a line's normalized text must occur more often
afterwards than in the file before the command or in HEAD, which excludes
formatter runs over human code and content restored by git (`stash`,
`checkout`, `pull`). Files over 1 MB and binary files are skipped; human edits
saved while a command runs are indistinguishable from the command's.

The log file is `--file`, else `$SWARMIA_LOG_FILE`, else the state directory's
`events.jsonl`. Appends are serialized with a file lock. User-scope hooks run
`log-hook --skip-if-project-hooks`, standing down in repositories that have
their own swarmia-cli hooks.

### State directory

Local state lives outside the repository, so it never shows up in `git status`
and survives `git clean -fdx`. One directory per repository, keyed by its git
common dir, so all worktrees of a clone share it:

| OS | Location |
|---|---|
| macOS | `~/Library/Application Support/swarmia/repos/<id>/` |
| Linux | `$XDG_STATE_HOME/swarmia/repos/<id>/` (default `~/.local/state/…`) |

It holds `events.jsonl`, `sessions/` (one JSON record per session),
`snapshots/` (for shell commands) and `repo.json` (which repository it is).
Files are owner-only (`0600`). Override the base with `$SWARMIA_STATE_DIR`;
`swarmia-cli state-dir` prints it.

Worktrees can have different swarmia-cli versions sharing this state, so
event records and session files carry a `format` version. A version leaves
what a newer one wrote, in a format it doesn't know, alone: such sessions,
and their records in the log, wait for a newer version to process them.

### Processing: `process`

- **Fold:** the raw log becomes one compact record per session — prompts, tool
  calls, edit hunks and the lines each edit replaced, without the whole-file
  payloads (~6× smaller) — and the log is emptied. Identical records logged
  twice fold once.
- **Usage:** token counts are read incrementally from the agent's transcript
  (usage fields only), counting each API response once. Codex records running
  totals, so each response is the difference between consecutive ones.
- **Attribute:** see below. Results are cached per session and recomputed only
  when its edits or candidate commits change.
- **Prune:** sessions inactive for 30 days are deleted.
- **Deliver**, if a token is configured.

It runs detached from `log-hook` at session end (or when the log passes 8 MB),
and before `report`. A lock keeps it to one run at a time.

### Attribution

- **Candidate commits:** non-merge commits reachable from any ref, made after
  the session's edits, touching an edited file, and yours — authored with your
  `user.email`, or created on this machine according to your worktrees'
  reflogs (other emails, rebase picks, cherry-picks). Amended or rebased-away
  commits are unreachable, so they drop out and their replacements are matched.
- **Matching:** each edit hunk is located in the committed file with a
  line-level local alignment; matching lines are classed `exact`, `normalized`
  (equal once whitespace, quotes, trailing commas and line wrapping are
  ignored) or `modified` (at least half the tokens shared). A `modified` match
  against a line the edit itself replaced is dropped: that commit holds the
  pre-edit version.
- **Introduced lines only:** a commit is credited only with lines it added
  relative to its parent.
- **Matcher version** (`internal/attribution.Version`) travels with every
  result so results from a version later found wrong can be discounted.

Not yet covered: code moved to another file (renames).

### Delivery

Each changed session's full payload — exactly `report --json` — goes in a
gzipped `POST <endpoint>/sessions` with the token as a bearer token; the
server keeps the latest version per session. Sessions are resent when their
payload changes, at most every 15 minutes while still running. The endpoint
answers per session (`accepted` = queued, or `rejected` with a reason that
`report` shows); failures back off exponentially, honouring `Retry-After`, and
for six hours on an unusable token. The contract is
[docs/ingest-api.md](docs/ingest-api.md).

## Development

```sh
make build         # compile ./swarmia-cli
make test          # unit tests, golden cases and fuzzers (with CI thresholds)
make vet           # go vet (including the e2e package)
make fmt           # gofmt -w
make e2e           # end-to-end with a real Claude Code release (docs/e2e.md)
make e2e E2E_AGENT=codex   # ... or Codex
make npm-pack VERSION=1.2.3   # build the npm packages into dist/npm
make npm-smoke-test VERSION=1.2.3   # ... and install them with npm and pnpm
```

This repository runs its own hooks from `.claude/settings.json`, which invokes
`${CLAUDE_PROJECT_DIR}/swarmia-cli` — run `make build` before starting Claude
Code here, then watch events with `tail -f "$(./swarmia-cli state-dir)/events.jsonl"`.

Exploring attribution quality beyond the CI settings:

```sh
go test ./internal/attribution -run TestAttributionFuzzer -v -fuzz.trials=5000 -fuzz.maxops=60
go test ./internal/report -run TestPipelineFuzzer -v -pfuzz.trials=100 -pfuzz.steps=30
go test ./internal/attribution -run '^$' -fuzz FuzzMatchHunk -fuzztime 1m   # invariants
```

Design notes and plans: [docs/roadmap.md](docs/roadmap.md).

### Layout

```
main.go            entrypoint → cmd.Execute()
cmd/               the commands (cobra): install, log-hook, process, report,
                   config, send, state-dir, version; detached background runs
internal/
  events/          the local event log format
  gitrepo/         finds the repository and reads HEAD and reflogs without spawning git
  snapshot/        what shell commands changed in the worktree
  attribution/     matching agent edits to committed files (golden cases in testdata/)
  report/          folding the log into sessions; attributing them to commits
  usage/           token usage from agent transcripts (Claude Code) and rollouts (Codex)
  codex/           Codex's hooks files and hook trust
  config/          layered configuration
  redact/          secret redaction and code stripping
  payload/         what delivery sends, with the privacy rules applied
  deliver/         sending to the ingest endpoint
  fslock/          cross-process file locks
e2e/               the end-to-end suite (build tag e2e)
npm/               the npm packages (launcher + one binary package per platform)
docs/              roadmap, ingest API, end-to-end suite
```

### npm packaging

`@swarmia/cli` follows the esbuild model: a small launcher (`npm/cli/bin`) plus
one binary-only package per platform under `optionalDependencies`, so npm and
pnpm install only the host's binary. The launcher is POSIX `sh` that forks
nothing on its common path — it adds ~6 ms per hook, where starting Node added
~85 ms — and project-scope hooks call it directly rather than through
`node_modules/.bin`, whose pnpm shim forks several processes of its own. The
packages in `npm/` carry a placeholder version; a release stamps its tag's.
Releasing: [RELEASING.md](RELEASING.md). Windows builds compile but aren't
packaged or tested yet.

## License

MIT. See [LICENSE](LICENSE).
