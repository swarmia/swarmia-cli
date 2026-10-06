# Swarmia Agent CLI — Roadmap

> Working roadmap. Supersedes the Notion RFC
> ("Swarmia Agent CLI: Tool for tracking agentic tool usage"). Feature context and
> use cases: "Link sessions, prompts, and AI-generated code to source code (AI Git
> Blame / Swarmia agent CLI)".

## Summary of the change from the RFC

The RFC stores encrypted, full session logs (including source code) in
`refs/ai-sessions/*` in the customer's git repo, and matches sessions to commits
on Swarmia's servers. This plan instead:

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
and metadata*. The RFC's storage and encryption machinery exists only to protect
code we don't need.

## Principles (revised)

1. Session ↔ commit linking is reliable, including after formatters and light
   human edits. Many-to-many.
2. ~~Locally running code is as dumb as possible.~~ → Local code does capture,
   matching and redaction; everything else (aggregation, propagation through
   squash merges, thresholds, analytics) is server-side. Matching is versioned so
   results from bad versions can be discounted. **Needs sign-off from the RFC
   owner.**
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
| D7 | Formatting-only differences | **Count as AI**, tagged `normalized` | Normalization per the RFC (whitespace, blank/comment lines, trailing `,`/`;`, quote style). |
| D8 | Git-ref storage | **Delete** `process-logs` / `refs/ai-sessions/*` | Remains in git history. |
| D9 | Agents | Claude Code, then **Codex, Cursor, Copilot CLI, Gemini CLI** | Capture layer needs a per-agent payload normalizer (session id, tool names, edit shapes differ). Order within the four: TBD. Reference: git-ai's `agent-support/` and per-agent "presets" normalize hook JSON for 14 agents, including where each puts shell commands (`tool_input.command`, `toolInput.command`, `tool_input.cmd`, `cmd`, …); its docs cover installing in cloud agents (Claude on the web, Codex Cloud, Cursor Agent, Devin), where sandboxes block background sockets. |
| D10 | Secret redaction | **Own layered redactor** (revised from "full scanner") | Embedding gitleaks cost +5 MB and ~200 modules and caught 1 of 4 sample secrets. Instead: provider token formats, private keys, URL/connection-string/`curl -u` passwords, secret-looking assignments, high-entropy strings; applied to prompts and commands. |
| D15 | Shell commands in payload | **Keep structure, strip code** | Heredoc bodies, inline interpreter scripts and quoted strings > 120 chars replaced by placeholders, then redacted; capped at 1000 chars. Repo root, home dir and user name masked in all free text. |
| D16 | Endpoint | **`https://hook.swarmia.com/cli`** default, overridable | Server side lives in the `hook` repo. |
| D17 | Ingest feedback | **Shape checks synchronous, the rest asynchronous** | `hook` (implemented in `src/cli.ts`) validates the request and each session's shape, publishes each session to Pub/Sub, and answers `200` with per-session `accepted` (= queued) / `rejected` (`invalid payload: <field>`, `repository unknown`, `session too large`); `503` if nothing could be queued, `401` for tokens not matching `swm_cli_[0-9a-f]{64,}`. The repo-in-org check, token→org resolution and storage happen downstream and aren't visible to the client. The client also treats a bare `2xx` as "all queued". **Planned:** a synchronous endpoint on `https://app.swarmia.com/` where the CLI, using the same write-only token, can check whether its sessions were received and stored or dropped (and why) — without exposing any session content, since the token may be public. Surfaced in `report`/`config` when it exists. |
| D11 | Windows | **Later, but design for it now** | Abstract file locking (flock) and paths behind small interfaces; keep state dir resolution OS-aware. |
| D12 | Non-Edit/Write changes | **Snapshot around Bash** | Pre/PostToolUse on Bash records a cheap working-tree fingerprint (`git status --porcelain` + hashes of dirty files); diffs between the two are AI-made changes (codegen, sed, agent-run formatters). Budget: tens of ms per Bash call; must stay off the hook's critical path where possible. |
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
- Locate each hunk with the RFC cascade (exact → normalized → 5-token-shingle
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
   repo (asynchronous, Pub/Sub), per `docs/ingest-api.md`.
7b. **Delivery status:** once app.swarmia.com offers the status endpoint (D17),
   show per-session stored/dropped state in `report` and token problems in
   `config`.
8. **Global install** (D2) + MDM docs.
9. **More agents:** Codex, Cursor, Copilot CLI, Gemini CLI.

Added after reviewing git-ai (2026-10-06; see "Lessons from git-ai"), in
suggested order:

10. **Token usage and cost per session.** Hooks carry no token counts, but every
    Claude Code payload has `transcript_path`. At session end (and
    incrementally), read the agent's transcript and sum input/output/cache
    tokens and cost per model; add them to the payload. Covers use case #6
    (cost per commit/PR). git-ai ports its parsing and deduplication from
    ccusage (MIT) for Claude Code and Codex. Payload fields are additive (no
    schema bump); tell the `hook` consumer.
11. **Reflog-based commit candidates.** Identify commits created on this
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
13. **Attribution fuzzer.** A randomized harness that simulates agent edits,
    human edits, formatter runs, shell codegen, amends, rebases and squashes
    against real repositories, and measures matcher precision and recall per
    class. Complements golden cases and dogfood data; runs in CI on a fixed
    seed and longer locally. git-ai's `docs/attribution-fuzzer-spec.md` is a
    reference.
14. **"Unknown", not "human".** Lines the CLI can't attribute are *unknown*:
    we have no evidence a human wrote them. Make that explicit in the payload
    docs and in how Swarmia computes AI% (the denominator is attributed +
    unknown lines, and unknown must not be reported as human). Later, optionally:
    "known human" evidence from IDE extensions reporting saves, as git-ai does.

## Matcher notes (from milestone 2)

- **Alignment instead of the RFC cascade.** One line-level Smith-Waterman local
  alignment, using the RFC's normalization for line equality, replaces exact →
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

## Still open

- RFC owner sign-off on local matching (principle 2) and on dropping git storage.
- Order among the four next agents.
- Quiet period length for no-commit sessions (D14) and retention window (30 days
  proposed).
- Which remote counts as "pushed" with multiple remotes / forks (proposal: any
  remote whose URL maps to the token's org).
- Worker mechanism details: detached child process vs. OS scheduler; lock to
  ensure a single worker per repo.
- Server ingest API shape — to agree with the monorepo team.
