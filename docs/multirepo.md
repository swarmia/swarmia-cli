# Multi-repo sessions

## Problem

Some developers start their agent in a parent directory that holds many git
repositories (5 to 100), not inside one repository. Agents also edit sibling
repositories from a session started inside one of them (`../common/...`).
Both cases need a session to span repositories.

This is separate from `docs/optimization-roadmap.md`, which makes
snapshotting *one* repo cheaper. Here the cost is the *number* of repos.

## What breaks today

Everything assumes the project dir is exactly one repo:

- **log-hook** (`cmd/loghook.go`): `gitrepo.Find(projectDir())` fails in a
  parent dir, so records carry no `git` and shell commands get no snapshots.
- **process / report** (`processOptions` in `cmd/process.go`): the same
  `gitrepo.Find` fails, so the log is never folded, attributed or delivered.
  Events pile up in a state dir keyed by the parent path (the non-repo
  fallback in `repoKey`, `cmd/paths.go`).
- **Edits outside the root are dropped**, even in a single-repo session:
  `relPath` (`internal/attribution/claude.go`) and `CodexEdits`
  (`internal/attribution/codex.go`) skip paths outside `root`.
- **Attribution** lists candidate commits from a single repo
  (`Attribute`, `opts.RepoDir`).
- **Delivery** sends one repository per session (`payloadContext(repo)`,
  `docs/ingest-api.md`).
- **Install**: Claude Code reads `.claude/settings.json` from the launch dir,
  so hooks committed in the child repos don't fire. Hooks have to be in the
  parent dir's `.claude/settings.local.json`. Check that `install` and
  `--skip-if-project-hooks` (`projectHasLogHook`) work when the launch dir is
  not a repo.

## Design: the session is the unit, the repo belongs to each edit

- **Logging**: one log per launch dir, as now (keyed by repo common dir, or
  the dir itself outside a repo). Each file change, whether from an edit tool
  or a shell snapshot, records the repo it is in (common dir and worktree
  root), not one `git` per record.
- **Fold**: key edits by (repo, path). `AILinesByPath` and friends become
  per repo.
- **Attribute**: run once per repo the session touched, each with its own
  candidate commits, author email and reflog.
- **Deliver**: one payload per session, listing its repos (see Delivery
  below and `docs/ingest-api.md`).
- **process** must work when the launch dir isn't a repo: discover repos from
  the folded edits instead of from `projectDir()`.

## Delivery

The contract is in `docs/ingest-api.md`.

### Options considered

- **One payload per (session, repo)**, each a full copy. Closest to the old
  single-repo contract, but usage, prompts and anything derived from the whole
  session (cost, active time, analysis of the timeline) would be counted once
  per repo, and the session's identity would stop being the agent's session
  id, which other sources of the same session (e.g. agent telemetry, which
  knows nothing of repos) are keyed on.
- **Per-repo slices with session-wide data in one "primary" slice.** No
  double counting, but the primary can change as the session grows, moving
  usage between slices, and is lost if the primary repo isn't connected.
- **Separate session and per-repo attribution records.** The cleanest
  normalization, but two record types and keys for no gain at current
  payload sizes.
- **One payload per session, with a repo on each repo-specific item.**
  Chosen. The session stays the unit; usage and prompts are sent once; tool
  calls stay one ordered timeline across repos; the server splits by repo
  where it needs to.

### Shape

- `repos[]`: each repo the session worked in (edited, read, searched, ran
  shell commands in) with its own `ai_lines_generated`/`ai_lines_committed`.
  The first is the repo the session started in, if any. Listing repos it only
  read keeps sessions that edited nothing deliverable. Repos without an
  identifiable `origin` aren't listed.
- `tools[].repo` and `commits[].repo`; tool file paths are relative to their
  own repo. A tool call outside every listed repo keeps its name, command and
  status but has no `repo` or `file`.
- The top-level `repo`, `ai_lines_generated` and `ai_lines_committed` are
  gone. Nothing consumes the contract in production yet, so v1 changes in
  place.

### Client rules

- **One delivery target per session.** The token and endpoint come from the
  configuration where the agent was started (its repo, else the nearest
  directory file), plus the user's file and the environment as always. The
  whole session goes there; repos' own tokens don't route parts of it
  elsewhere. A developer belongs to one organization, and several tokens for
  one would only be a source of confusion. (An earlier version split the
  session by each repo's token; it couldn't keep commands and prompts from
  naming the other organization's repos anyway, and counted usage once per
  part.)
- **Prompts withheld if the configuration or any repo the session touched
  opts out.** Prompts belong to the whole session, so the strictest setting
  wins.
- **`author_email`** comes from the first listed repo that has one.
- **Masking**: each listed repo's local root becomes `<repo:host/path>`, an
  unlisted repo's `<repo>`, and a launch dir outside any repo `<project>`.
- **Delivery state** (what was sent, rejections, backoff) is kept per
  session, as before.

### Server expectations (in the contract)

- Keep a session if at least one of its repos is connected; drop the
  unconnected repos' data.

This also fixes single-repo sessions that edit a sibling, so it's useful
regardless of how the agent was launched.

## Which repos to snapshot

Running `git status` in every repo before and after every shell command isn't
feasible. Measured on 32 repos in one directory (macOS, warm cache, our
current `dirtyPaths` flags):

| | time |
|---|---|
| one small repo (~20 files) | ~5 ms |
| 24 repos under ~1k files, 8 in parallel | ~70 ms |
| all 32, serial | ~650 ms |
| all 32, 8 in parallel | ~190 ms |

For small repos the cost is process spawns, not size. Extrapolated (not
measured), 100 small repos take ~250–300 ms per side even in parallel, twice
per shell command, against a 500 ms `snapshot.Budget`.

So a shell command snapshots only the repos it's likely to touch:

1. **Named by the command's context.**
   - The repo of the shell's working directory (the hook payload's `cwd`;
     Claude Code keeps the Bash cwd across calls while it stays inside the
     project).
   - Repos the command names: `cd X`, `pushd X`, `git -C X`, `make -C X`,
     and path arguments whose first component is a known child repo.
     `internal/redact/command.go` already tokenizes commands.
2. **The session's working set.** Repos the session has read, edited,
   searched or changed through the shell so far, kept in a small per-session
   file. Usually one to three repos.

Repos are snapshotted in parallel within the shared budget. Child repos
under the launch dir are discovered by reading one or two directory levels
for `.git`, cached and refreshed when the launch dir's mtime changes.

Each record says which repos were snapshotted, so a missed one is visible in
the data rather than silent.

### The gap, accepted and measured

A command that writes into a repo it never named, outside the working set
(a script that writes elsewhere, a codegen step across repos), goes
unrecorded. We accept that and measure it:

- While dogfooding, after a sample of shell commands, run a detached
  background sweep of the other repos and count commands that changed files
  in a repo outside tiers 1–2. Record the misses (command shape, which
  repo) locally.
- Turn the misses into test cases and better tier rules (more command
  patterns, working-set rules). The aim is to make tiers 1–2 reliable from
  real sessions rather than to watch everything.

Only if the misses stay significant: on macOS, the FSEvents history (save
the event id before the command, replay the launch dir from it afterwards)
says which repos changed, with no daemon and one stream for all repos. It
needs cgo and a flush marker, and has no Linux equivalent. See the daemon
section in `docs/optimization-roadmap.md` for the same issues.

## Relation to other optimizations

- The private index (idea 1 in `docs/optimization-roadmap.md`) makes each
  repo's status cheaper and applies per repo. Independent of this.
- `bashEditDiff` probably only covers the repo at Claude Code's cwd, or
  nothing when the launch dir isn't a repo. Unverified.

## Next steps

1. ~~Resolve each edit's repo in fold; attribute per repo; make `process`
   work outside a repo.~~ Done.
2. ~~Build payloads in the new shape.~~ Done (one per session; see Client
   rules).
3. ~~Snapshot tiers 1 and 2, recording which repos each command covered.~~
   Done (`snapshot.Scope`, at most 8 worktrees per command; records list
   them in `snapshotted`). Measured from `~/swarmia-src/monorepo`: three
   extra worktrees cost ~7 ms per side, eight ~30 ms.
4. ~~E2E scenarios: launch in a parent dir, edit two child repos, commit in
   both; a single-repo session editing a sibling.~~ Done (`e2e/multirepo_test.go`;
   the sibling case in `cmd/multirepo_test.go` and
   `internal/report/multirepo_test.go`).
5. Sampled background sweep while dogfooding; feed the misses back into
   tests and tier rules.
