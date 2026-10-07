# End-to-end tests

The fuzzers (`internal/attribution`, `internal/report`) measure attribution on
simulated edits. The end-to-end suite checks the real thing: a real coding
agent, installed at its current version, does a small task in a real
open-source repository with swarmia-cli's hooks installed, while a script plays
the developer — formatting, editing, committing, rebasing, pushing. Then the
suite checks what swarmia-cli captured, attributed and would send.

It answers questions the fuzzers can't: did a new Claude Code release change
its hook payloads or transcripts; do hooks fire and stay quiet; does the
packaged CLI install and run; does attribution hold up on code an agent
actually wrote and prettier actually reformatted.

## Running

```sh
make e2e                                  # Claude Code "latest"
make e2e E2E_CLAUDE_VERSION=stable        # or a dist-tag / exact version
E2E_ARTIFACTS=/tmp/e2e make e2e           # keep logs, transcripts, payloads
```

Credentials, first match wins (nothing else from your environment leaks in):

1. Workload identity federation, as in CI: `ANTHROPIC_FEDERATION_RULE_ID`,
   `ANTHROPIC_ORGANIZATION_ID`, `ANTHROPIC_SERVICE_ACCOUNT_ID` (and optionally
   `ANTHROPIC_WORKSPACE_ID`, `ANTHROPIC_OIDC_AUDIENCE`, default
   `https://api.anthropic.com`), with GitHub's OIDC request variables. The suite
   mints a **fresh** GitHub OIDC token for every agent run: a token with a
   `jti` can be exchanged only once.
2. `ANTHROPIC_API_KEY`.
3. `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) — the easiest way to
   run locally on a subscription.

Without credentials the suite still sets everything up (installs, fixture,
hooks) and then skips the agent steps, which checks the setup on its own.

Everything runs in a temporary directory with its own `HOME`,
`CLAUDE_CONFIG_DIR`, git config and swarmia-cli state, so it never reads or
writes your own Claude Code configuration. Claude Code runs with
`--setting-sources project`, so only the fixture's `.claude/settings.json` (our
hooks) applies.

## Setup per run

1. Build swarmia-cli from the working tree.
2. Install Claude Code with `npm install --prefix <tmp>
   @anthropic-ai/claude-code@<version>` (`latest`, `stable`, or exact).
3. Fixture: `unjs/ufo` (MIT, TypeScript, prettier, vitest), fetched at a pinned
   commit. A local bare repository acts as `origin`; a teammate clone pushes
   to it.
4. `pnpm install`, then `swarmia-cli install --scope project`. `.claude/` is in
   `.git/info/exclude`, so the hooks are never committed.

## Scenario

| Step | Who | Exercises |
|---|---|---|
| "Add `isSecureURL` to `src/utils.ts`, with tests, in single quotes" (`--session-id S1`) | agent | Write/Edit capture, transcript usage |
| prettier over `src` and `test` (double quotes) | script | `normalized` |
| tweak one agent line by hand; commit `src`, then `test` (unpushed) | script | `modified`, partial commits |
| "Rename it with a single `sed`" (`--session-id S2`) | agent | shell snapshots |
| teammate pushes; fetch, rebase, squash the last two, amend, push | script | reflog/author filter, reachability, rewrites |
| `--resume S1`: "add `isInsecureURL` with a test"; commit, push | agent | resumed sessions |

Agent runs use `claude -p --output-format stream-json --verbose
--include-hook-events --permission-mode bypassPermissions --setting-sources
project`, with an explicit `--session-id` so sessions map one-to-one to
swarmia-cli's.

## Checks

The agent writes different code every run, so checks are properties, not
snapshots:

1. **Capture**: every tool call in the agent's output stream has an event in
   swarmia-cli's session; the session has the id we chose, its prompt and model,
   and is marked ended; no hook reported an error.
2. **Usage**: swarmia-cli's token totals are non-zero and equal an independent
   count over the transcript.
3. **Attribution**, per session: a session's lines are the union of its
   transcript's edit-tool inputs and the working tree's changes during its runs
   (which covers shell edits). Each line the scenario's pushed commits
   introduced gets an expected class — `exact`, `normalized` (only formatting
   differs), or `modified` (the hand-tweaked line) — compared with the payload
   for precision, recall and class agreement. A false positive is a line no
   agent wrote, or one that existed before the session started and that it
   didn't write. Invariants: every attributed commit is on `origin/main`; the
   teammate's commit is never attributed; the tweaked line comes out
   `modified`; prettier's changes come out `normalized`.
4. **Payload**: required fields present; a fake secret planted in the prompt
   comes out `[REDACTED:…]`; no temp paths or user names.
5. **Inconclusive vs failed**: if the agent didn't do the task (the function is
   missing), the run is skipped as inconclusive rather than failed.

Artifacts (agent output streams, transcripts, swarmia-cli sessions, payloads, a
`metrics.json`) go to `E2E_ARTIFACTS` when set; CI uploads them.

## CI

`.github/workflows/e2e.yml`: nightly and on demand (pick the Claude Code
version), matrix `latest` × `stable` on Ubuntu, with `id-token: write` for
federation. It needs the organization's `ANTHROPIC_*` federation variables to be
available to this repository, and the federation rule to accept this
repository's OIDC subject.

## Phases

1. ✅ Harness, isolation, credentials, the first agent task, capture/usage/
   payload checks. Its first CI run found two payload bugs (local `origin`
   paths in `repo`; no model for headless sessions).
2. ✅ The full scenario and the per-session attribution oracle. Its first run
   found a false attribution: a commit rebased after a later session's edit
   was credited to that session as `modified` (fixed: hunks carry the lines
   their edit replaced). Now: precision, recall and class agreement 1.000 for
   both sessions, all three classes exercised, ~$0.23 per run.
3. Install through the npm package; delivery to a fake `hook`; macOS; Codex
   once swarmia-cli supports it. ← next
