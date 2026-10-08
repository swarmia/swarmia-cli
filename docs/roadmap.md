# Swarmia Agent CLI — Roadmap

> Working roadmap.

## Summary of the change from the original design

The original design stored encrypted, full session logs (including source code)
in `refs/ai-sessions/*` in the customer's git repo, and matched sessions to
commits on Swarmia's servers. This plan instead:

1. **Matches locally.** The CLI attributes AI-generated lines to commits on the
   developer's machine, where it has the exact AI output and the exact committed
   content.
2. **Sends no source code.** Only line-level attribution (per pushed commit +
   path), session metadata and (redacted) prompts go to Swarmia.
3. **Delivers over HTTPS**, not via git. No refs, no per-user encryption keys, no
   CI decryption script, no polling of customer repos. Fits the monorepo's
   webhook-driven GitHub integration.

**Why:** none of the use cases need source code at rest. AI % per line/PR/dev,
acceptance/survival rate, rework, cost-per-PR and incident linking need *line
attribution*; session analysis, skill loop and struggle detection need *prompts
and metadata*. The original storage and encryption machinery existed only to protect
code we don't need.

## Principles (revised)

1. Session ↔ commit linking is reliable, including after formatters and light
   human edits. Many-to-many.
2. ~~Locally running code is as dumb as possible.~~ → Local code does capture,
   matching and redaction; everything else (aggregation, propagation through
   squash merges, thresholds, analytics) is server-side. Matching is versioned so
   results from bad versions can be discounted.
3. Source code never leaves the developer's machine.
4. Prompt privacy: only the author, Swarmia and customer admins see prompts; any
   individual can withhold their own prompts.
5. Never interferes with the developer: hooks are passive, always exit 0, heavy
   work runs in a detached background worker.
6. No network traffic unless a Swarmia token is configured (repo or user level).

## Decisions

| # | Topic | Decision | Notes / consequences |
|---|---|---|---|
| D1 | Prompt text | **Sent by default**; disable with a local setting | Individual opt-out (`prompts: false` in user config or `SWARMIA_PROMPTS=0`) always wins over repo/org config. Opt-out withholds prompt text only — line attribution and metadata still flow so org metrics have no blind spots. |
| D2 | Install scope | **Both** user-global (MDM) and per-repo | Global hooks must not capture non-company repos **at all — not even in the local log**: developers have personal projects (learned while dogfooding). So the repo filter (remote matches an org allowlist) belongs in `log-hook`, not only in delivery. When both are installed, the global hook no-ops in repos that have per-repo hooks (de-dupe, done via `--skip-if-project-hooks`). `install --scope user` exists today but has no repo filter yet — don't use it outside testing. |
| D3 | Auth & identity | **Org token + git author email** | Token is write-only: can only submit data for its org; server rejects repos outside the org. Developer identity = git author email, mapped by existing Swarmia identities. |
| D4 | Token distribution | **Both**: committed `.swarmia.json` (write-only token, like a Sentry DSN) and user config | User config (`~/.config/swarmia/config.json`, MDM or `swarmia-cli configure`) overrides repo config. No token anywhere → capture + match locally, send nothing. Accepted risk: a committed token allows junk submissions for that org → server-side validation/rate limits. |
| D5 | Commit detection | **Lazy scan only** | On agent hooks and worker runs, scan `git log --all` + reflog since the oldest pending session. No git hooks, no daemon. Commits made after the developer stops using the agent are picked up at their next session. |
| D6 | Partial edits | **Line-level diff** | For each located hunk, classify every committed line: `exact` (identical to AI output), `normalized` (D7), or `modified`/`human`. Server gets per-line classes, applies its own thresholds. |
| D7 | Formatting-only differences | **Count as AI**, tagged `normalized` | Normalization ignores whitespace, blank/comment lines, trailing `,`/`;` and quote style. |
| D8 | Git-ref storage | **Delete** `process-logs` / `refs/ai-sessions/*` | Remains in git history. |
| D9 | Agents | Claude Code, then **Codex, Cursor, Copilot CLI, Gemini CLI** | Capture layer needs a per-agent payload normalizer (session id, tool names, edit shapes differ). Order within the four: TBD. Reference: git-ai's `agent-support/` and per-agent "presets" normalize hook JSON for 14 agents, including where each puts shell commands (`tool_input.command`, `toolInput.command`, `tool_input.cmd`, `cmd`, …); its docs cover installing in cloud agents (Claude on the web, Codex Cloud, Cursor Agent, Devin), where sandboxes block background sockets. |
| D10 | Secret redaction | **Own layered redactor** (revised from "full scanner") | Embedding gitleaks cost +5 MB and ~200 modules and caught 1 of 4 sample secrets. Instead: provider token formats, private keys, URL/connection-string/`curl -u` passwords, secret-looking assignments, high-entropy strings; applied to prompts and commands. |
| D15 | Shell commands in payload | **Keep structure, strip code** | Heredoc bodies, inline interpreter scripts and quoted strings > 120 chars replaced by placeholders, then redacted; capped at 1000 chars. Repo root, home dir and user name masked in all free text. |
| D16 | Endpoint | **`https://hook.swarmia.com/cli`** default, overridable | Received by Swarmia's `hook` service. |
| D17 | Ingest feedback | **Shape checks synchronous, the rest asynchronous** | `hook` validates the request and each session's shape, queues each session for processing, and answers `200` with per-session `accepted` (= queued) / `rejected` (`invalid payload: <field>`, `repository unknown`, `session too large`); `503` if nothing could be queued, `401` for tokens not matching `swm_cli_[0-9a-f]{64,}`. The repo-in-org check, token→org resolution and storage happen downstream and aren't visible to the client. The client also treats a bare `2xx` as "all queued". **Planned:** a synchronous endpoint on `https://app.swarmia.com/` where the CLI, using the same write-only token, can check whether its sessions were received and stored or dropped (and why) — without exposing any session content, since the token may be public. Surfaced in `report`/`config` when it exists. |
| D11 | Windows | **Later, but design for it now** | Abstract file locking (flock) and paths behind small interfaces; keep state dir resolution OS-aware. |
| D12 | Non-Edit/Write changes | **Snapshot around Bash** | Pre/PostToolUse on Bash records a cheap working-tree fingerprint (`git status --porcelain` + hashes of dirty files); diffs between the two are AI-made changes (codegen, sed, agent-run formatters). Budget: tens of ms per Bash call; must stay off the hook's critical path where possible. Read-only commands aren't snapshotted, and each side gives up after 500 ms (milestone 16). |
| D13 | Local state location | **User state dir**: `~/.local/state/swarmia/<repo-id>/` (macOS: `~/Library/Application Support/swarmia/…`) | Never in `git status`, survives `git clean -fdx`, same for global and per-repo installs. Replaces in-repo `.swarmia/`. `<repo-id>` = hash of the common git dir so worktrees share state. |
| D14 | Sessions with no commits | **Sent when they end** (revised from "after a quiet period") | Every send is the session's full payload and the server keeps the latest per session, so there's nothing to batch up: sessions go when they end (or when they change, at most every 15 min while running), and commit attribution follows as replacements when commits are pushed. |

## Data flow

```
agent hooks ──► log-hook ──► <state>/events.jsonl            (raw, local only)
   │  (Bash Pre/Post) └────► <state>/snapshots/               (working-tree fingerprints)
   │
   └─(lifecycle hook) spawns detached worker
                     ┌──────────────┴───────────────┐
        extract AI changes                    lazy scan: git log --all
        (Edit/Write payloads +                 + reflog since oldest
         Bash snapshot diffs)                  pending session
                     └──────► local matcher ◄───────┘
                                   │  per-line classes (exact/normalized/modified)
                     pending store (sessions, hunks, matches; 30-day retention)
                                   │  only commits reachable from a remote-tracking ref
                                   ▼
                       redact (D10) → HTTPS POST (batched, idempotent, retried)
```

### Capture

- `log-hook` appends raw hook payloads (exists for Claude Code).
- **New:** record `HEAD` sha and branch at SessionStart and on each edit, so
  hunks know their base.
- **New (D12):** Bash Pre/PostToolUse working-tree fingerprints.
- Per-agent normalizers map payloads into one internal event shape (D9).

### Local matching

- AI changes = Edit/Write/MultiEdit hunks + Bash snapshot diffs.
- Candidate commits: after the session's first edit, touching the file, any local
  branch, plus reflog for rewritten history.
- Locate each hunk with a cascade (exact → normalized → 5-token-shingle
  containment ≥ 0.5 to *locate*), then line-diff the located region to classify
  each committed line (D6, D7).
- Track `ai_lines_generated` vs `ai_lines_committed` per session.
- **Pushed-only rule:** report only commits reachable from a remote-tracking
  branch. Unpushed commits that get rewritten are re-matched against the new
  commits.
- Ephemeral/cloud agents: also match at Stop/SessionEnd against commits made in
  the session.
- `matcher_version` on every result; raw local data kept 30 days so a fixed
  matcher can re-match recent history.

### Delivery

- Detached background worker spawned from lifecycle hooks.
- Batched JSON over HTTPS; idempotency key = session id + commit sha + path;
  exponential backoff; pending data persists until acked.
- Sends only when a token resolves (D4).

### Payload (draft)

```json
{
  "session_id": "…", "agent": "claude-code", "model": "…",
  "matcher_version": "1.0.0", "cli_version": "…",
  "repo": "github.com/acme/app",
  "author_email": "…",
  "started_at": "…", "ended_at": "…",
  "prompts": [{ "at": "…", "text": "…(redacted)…" }],
  "prompts_withheld": false,
  "tools": [{ "at": "…", "name": "Bash", "file": null, "command": "npm test" }],
  "commits": [{
    "sha": "<pushed sha>", "path": "src/foo.ts",
    "lines": [
      { "start": 40, "end": 58, "class": "exact" },
      { "start": 59, "end": 61, "class": "modified" },
      { "start": 62, "end": 72, "class": "normalized" }
    ]
  }],
  "ai_lines_generated": 180,
  "ai_lines_committed": 142
}
```

Tool *outputs* (Read/Bash/Grep results) are never sent.

### Server side (monorepo; listed for the contract)

- Ingest endpoint keyed by org token; validate repo ∈ org; rate-limit.
- Join line attribution onto commits already ingested via webhooks.
- Propagate ranges from PR commits to squash/merge commits using diff hunk
  headers from the GitHub API (no file contents stored).
- Discount results from known-bad `matcher_version`s.

## Current state of this repo vs. the plan

| Piece | State |
|---|---|
| `install` (Claude Code, per-repo) | ✅ done; needs global mode (D2) |
| `log-hook` raw capture | ✅ done; writes to state dir, records HEAD/branch |
| npm packaging, CI | ✅ done |
| `process-logs` → `refs/ai-sessions/*` | ✅ deleted (D8); install strips stale hooks |
| State dir + repo id | ✅ done (`swarmia-cli state-dir`) |
| HEAD/branch capture | ✅ done |
| Bash snapshots | ✅ done (`internal/snapshot`; `changes` on the finishing event's record) |
| Lock abstraction (Windows-ready) | ✅ done; Windows compiles in CI, untested, not shipped |
| Matcher (locate + line classes) | ✅ done (`internal/attribution`, v0.1.0) |
| Lazy commit scan + pushed-only rule | ✅ done (pushed flag per commit; delivery filters) |
| Pending store + retention | ✅ done (`sessions/` store, 30-day retention) |
| Background worker | 🟡 basic: detached `process --background` on SessionEnd / log ≥ 8 MB, single-run lock; delivery will hook in here |
| Config (repo + user), token resolution, prompt opt-out | ✅ done (`internal/config`, `swarmia-cli config`) |
| Secret redaction | ✅ done (`internal/redact`; payload builder in `internal/payload`) |
| HTTPS sender | ✅ done (`internal/deliver`, `swarmia-cli send`; API in `docs/ingest-api.md`) |
| Delivery status check | ❌ needs the planned app.swarmia.com endpoint (D17) |
| Other agents | ❌ |

## Milestones

1. ✅ **Foundation** (done 2026-10-06): delete `process-logs`; state dir (D13); HEAD/branch capture;
   lock/path abstractions (D11).
2. ✅ **Matcher** (done 2026-10-06): hunk extraction, line classification;
   golden tests from real sessions incl. a formatter case. See "Matcher notes".
3. ✅ **Bash snapshots** (done 2026-10-06): ≈70 ms per hook in the monorepo
   (one `git status`); see "Findings from milestone 3".
4. ✅ **Local report** (done 2026-10-06): `swarmia-cli report` (+ `--json` draft
   payload). Also pulled forward from M8: `install --scope local|user` for
   personal setups (dogfooding in the monorepo).
5. ✅ **Session store, retention, rewrite handling** (done 2026-10-06): see
   "Findings from milestone 5".
6. ✅ **Config + redaction** (done 2026-10-06): see D10, D15, D16 and
   "Findings from milestone 6".
7. ✅ **Delivery** (done 2026-10-06, client side): full-payload upserts to
   `POST <endpoint>/sessions`, `2xx` = received (D17), backoff. Server: `hook`
   (asynchronous), per `docs/ingest-api.md`.
7b. **Delivery status:** once app.swarmia.com offers the status endpoint (D17),
   show per-session stored/dropped state in `report` and token problems in
   `config`.
8. **Global install** (D2) + MDM docs.
9. **More agents:** Codex (on its branch, see "Codex support"), then Cursor,
   Copilot CLI, Gemini CLI.

15. 🟡 **End-to-end suite** ([docs/e2e.md](e2e.md)): a real Claude Code release
    (`latest` and `stable`, installed from npm) does a task in `unjs/ufo` (MIT,
    TypeScript, prettier) with our hooks, in a fully isolated environment;
    checks capture completeness, usage, attribution against an oracle built
    from the transcript, and payload privacy. Phase 1 done 2026-10-07 and
    passing in CI: workload identity federation works for this repo as
    configured for the monorepo (no rule changes needed). First green run:
    5/5 tool calls captured, precision and recall 1.000 on 30 agent lines,
    planted secret redacted, $0.05 per run. The first run found two product
    bugs, both fixed: local `origin` paths leaked into `repo`, and headless
    sessions had no model. Phase 2 (done 2026-10-07): the full scenario —
    single quotes + prettier, a hand tweak, partial commits, a `sed` rename by a
    second session, a teammate rebase, squash, amend, and a resumed session.
    It found a false attribution (a commit rebased after a later session's
    edit credited to that session as `modified`), fixed in matcher 0.2.0 by
    hunks carrying the lines their edit replaced. Now 1.000 precision, recall
    and class agreement for both sessions; ~$0.23 per run.
    Watch: Claude Code's docs say `--bare` "will become the default for `-p`";
    bare mode skips hooks, so headless sessions would stop reaching us — the
    nightly `latest` run is what would catch it.
16. 🟡 **Snapshot cost** (cheap fixes done 2026-10-08). Measured in the
    monorepo (14k tracked files, 250k ignored): `git status` ≈ 60 ms per
    snapshot, two per Bash call; about two-thirds of it is the untracked-file
    scan (tracked files only: 22 ms). Non-Bash hooks cost ≈ 8 ms, the same as
    starting the binary.
    - ✅ `git status --no-optional-locks`: plain status refreshes the index
      under `index.lock`, which can fail a git command the agent runs in a
      parallel tool call.
    - ✅ **Read-only commands skip both snapshots** (`snapshot.ReadOnly`):
      a quote-aware lexer plus an allowlist (`sed -n N,Mp`, `grep`, `cat`,
      `git log/diff/show/status`, `gh … view/list`, …), rejecting command
      substitution, subshells, here-documents, background jobs and
      redirections other than to `/dev/null`. 213 of 680 recorded Bash calls
      (31%); such a call now costs 31 ms instead of 134 ms in the monorepo.
      A misclassified write loses its edits to unknown, never misattributes.
    - ✅ **Budget:** each side of a command gets 500 ms (git is killed
      past it); the command's changes then go unrecorded. Records carry
      `snapshot: read-only | over-budget | failed`; sessions count the latter
      two (`shell_unrecorded`) and `report` shows them. Watch these in
      dogfooding before doing more.
    - **Next, if dogfooding shows it matters:** use git's own caches when
      the developer has them on — with `core.untrackedCache`, run status
      with `--untracked-files=normal` (60 → 25 ms in the monorepo; `all`
      bypasses the cache) and list new untracked directories ourselves; with
      `core.fsmonitor` status gets cheaper on its own. Recommend both for
      large repos in the README; don't set them in anyone's git config. On a
      synthetic repo (60k tracked, 100k ignored files) fsmonitor plus the
      untracked cache took status from 89 to 30 ms, but only once the
      index had been written with them, which `--no-optional-locks` avoids,
      and git leaves a daemon running per repository (macOS and Windows
      only).
    - **Later, maybe: a per-repo daemon** (would revise D5's "no daemon").
      Only change notifications beat a full scan (FSEvents on macOS — needs
      cgo, fine with the per-platform release runners; inotify on Linux;
      not Go's portable watcher, which holds a descriptor per file on
      macOS), making a snapshot cost what changed rather than the repo's
      size: ≈ 70 → ≈ 10 ms per Bash hook (process start is the floor).
      Timestamped change events would also help milestone 12. Constraints
      if built: a cache only — hooks fall back to `git status` when it is
      missing, slow (≈ 20 ms connect timeout), restarted or blocked by a
      sandbox, so no state lives only in it; started detached by a hook,
      one per repository (lock + socket in the state dir, mode 0700), exits
      after ~30 idle minutes and when its executable changes; socket path
      includes the protocol version so mixed versions (worktrees, user +
      project installs) never talk across versions. Weigh against
      perception: a background process watching every file change, from an
      employer's tool, gets noticed.

Added after reviewing git-ai (2026-10-06; see "Lessons from git-ai"), in
suggested order:

10. ✅ **Token usage per session** (done 2026-10-06; `internal/usage`, payload
    `usage`, docs in `docs/ingest-api.md`). Tokens only — cost is priced
    server-side. Matches an independent count over this repo's own transcript
    exactly. Sessions folded before this change have no transcript path and
    show no usage. Original plan: Hooks carry no token counts, but every
    Claude Code payload has `transcript_path`. At session end (and
    incrementally), read the agent's transcript and sum input/output/cache
    tokens and cost per model; add them to the payload. Covers use case #6
    (cost per commit/PR). git-ai ports its parsing and deduplication from
    ccusage (MIT) for Claude Code and Codex. Payload fields are additive (no
    schema bump); tell the `hook` consumer.
11. ✅ **Reflog-based commit candidates** (done 2026-10-06): candidates are
    reachable commits that are authored with `user.email` **or** created on
    this machine per the HEAD reflogs of all worktrees (commit, amend, rebase /
    pull --rebase picks, cherry-pick, revert, am). Union rather than
    replacement: reflog-only would drop rebased commits' attribution, and
    amended-away commits stay in the reflog (reachability filters them).
    Monorepo report: 0.24 s. Original plan: Identify commits created on this
    machine from the worktree's HEAD reflog (`commit:`, `commit (initial):`,
    `commit (merge):` entries, corroborated by the branch reflog) instead of
    relying only on `user.email`. Pulled, rebased and cherry-picked commits never
    produce those entries; people with several emails are covered. Keep the
    author filter as the fallback when the reflog has expired or is missing.
12. **Shell edit fallback.** Snapshots miss commands that run in another repo or
    worktree, files with coarse mtimes, and runs where Pre/Post didn't pair
    up — git-ai saw the same in real usage. (a) Snapshot the repository of the
    command's working directory, not only the project's. (b) Record every Bash
    call's time window durably; when attributing a commit, give still-
    unattributed lines in a file whose mtime/ctime falls within ±3 s of a
    window to that session, tagged so it can be measured separately. Validate
    against dogfood data ("via shell" counts, unattributed lines) first.
13. ✅ **Attribution fuzzer** (done 2026-10-06; `internal/attribution/fuzz_test.go`):
    matcher-level scenarios from this repo's own files; CI profiles "light"
    (≤ 6 changes, precision ≥ 0.995, recall ≥ 0.98) and "heavy" (≤ 30,
    ≥ 0.99 / ≥ 0.90), plus native `FuzzMatchHunk` invariants; and a
    pipeline fuzzer (`internal/report/fuzz_test.go`) running whole scenarios
    through real git repos and `Build`. See "Findings from milestone 13".
    Original plan: A randomized harness that simulates agent edits,
    human edits, formatter runs, shell codegen, amends, rebases and squashes
    against real repositories, and measures matcher precision and recall per
    class. Complements golden cases and dogfood data; runs in CI on a fixed
    seed and longer locally. git-ai's `docs/attribution-fuzzer-spec.md` is a
    reference.
14. ✅ **"Unknown", not "human"** (done 2026-10-06; `docs/ingest-api.md`):
    unlisted lines are documented as unknown in CommitAttribution/LineRange:
    AI share per commit or PR is attributed lines by class vs unknown, never
    human; commits with no recorded sessions are unknown. README notes it too. Still possible later:
    "known human" evidence from IDE extensions. Original plan: Lines the CLI can't attribute are *unknown*:
    we have no evidence a human wrote them. Make that explicit in the payload
    docs and in how Swarmia computes AI% (the denominator is attributed +
    unknown lines, and unknown must not be reported as human). Later, optionally:
    "known human" evidence from IDE extensions reporting saves, as git-ai does.

## Matcher notes (from milestone 2)

- **Alignment instead of the cascade.** One line-level Smith-Waterman local
  alignment, using D7's normalization for line equality, replaces exact →
  normalized → shingle. It gives per-line classes directly (D6) and tolerates
  human insertions, moved blocks (iterates up to 8 pieces) and formatter line
  wrapping/joining (up to 4 lines). Acceptance: ≥ 2 substantive lines matched,
  including a surviving added line — or a modified added line pinned by context.
- **Hunks are rebuilt, not read from `structuredPatch`.** Claude Code's
  structuredPatch renders tabs as two spaces; hunks are diffed from
  `originalFile` + tool input instead, with 3 lines of context.
- **Combining hunks (for M5):** the same committed line can be claimed by several
  hunks of a session (an AI line later rewritten by the AI). Take the best class
  per line (exact > normalized > modified). `ai_lines_generated` should come
  from the session's net diff, not the sum of hunks, or overwritten lines count
  twice.
- **Known limitation:** boilerplate (Go `package`/imports, near-identical
  `package.json`s) matches across files. Harmless while matching is same-path
  only; matters if a human rewrites an AI file except its boilerplate, or if
  rename-following is added.
- Real-data check (46 edits from July sessions): all edits that landed matched,
  with every non-100% survival explained by later edits in the same session.

## Findings from milestone 4

- **Bash commands can contain source code.** Agents edit files via heredocs and
  scripts; the draft payload's `tools[].command` would carry that code verbatim,
  breaking principle 3. M6 needs a policy for tool inputs, not just secret
  scanning (e.g. command name + hash, or strip heredoc/inline script bodies).
- **Attribution rule:** a commit gets only lines it introduced vs. its first
  parent; merges skipped; candidates limited to the user's `user.email` (keeps
  the monorepo scan cheap: ~0.04 s for a week across 4,460 refs).
- **Renames lose authorship:** lines moved to a new path (e.g. `git mv` + edit)
  count only the edit. Rename-following is a later improvement.
- **Log growth:** ~700 KB per 20 minutes of active session (Write payloads carry
  whole files). `report` re-reads everything. Retention/compaction (M5) is
  needed within a couple of weeks of dogfooding.
- **Edits via Bash show up as `modified`** (seen on this repo's own history) —
  concrete evidence for D12.

## Findings from milestone 3

- **Failed Bash commands never fire PostToolUse** — they fire
  `PostToolUseFailure` (seen in recorded sessions: Pre without Post for every
  failing command). install now hooks it; report reads both.
- **Authored-line rule** for snapshot diffs: an added line counts only while its
  normalized text occurs more often after the command than in the file before
  it, HEAD before, or HEAD after. Excludes formatter runs over human code and
  git stash/checkout/pull restores; keeps genuinely new generic lines.
- **Known imprecision:** human edits saved while a command runs, and overlapping
  parallel Bash calls (each sees the other's changes), are attributed to the
  agent. Files > 1 MB and binary files are skipped.
- **Latency:** 35 ms per Pre+Post pair here, 139 ms in the monorepo, dominated by
  `git status`. Option later: use fsmonitor when the repo has it configured.
  See milestone 16 for what was done about it.

## Findings from milestone 5

- **Compaction:** folding the raw log into per-session records drops whole-file
  payloads: this repo's 2.0 MB log became 312 KB. `report` with cached
  attribution: 0.07 s.
- **Rewrite handling falls out of recomputation:** candidates are commits
  reachable from refs, so amended/rebased commits disappear and replacements
  match. Attribution is cached per session by (matcher version, edit count,
  candidate SHAs); pushed flags refresh every run.
- **Dedup:** identical records within a batch are folded once (fixes double
  logging from overlapping hooks). Not across batches: a crash between saving
  sessions and truncating the log would fold that batch twice.
- **Worker:** `log-hook` launches `process --background` detached (setsid) on
  SessionEnd or when the log reaches 8 MB; a lock file keeps it single-run.
  Sessions that never end are still folded via the size trigger.

## Findings from milestone 6

- On this session's real payload: 85 heredoc bodies, 2 inline scripts and 25
  long strings stripped; no command retained code. The redactor's only hit in
  133 real prompts/commands was a Claude tool-use id (now exempt).
- **Local paths leaked** (19 mentions of the home directory, plus the user
  name encoded in Claude's scratchpad paths) until masking was added.
- Prompts can still contain pasted code; redaction can't tell.

## Findings from milestone 13

Precision holds as post-edit changes pile up; recall degrades gracefully,
mostly for `modified` lines (which need surrounding lines to anchor them).
1,000 trials, seed 1:

| changes per trial (max) | precision | recall | exact | normalized | modified |
|---|---|---|---|---|---|
| 6 | 1.000 | 0.993 | 0.996 | 0.987 | 0.976 |
| 15 | 0.999 | 0.966 | 0.987 | 0.949 | 0.870 |
| 30 | 0.998 | 0.921 | 0.977 | 0.890 | 0.755 |
| 60 | 0.996 | 0.795 | 0.924 | 0.759 | 0.549 |

(Class columns: share of those lines attributed at all.) The remaining false
positives are boilerplate repeated across a file (`if err != nil {`,
`return nil`, import lines) matched in the wrong place. Seeds 1–3 agree within
±0.002 on precision and recall. Native fuzzing (6.4M executions) found no
invariant violations. Recall for heavily edited code is the tuning target once
dogfood data shows how much editing real sessions see.

**Pipeline fuzzer** (git level): agent edits via the edit tool and via shell
snapshots, human edits and formatting, commits (all or one file), amends,
squashes, and rebases onto a teammate's commits fetched from their clone; truth
is the lines git says each reachable commit introduced, with origins from a
per-commit snapshot of a working-tree model. Results: precision 1.000, recall
0.999 at the CI defaults (10 trials × 12 steps) and at seeds 2–3 with
40 trials × 30 steps (~4,500 agent lines, ~100 amends, ~120 rebases, ~35 squashes
per seed). Teammate lines, lines a commit didn't introduce, and unreachable
commits were never attributed; the only false positives were boilerplate
(`if err != nil {`). Its first run reported precision 0.915 — entirely the
generator's own bookkeeping (re-deriving the diff in the test; agent blocks
duplicating the edited file's lines), fixed by using the matcher's diff
(`attribution.AddedFlags`). CI thresholds: precision ≥ 0.99, recall ≥ 0.97.
Not covered: merges, cherry-picks, and squash merges done on the server (the
server's job).

## Lessons from git-ai (reviewed 2026-10-06)

git-ai (`../git-ai`, Rust, v1.7.6) solves the same problem with opposite
choices on two fronts; what it does well became milestones 10–14 and a D9 note.

- **Evidence only, never similarity.** Agents call `git-ai checkpoint` when
  they write files (including via Bash); a line is attributed only on that
  evidence, carried across rewrites by git's own tree diffs. No false
  positives from look-alike lines — but formatter output and anything the
  checkpoints missed becomes "unknown". We match content after the fact, which
  survives formatters (`normalized`) at a precision risk we should measure
  (milestone 13).
- **Exact history via trace2, no hooks or wrapper.** A daemon receives git's
  trace2 events over a socket and uses reflog cursors to learn exactly which
  commits each commit/rebase/cherry-pick produced, then migrates attribution
  notes. Accurate, but costly: its spec lists several failed approaches and
  needs a fuzzer to stay correct. We keep recompute-against-reachable-commits
  plus server-side squash propagation; revisit if dogfood data shows
  rebase/cherry-pick misattribution.
- **Storage:** git notes (`refs/notes/ai`) for attribution, readable by anyone
  with repo access; prompts in a local SQLite (or uploaded for paying teams).
  We already chose HTTPS delivery and no git storage.
- **Metrics agree with ours:** `ai_additions` (committed with AI attribution)
  and `ai_accepted` (accepted without human edits) ≈ our exact+normalized vs
  modified.
- **Known human:** IDE extensions (VS Code, JetBrains, + others) report saved
  files, so lines are AI / known human / unknown (milestone 14).

## Codex support (branch `otto/codex`)

How Codex differs from Claude Code, as far as swarmia-cli is concerned
(verified against codex-cli 0.161.0 where marked ✓; the rest from git-ai's and
entire-cli's integrations, to be confirmed by the first end-to-end runs):

- ✓ **Hooks** live in `.codex/hooks.json` (project) or `~/.codex/hooks.json`
  (user, or `$CODEX_HOME`); there is no per-checkout local file.
- ✓ **Hooks run in the session's working directory** (a subdirectory, if
  Codex was started there), with no project-directory variable like
  `CLAUDE_PROJECT_DIR`. Project hooks therefore walk up to the repository's
  `swarmia-cli` in `sh` (0.5 ms; `git rev-parse` costs ~10 ms per hook).
- ✓ **Trust:** Codex silently skips hooks without a `trusted_hash` in the
  user's `config.toml`, keyed by the hooks file's real path, the event and the
  handler's position; the hash covers the handler's definition. Our hashes
  match Codex's own (`codex app-server` `hooks/list` reports `currentHash` and
  `trustStatus`, a model-free oracle the e2e suite now checks in setup).
- ✓ **Project trust:** Codex reads a project's `.codex/` at all only once the
  project folder is trusted (`[projects."<path>"] trust_level`), which it asks
  about on first use; `codex exec --dangerously-bypass-approvals-and-sandbox`
  records it itself.
- ✓ **Payloads** carry `session_id`, `transcript_path`, `cwd` and (except
  `SessionEnd`) `model`.
- ✓ **`SessionEnd`** fires in `codex exec`; the detached processing run it
  starts survives Codex's teardown (3 s cap, then the hook's process tree is
  killed).
- ✓ **Auth for `codex exec`:** it sends `CODEX_API_KEY`, not
  `OPENAI_API_KEY`.
- **Edits** arrive as `apply_patch` (`tool_input.command` holds the patch
  text). `PreToolUse` now fires for all tools (it used to be shell-only).
- **Transcripts** are "rollout" JSONL with cumulative `token_count` events and
  the model in `turn_context`.
- **No hooks fire in read-only mode.**

Done on the branch:

1. ✅ **Install:** a Codex agent for `project` and `user` scope (`local`
   explains it isn't available); hooks run `log-hook --agent codex` with a
   30 s timeout (`SessionEnd`: 3 s); `install` prints how to approve them with
   `/hooks`, `install --trust` records the trust in `config.toml` (a textual
   edit that keeps the rest of the file, verified to parse before replacing
   it); `config` warns about untrusted hooks.
2. ✅ **Capture and attribution:** the agent is recorded on each event;
   `apply_patch` chunks become hunks (before/after diffed, so replaced lines
   are kept; paths resolved against `cwd`); edit extraction dispatches by
   agent; payload `agent: "codex"`.
3. ✅ **Usage:** per-response differences of the rollout's cumulative totals,
   per model; cached input split out of input, reasoning reported as part of
   output (`reasoning_output_tokens`). Subagents later.
4. ✅ **End-to-end matrix** (nightly, agent × channel), one scenario and oracle
   for both agents:

   | Agent | Development | Stable |
   |---|---|---|
   | Claude Code | `latest` | `stable` |
   | Codex | `alpha` | `latest` |

5. ✅ **Docs:** README (agents, scopes, trust), e2e.md, ingest-api.md.

Still to verify with the first real Codex runs (CI): `apply_patch`'s payload
shape and tool name in `PostToolUse`; the shell tool's name (we expect
`Bash`) and whether shell snapshots work for failing commands (Codex has no
`PostToolUseFailure`); whether `exec resume` appends to the same rollout (the
usage tracker assumes so: a new file replaying old totals would double-count,
and the usage check would catch it).

**CI credentials (decided):** an `OPENAI_API_KEY` secret in the `e2e`
environment, from a dedicated OpenAI project with a spend cap and
inference-only permissions. While the branch is in progress, `otto/codex` is
temporarily allowed in the `e2e` environment's deployment branches. Leak
safeguards for a public repository: fork PRs never get secrets and the
workflow runs only on schedule/dispatch; only Codex jobs get the key, and the
suite passes it to the agent process alone; artifacts are **scrubbed** of
every credential value as they're written (they're public and not masked by
GitHub), and a found value fails the run; a workflow step checks the artifacts
for the key again, independently, and withholds them if it's there.

## Before publishing the npm package

Packaging works (pnpm 10 isolated layout verified with packed tarballs), but:

- ✅ **Launcher latency** (fixed 2026-10-07): the Node launcher added ~85 ms per
  hook. Now a POSIX `sh` launcher that forks nothing on its common path: 14 ms
  per hook via npm or pnpm vs 8 ms for the binary directly (was 91 ms).
  Project-scope hooks call the package's launcher, bypassing pnpm's `.bin`
  shim (28 → 14 ms). Verified with packed tarballs: pnpm isolated, npm local,
  npm global, and a path containing a space.
- ✅ **Missing `node_modules`** (fixed 2026-10-07): project-scope hooks
  failed on every tool call in a fresh clone or new worktree before
  `pnpm install`. Claude Code hooks now check for the binary first
  (`[ ! -x … ] || exec …`, no fork), as Codex hooks already did, and
  `install` rewrites hooks in the old form.
- ✅ **Personal installs from npm** (fixed 2026-10-07): `install --scope
  local|user` pointed at the running binary's real path, which under pnpm is
  versioned (`node_modules/.pnpm/@swarmia+cli-darwin-arm64@0.1.0/…`), so an
  upgrade broke the hooks. Run from the repository's npm dependency, they now
  point at its launcher. Global npm/pnpm installs still record the binary's
  path (pnpm's global store is versioned too).
- ✅ **Release workflow** (2026-10-07): a `v*` tag on main builds the
  packages versioned from the tag, smoke-tests them with npm and pnpm on all
  four platforms, publishes to npm with provenance after approval (trusted
  publishing), and creates a GitHub release. See RELEASING.md.
- ✅ **Decisions:** public npmjs, MIT.

## Still open

- Order among the four next agents.
- Whether snapshot cost justifies a per-repo daemon (milestone 16).
- Retention window: 30 days implemented; revisit with dogfood data.
- Which remote counts as "pushed" with multiple remotes / forks (proposal: any
  remote whose URL maps to the token's org).
- Tell the `hook`/consumer side about the additive `usage` field (it passes
  unknown fields through untouched).
