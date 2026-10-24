# End-to-end tests

The fuzzers (`internal/attribution`, `internal/report`) measure attribution on
simulated edits. The end-to-end suite checks the real thing: a real coding
agent, installed at its current version, does a small task in a real
open-source repository with swarmia-cli's hooks installed, while a script plays
the developer — formatting, editing, committing, rebasing, pushing. Then the
suite checks what swarmia-cli captured, attributed and would send.

It runs against Claude Code and Codex. It answers questions the fuzzers can't:
did a new agent release change its hook payloads or transcripts; do hooks fire and stay quiet; does the
packaged CLI install and run; does attribution hold up on code an agent
actually wrote and prettier actually reformatted.

## Running

```sh
make e2e                                  # Claude Code "latest"
make e2e E2E_CLAUDE_VERSION=stable        # or a dist-tag / exact version
make e2e E2E_AGENT=codex                  # Codex "latest" (E2E_CODEX_VERSION=alpha, …)
E2E_ARTIFACTS=/tmp/e2e make e2e           # keep logs, transcripts, payloads
```

`E2E_MODEL` picks Claude Code's model (default `sonnet`), `E2E_CODEX_MODEL`
Codex's (default: Codex's own).

Claude Code's credentials, first match wins (nothing else from your
environment leaks in):

1. Workload identity federation, as in CI: `ANTHROPIC_FEDERATION_RULE_ID`,
   `ANTHROPIC_ORGANIZATION_ID`, `ANTHROPIC_SERVICE_ACCOUNT_ID` (and optionally
   `ANTHROPIC_WORKSPACE_ID`, `ANTHROPIC_OIDC_AUDIENCE`, default
   `https://api.anthropic.com`), with GitHub's OIDC request variables. The suite
   mints a **fresh** GitHub OIDC token for every agent run: a token with a
   `jti` can be exchanged only once.
2. `ANTHROPIC_API_KEY`.
3. `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) — the easiest way to
   run locally on a subscription.

Codex's: `OPENAI_API_KEY` (or `CODEX_API_KEY`), given to `codex exec` as
`CODEX_API_KEY`, the variable it reads.

Without credentials the suite still sets everything up (installs, fixture,
hooks, and for Codex the trust check below) and then skips the agent steps,
which checks the setup on its own.

Everything runs in a temporary directory with its own `HOME`,
`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, git config and swarmia-cli state, so it
never reads or writes your own agent configuration. Claude Code runs with
`--setting-sources project`, so only the fixture's `.claude/settings.json` (our
hooks) applies.

Credentials only go to the agent process. Artifacts (which CI publishes) are
scrubbed of every credential value the suite knows as they're written, and
finding one fails the run.

## Setup per run

1. Build swarmia-cli from the working tree.
2. Install the agent from npm into the temporary directory:
   `@anthropic-ai/claude-code@<version>` (`latest`, `stable`, or exact) or
   `@openai/codex@<version>` (`latest`, `alpha`, or exact).
3. Fixture: `unjs/ufo` (MIT, TypeScript, prettier, vitest), fetched at a pinned
   commit and published as `ssh://git@e2e.example/acme/ufo.git`; a teammate
   clone pushes to it. Remotes look hosted, so payloads name their
   repositories as they would for real (`e2e.example/acme/ufo`), but live on
   disk: a stand-in for ssh (`GIT_SSH_COMMAND`) serves them from local bare
   repositories.
4. `pnpm install`, then `swarmia-cli install --scope project` (for Codex with
   `--trust`, into the sandbox's `CODEX_HOME`). `.claude/` or `.codex/` is in
   `.git/info/exclude`, so the hooks are never committed.
5. Codex only: trust the fixture as a project, then ask Codex (`codex
   app-server`, `hooks/list`, no model involved) whether it loads all six
   swarmia-cli hooks and trusts them. Codex skips untrusted hooks silently, so
   a trust hash that no longer matches a new release fails here, by name.

## Scenario

| Step | Who | Exercises |
|---|---|---|
| "Add `isSecureURL` to `src/utils.ts`, with tests, in single quotes" (session S1) | agent | edit capture (Write/Edit, `apply_patch`), transcript usage |
| prettier over `src` and `test` (double quotes) | script | `normalized` |
| tweak one agent line by hand; commit `src`, then `test` (unpushed) | script | `modified`, partial commits |
| "Rename it with a single `sed`" (session S2) | agent | shell snapshots |
| teammate pushes; fetch, rebase, squash the last two, amend, push | script | reflog/author filter, reachability, rewrites |
| resume S1: "add `isInsecureURL` with a test"; commit, push | agent | resumed sessions |

Claude Code runs as `claude -p --output-format stream-json --verbose
--include-hook-events --permission-mode bypassPermissions --setting-sources
project`, with an explicit `--session-id` (and `--resume`) so sessions map
one-to-one to swarmia-cli's. Codex runs as `codex exec --json
--dangerously-bypass-approvals-and-sandbox`, which picks the session id itself
(the stream's `thread.started`), and `codex exec resume <id>`.

## Multi-repository scenario

Developers with many repositories start their agent in the directory holding
them (see `docs/multirepo.md`). This scenario does that: a workspace that isn't
a repository holds the fixture as `ufo`, a small `client` repository and an
`unrelated` one, all published like the fixture; the hooks are installed in
the workspace.

| Step | Who | Exercises |
|---|---|---|
| "Add `isLocalhostURL` to `ufo/src/utils.ts`; create `client/src/hosts.ts` with one shell heredoc" | agent | edit capture in one repository, shell snapshots in another named by the command |
| commit and push both repositories | script | attribution per repository |

Checks: one payload for the session (no tokens: one delivery target); `repos`
lists `ufo` and `client`, each with AI lines committed, and any other only if
a tool call worked in it; tool calls carry their repository and a path
relative to it; per repository, the same oracle as above (precision, recall,
class agreement); no local paths. It costs one agent turn.

## Checks

The agent writes different code every run, so checks are properties, not
snapshots:

1. **Capture**: every tool call in the agent's output stream has an event in
   swarmia-cli's session; the session has the id we chose, its prompt and model,
   and is marked ended; no hook reported an error.
2. **Usage**: swarmia-cli's output tokens are non-zero and equal an
   independent count over the session's transcripts (Claude Code: each
   response once; Codex: the rollout's last running total).
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
   missing), the run is skipped as inconclusive rather than failed. An agent
   that got no model response at all (a failed token exchange, an API outage)
   fails the run.

Artifacts (agent output streams, transcripts, swarmia-cli sessions, payloads, a
`metrics.json`) go to `E2E_ARTIFACTS` when set; CI uploads them.

## CI

`.github/workflows/e2e.yml`: nightly and on demand (pick the agent, a version
and a model), on Ubuntu. Nightly runs each agent on two release channels, which
separates "the agent changed" from "we broke something":

| Agent | Development | Stable |
|---|---|---|
| Claude Code | `latest` | `stable` |
| Codex | `alpha` | `latest` |

Jobs run in the `e2e` GitHub environment, which only `main` may deploy to and
which holds the `ANTHROPIC_*` federation variables and the `OPENAI_API_KEY`
secret (a dedicated OpenAI project with a spend cap). The Anthropic federation
rule accepts only that environment's OIDC subject,
`repo:swarmia/swarmia-cli:environment:e2e`, so pull requests, forks and other
branches can't obtain Anthropic credentials. Only Codex jobs get the OpenAI
key; before uploading artifacts, a workflow step checks them for it once more
and withholds them if it's there.

## Phases

1. ✅ Harness, isolation, credentials, the first agent task, capture/usage/
   payload checks. Its first CI run found two payload bugs (local `origin`
   paths in `repo`; no model for headless sessions).
2. ✅ The full scenario and the per-session attribution oracle. Its first run
   found a false attribution: a commit rebased after a later session's edit
   was credited to that session as `modified` (fixed: hunks carry the lines
   their edit replaced). Now: precision, recall and class agreement 1.000 for
   both sessions, all three classes exercised, ~$0.23 per run.
3. Codex, on both channels, with the same scenario and oracle. ← in progress
4. Install through the npm package; delivery to a fake `hook`; macOS.
