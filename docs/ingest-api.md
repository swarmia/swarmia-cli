# Swarmia Agent CLI — Ingest API (v1)

This is the contract between `swarmia-cli` (the client, this repository) and
Swarmia's ingest endpoint. It describes exactly what the client sends, and what
it expects back.

**Processing is asynchronous.** The endpoint checks each request's shape,
queues every session for processing, and answers with a result per session.
Resolving the token to an organization, checking the repository and storing the
data happen afterwards. So `accepted` means *queued*: the client learns about
shape problems (`rejected`), but not about sessions Swarmia later drops (for
example, because the repository isn't connected to the token's organization).

- **Base URL:** `https://hook.swarmia.com/cli` (clients can override it with the
  `endpoint` setting, e.g. to test against a local server).
- **One endpoint:** `POST /sessions` → `https://hook.swarmia.com/cli/sessions`.
- **Schema version:** `1` (in the request body).

## What the data is

A coding agent (Claude Code today; Codex, Cursor, Copilot CLI and Gemini CLI
later) works in a developer's git repository. The CLI records the session
locally, works out on the developer's machine which lines of which commits came
from the agent, and sends Swarmia one **session payload** per agent session:

- session metadata: agent, model, start/end time, whether it has ended;
- the developer's **prompts** (secrets redacted; withheld entirely if the
  developer or repository opted out);
- the **tool calls** the agent made: tool name, repository-relative file path,
  and shell commands with code stripped and secrets redacted;
- **line attribution**: for each *pushed* commit that contains lines the agent
  wrote, which line ranges of which files, classified as `exact`, `normalized`
  or `modified`;
- two counts: how many lines the agent wrote, and how many of them landed in
  pushed commits unchanged or only reformatted;
- **token usage** per model, read from the agent's transcript (no cost: the
  server prices it).

**No source code is sent.** Attribution is line numbers only. Tool output
(file contents, command output) is never included. Shell commands lose heredoc
bodies, inline interpreter scripts and long quoted strings. The one exception
the client can't prevent: a developer may paste code into a prompt.

## Delivery semantics (read this first)

- **Full replacement, not deltas.** Every send of a session carries its complete
  current payload. The server keeps the **latest version per session** (upsert)
  and replaces the previous one wholesale.
- **The same session is sent many times.** It is resent whenever its payload
  changes: the session continued, a commit containing its lines was pushed, a
  commit was rebased/amended and re-attributed, or the CLI was upgraded (the
  `cli_version` / `matcher_version` change). Expect a session to arrive a
  handful of times over hours or days; most sessions are first sent when they
  end and again when their commits are pushed. While a session is running it is
  resent at most every 15 minutes.
- **Retries may duplicate.** If a request times out after the server processed
  it, the client sends the same payload again. Upserts make this harmless.
- **Ordering.** Use the request's `sent_at` to drop stale writes: if the stored
  version of a session came from a request with a later `sent_at`, ignore the
  incoming one (still answer `accepted`).
- **Batching.** One request carries 1–25 sessions and at most ~4 MB of JSON
  before compression. Sessions in a batch are independent: the server answers
  for each one separately and never fails the batch as a whole because of one
  session.
- **Sessions belong to one repository.** A session's `repo` is the repository the
  agent worked in. (One agent session in one repository maps to one payload.)

## Request

```
POST /cli/sessions HTTP/1.1
Host: hook.swarmia.com
Authorization: Bearer <token>
Content-Type: application/json
Content-Encoding: gzip
User-Agent: swarmia-cli/<cli version>

<gzip-compressed JSON body>
```

The body is always gzip-compressed. The server must decompress it (and should
cap the decompressed size, see Limits).

### Body

```json
{
  "schema_version": 1,
  "cli_version": "0.4.0",
  "sent_at": "2026-10-06T21:59:12Z",
  "sessions": [ <SessionPayload>, ... ]
}
```

| Field | Type | Description |
|---|---|---|
| `schema_version` | integer | Always `1` for this version of the contract. |
| `cli_version` | string | The client's version (e.g. `0.4.0`, or a git describe string like `5141f98-dirty` for development builds). |
| `sent_at` | string, RFC 3339 UTC | When the client built this request. Use it to order versions of a session. |
| `sessions` | array of SessionPayload, 1–25 items | The sessions in this batch. |

### SessionPayload

```json
{
  "session_id": "7999d991-42b2-47e9-932e-224855021364",
  "agent": "claude-code",
  "model": "claude-opus-4-8[1m]",
  "matcher_version": "0.1.0",
  "cli_version": "0.4.0",
  "repo": "github.com/acme/app",
  "author_email": "dev@acme.com",
  "started_at": "2026-10-06T20:43:00Z",
  "ended_at": "2026-10-06T21:41:57Z",
  "ended": true,
  "prompts": [
    { "at": "2026-10-06T20:43:12Z", "text": "Add retries to the webhook sender. The test key is [REDACTED:stripe-key]." }
  ],
  "prompts_withheld": false,
  "tools": [
    { "at": "2026-10-06T20:44:01Z", "name": "Read", "file": "src/webhooks/sender.ts" },
    { "at": "2026-10-06T20:45:30Z", "name": "Edit", "file": "src/webhooks/sender.ts" },
    { "at": "2026-10-06T20:46:10Z", "name": "Bash", "command": "cat > src/webhooks/retry.ts <<'EOF'\n[24 lines omitted]\nEOF" },
    { "at": "2026-10-06T20:46:55Z", "name": "Bash", "command": "pnpm test src/webhooks" }
  ],
  "commits": [
    {
      "sha": "59e69c61608f8b15237770925b6daafd7496f7eb",
      "path": "src/webhooks/sender.ts",
      "lines": [
        { "start": 40, "end": 58, "class": "exact" },
        { "start": 59, "end": 61, "class": "modified" },
        { "start": 62, "end": 72, "class": "normalized" }
      ]
    },
    {
      "sha": "59e69c61608f8b15237770925b6daafd7496f7eb",
      "path": "src/webhooks/retry.ts",
      "lines": [ { "start": 1, "end": 24, "class": "exact" } ]
    }
  ],
  "ai_lines_generated": 61,
  "ai_lines_committed": 54,
  "usage": [
    {
      "model": "claude-opus-4-8", "speed": "standard",
      "requests": 277, "input_tokens": 554, "output_tokens": 408901,
      "cache_read_input_tokens": 99049271,
      "cache_creation_5m_input_tokens": 0, "cache_creation_1h_input_tokens": 1246914
    },
    {
      "model": "claude-fable-5-1", "speed": "standard", "subagent": true,
      "requests": 12, "input_tokens": 40, "output_tokens": 9120,
      "cache_read_input_tokens": 310442,
      "cache_creation_5m_input_tokens": 0, "cache_creation_1h_input_tokens": 28110,
      "web_fetch_requests": 2
    }
  ]
}
```

| Field | Type | Description |
|---|---|---|
| `session_id` | string | The agent's session id (a UUID for Claude Code). Unique per agent session; together with the organization (from the token) it identifies the session. Treat it as opaque. |
| `agent` | string | Which coding agent: currently always `claude-code`. Future values: `codex`, `cursor`, `copilot-cli`, `gemini-cli`. Accept unknown values. |
| `model` | string, optional | The model the session started with, as the agent reports it (e.g. `claude-opus-4-8[1m]`). Omitted if unknown. |
| `matcher_version` | string (semver) | Version of the client's attribution rules. If a version is later found to attribute wrongly, the server can discount or flag results from it. |
| `cli_version` | string | Client version that built this payload. |
| `repo` | string, optional | The repository as `host/owner/name` (derived from the `origin` remote, e.g. `github.com/acme/app`; GitLab subgroups give `gitlab.com/group/sub/repo`). Empty/omitted if the repository has no `origin` remote. |
| `author_email` | string, optional | The developer's git `user.email` in that repository — the same identity their commits carry. Use it to map the session to a Swarmia identity. |
| `started_at` | string, RFC 3339 UTC | First event of the session. |
| `ended_at` | string, RFC 3339 UTC | Last event seen so far (not necessarily the end — see `ended`). |
| `ended` | boolean | Whether the agent reported the session's end. A later version may flip it back to `false` if the session is resumed. |
| `prompts` | array of Prompt | The developer's prompts in order. Empty when withheld. |
| `prompts_withheld` | boolean | `true` if the developer or the repository configuration opted out of sharing prompts. Then `prompts` is empty — that is not the same as a session without prompts. |
| `tools` | array of Tool | The agent's tool calls, in order (only completed calls, successful or failed). |
| `commits` | array of CommitAttribution | Line attribution, one entry per (commit, file). Only commits that were **pushed** (contained in a remote-tracking branch on the developer's machine) when the payload was built. Empty if nothing has been pushed yet. |
| `ai_lines_generated` | integer | Non-blank lines the agent wrote that were still present after its last edit to each file. Lines it wrote and then replaced itself don't count. Includes lines written through shell commands. Says nothing about human-written lines. |
| `ai_lines_committed` | integer | Of the attributed lines in `commits`, how many are `exact` or `normalized`. The ratio `ai_lines_committed / ai_lines_generated` is the session's "survival" rate, but note the numerator only covers pushed commits. Says nothing about the commits' other lines, which are unknown. |
| `usage` | array of Usage, optional | The session's token usage so far, read from the agent's transcript (including its subagents' transcripts). Empty if unknown; absent from older clients. Cumulative: each send carries the totals to date, replacing the previous ones. |

#### Usage

One bucket per (model, speed, main session vs subagents). The client doesn't
compute cost — prices change and depend on contracts — so the server prices
these counts. Each API response counts once (agents write one transcript line
per content block; the client deduplicates by message and request id).

| Field | Type | Description |
|---|---|---|
| `model` | string | Model id as the agent reports it, e.g. `claude-opus-4-8`. |
| `speed` | string | `standard` or `fast` (fast mode is priced differently). Accept other values. |
| `subagent` | boolean, optional | `true` for calls made by the session's subagents (e.g. Claude Code's Task tool); absent for the main session. |
| `requests` | integer | Model calls (API responses). |
| `input_tokens` | integer | Uncached input tokens. |
| `output_tokens` | integer | Output tokens (including thinking). |
| `cache_read_input_tokens` | integer | Input tokens read from the prompt cache. |
| `cache_creation_5m_input_tokens` | integer | Input tokens written to the 5-minute cache. |
| `cache_creation_1h_input_tokens` | integer | Input tokens written to the 1-hour cache. |
| `web_search_requests` | integer, optional | Server-side web searches. |
| `web_fetch_requests` | integer, optional | Server-side web fetches. |

#### Prompt

| Field | Type | Description |
|---|---|---|
| `at` | string, RFC 3339 UTC | When the prompt was submitted. |
| `text` | string | The prompt, with secrets replaced by `[REDACTED:<kind>]` and local details masked (see Redaction markers). Can be long, and can contain code the developer pasted. |

#### Tool

| Field | Type | Description |
|---|---|---|
| `at` | string, RFC 3339 UTC | When the tool call finished. |
| `name` | string | The agent's tool name, e.g. `Read`, `Edit`, `Write`, `MultiEdit`, `Bash`, `Grep`, `Glob`, `WebFetch`, `Task`, `mcp__<server>__<tool>`. Agent-specific; accept anything. |
| `file` | string, optional | The file the tool acted on, relative to the repository root, `/`-separated. Omitted if the tool has no file or it lies outside the repository. |
| `command` | string, optional | For shell tools: the command, with heredoc bodies replaced by `[N lines omitted]`, inline interpreter scripts (`python -c`, `node -e`, …) by `'[script omitted]'`, quoted strings over 120 characters by `'[N chars omitted]'`, secrets redacted, local paths masked, and capped at ~1000 characters (truncation ends with `…`). |

#### CommitAttribution

| Field | Type | Description |
|---|---|---|
| `sha` | string | Full commit SHA (40 hex characters, or 64 for SHA-256 repositories). The same commit appears once per attributed file. |
| `path` | string | File path within the repository at that commit, `/`-separated. |
| `lines` | array of LineRange, ascending, non-overlapping | The lines of `path` *as of `sha`* that came from this session. |

Only lines that **this commit introduced** (relative to its first parent) are
attributed to it. A later commit that merely carries the agent's lines along is
not listed for them. Merge commits are never listed.

Lines a commit introduced that no session lists are **unknown**, not human:
the CLI has no evidence about who wrote them. A human may have, or an agent
session that wasn't recorded (hooks not installed, another machine, another
agent), or the code moved from another file (renames aren't followed yet), or a
shell edit the snapshots missed.

#### LineRange

| Field | Type | Description |
|---|---|---|
| `start` | integer ≥ 1 | First line, 1-based, in the file as of the commit (the "new side" of the commit's diff). |
| `end` | integer ≥ `start` | Last line, inclusive. |
| `class` | string | `exact` — identical to what the agent wrote (ignoring trailing whitespace). `normalized` — equal once formatting is ignored (whitespace, indentation, quote style, trailing commas/semicolons, line wrapping), i.e. a formatter changed it. `modified` — recognisably the agent's line but edited by a human (at least half its tokens shared). Lines not listed are *unknown*, not human-written (see CommitAttribution). |

Blank lines are never attributed, so ranges break at blank lines.

### Redaction markers

Free text (`prompts[].text`, `tools[].command`) may contain:

- `[REDACTED:<kind>]` — a secret the client removed. Kinds: `swarmia-token`, `github-token`,
  `gitlab-token`, `slack-token`, `slack-webhook`, `aws-access-key`,
  `google-api-key`, `stripe-key`, `anthropic-key`, `openai-key`, `npm-token`,
  `sendgrid-key`, `jwt`, `private-key`, `password`, `secret`, `high-entropy`.
  New kinds may appear.
- `<repo>` — the repository's local root directory.
- `~` — the developer's home directory.
- `<user>` — the developer's local user name.

Display them as-is. Never try to reverse them.

## Response

### 200 OK — per-session results

Once the request passes the whole-request checks below, the server answers
`200` with one result per session it could identify:

```json
{
  "results": [
    { "session_id": "7999d991-42b2-47e9-932e-224855021364", "status": "accepted" },
    { "session_id": "0b3c2f6e-1111-4222-8333-944455556666", "status": "rejected", "error": "repository unknown" }
  ]
}
```

| Field | Type | Description |
|---|---|---|
| `results[].session_id` | string | Echoes the session's id. |
| `results[].status` | string | `accepted` — queued for processing; `rejected` — failed a shape check. |
| `results[].error` | string, only for `rejected` | `invalid payload: <field>` (a required field missing or of the wrong type), `repository unknown` (no `repo`), or `session too large` (over ~9 MiB). Shown to the developer by `swarmia-cli report`. |

Client behaviour:

- `accepted`: marked sent; resent only when the payload changes.
- `rejected`: marked rejected with the reason; **not retried until the payload
  changes** (e.g. after an `origin` remote is added, `repository unknown` goes
  away on the next change).
- **Left out of `results`:** treated as not sent and retried on the next run.

Whether Swarmia later stores or drops an accepted session isn't visible to the
client.

### Errors — the whole request

| Status | When | Client behaviour |
|---|---|---|
| `400` | Body isn't valid gzip or JSON (or not an object), `schema_version` isn't `1`, `cli_version` or `sent_at` is missing or invalid, or `sessions` is empty. | Backs off exponentially (1 min doubling, max 6 h) and retries. |
| `401` | No `Authorization` header, or the token isn't `swm_cli_` followed by 64+ lowercase hex characters. | Pauses delivery for 6 hours. (`swarmia-cli config` and `send` warn about a malformed token up front.) |
| `404` / `405` | Any path other than `/cli/sessions`, or any method other than `POST`. | Backs off exponentially (a misconfigured `endpoint`). |
| `413` | More than 25 sessions. | Splits the batch in half and retries (the client never sends more than 25). |
| `429` | Rate limited. | Waits for `Retry-After` (seconds) if given, else backs off exponentially. |
| `503` | None of the sessions could be queued. | Waits for `Retry-After` if given, else backs off exponentially. |
| other `5xx` | Unexpected failures. | As `503`. |

Error bodies are only logged (first ~200 characters). Network errors and
timeouts (the client waits 30 s) back off like `5xx`. Because the client retries
after timeouts, the same payload can arrive twice; the server's upserts make
that harmless.

## Authentication and tokens

- `Authorization: Bearer <token>`. The token identifies the **organization**.
- **Format:** `swm_cli_` followed by 64 or more lowercase hex characters. The
  CLI redacts `swm_cli_…` strings from prompts and commands.
- **The token is write-only and may be public.** Teams can commit it to a
  repository's `.swarmia.json` so every developer's CLI picks it up. Anyone who
  can read that repository can submit data with it, but it allows nothing else:
  no reads, and only sessions for repositories connected to its organization
  are kept.
- Tokens are also distributed by MDM, user config files or the `SWARMIA_TOKEN`
  environment variable.

## Limits

| Limit | Client guarantee |
|---|---|
| Sessions per request | ≤ 25 (the server answers `413` beyond that). |
| Request size (uncompressed JSON) | ≤ 4 MB, except a single session can exceed it. |
| Session size | Usually < 100 KB; long sessions with many prompts can reach a few MB. The server rejects sessions over ~9 MiB (`session too large`). |
| Request rate | One request per batch per background run; background runs happen at session ends. |

## Versioning

- Additive changes (new optional fields, new `agent` values, new redaction
  kinds, new tool names) don't change `schema_version`; the server ignores
  unknown fields and accepts unknown enum-like values except `class`.
- Breaking changes bump `schema_version`. The server keeps accepting the
  previous version for a transition period, since developers update the CLI at
  their own pace.

## Checking payloads with the CLI

The CLI shows exactly what it would send, without sending anything:

```sh
swarmia-cli report --json            # the session payloads delivery would send
swarmia-cli report --json --session <id>
swarmia-cli send --dry-run           # how many sessions would be sent now
swarmia-cli config                   # resolved token, endpoint and prompt sharing
```

To test a server implementation, point the CLI at it and send:

```sh
SWARMIA_ENDPOINT=http://127.0.0.1:8080/cli SWARMIA_TOKEN=swm_cli_$(openssl rand -hex 32) swarmia-cli send
```
