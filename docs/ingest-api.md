# Swarmia Agent CLI — Ingest API (v1)

This is the contract between `swarmia-cli` (the client, this repository) and the
ingest service that receives its data (the `hook` service). It describes exactly
what the client sends today, what it expects back, and what Swarmia is
responsible for doing with it.

**Processing is asynchronous.** `hook` checks each request's shape, publishes
every session as its own message to a Pub/Sub queue, and answers with a result
per session. Resolving the token to an organization, the repository check,
storage and attribution joining happen downstream, after the client has its
answer. So `accepted` means *queued*: the client learns about shape problems
(`rejected`), but not about sessions the consumer later drops. (A way to check
that is planned — see "Future: delivery status".)

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
  pushed commits unchanged or only reformatted.

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
  before compression. Sessions in a batch are independent: `hook` answers for
  each and publishes each as its own message, and the consumer must process
  each on its own, never fail the batch as a whole.
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
  "ai_lines_committed": 54
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
| `ai_lines_generated` | integer | Non-blank lines the agent wrote that were still present after its last edit to each file. Lines it wrote and then replaced itself don't count. Includes lines written through shell commands. |
| `ai_lines_committed` | integer | Of the attributed lines in `commits`, how many are `exact` or `normalized`. The ratio `ai_lines_committed / ai_lines_generated` is the session's "survival" rate, but note the numerator only covers pushed commits. |

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

#### LineRange

| Field | Type | Description |
|---|---|---|
| `start` | integer ≥ 1 | First line, 1-based, in the file as of the commit (the "new side" of the commit's diff). |
| `end` | integer ≥ `start` | Last line, inclusive. |
| `class` | string | `exact` — identical to what the agent wrote (ignoring trailing whitespace). `normalized` — equal once formatting is ignored (whitespace, indentation, quote style, trailing commas/semicolons, line wrapping), i.e. a formatter changed it. `modified` — recognisably the agent's line but edited by a human (at least half its tokens shared). Lines a human wrote from scratch are never listed. |

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

Once the request passes the whole-request checks below, `hook` answers `200`
with one result per session it could identify:

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
| `results[].status` | string | `accepted` — published to the queue; `rejected` — failed a shape check. |
| `results[].error` | string, only for `rejected` | `invalid payload: <field>` (a required field missing or of the wrong type), `repository unknown` (no `repo`), or `session too large` (the session's message exceeds the queue's ~9 MiB message limit). Shown to the developer by `swarmia-cli report`. |

Client behaviour:

- `accepted`: marked sent; resent only when the payload changes.
- `rejected`: marked rejected with the reason; **not retried until the payload
  changes** (e.g. after an `origin` remote is added, `repository unknown` goes
  away on the next change).
- **Left out of `results`:** treated as not sent and retried on the next run.
  `hook` leaves out sessions whose publish failed, and sessions without a
  `session_id` (which the client never sends).

Whether the consumer later stores or drops an accepted session is invisible to
the client.

### Errors — the whole request

| Status | When | Client behaviour |
|---|---|---|
| `400` | Body isn't valid gzip or JSON (or not an object), `schema_version` isn't `1`, `cli_version` or `sent_at` is missing or invalid, or `sessions` is empty. | Backs off exponentially (1 min doubling, max 6 h) and retries. |
| `401` | No `Authorization` header, or the token isn't `swm_cli_` followed by 64+ lowercase hex characters. | Pauses delivery for 6 hours. (`swarmia-cli config` and `send` warn about a malformed token up front.) |
| `404` / `405` | Any path other than `/cli/sessions`, or any method other than `POST`. | Backs off exponentially (a misconfigured `endpoint`). |
| `413` | More than 25 sessions. | Splits the batch in half and retries (the client never sends more than 25). |
| `429` | Rate limited, if added (e.g. by infrastructure in front of `hook`). | Waits for `Retry-After` (seconds) if given, else backs off exponentially. |
| `503` | None of the sessions could be published to the queue. | Waits for `Retry-After` if given, else backs off exponentially. |
| other `5xx` | Unexpected failures. | As `503`. |

Error bodies are only logged (first ~200 characters). Network errors and
timeouts (the client waits 30 s) back off like `5xx`. Because the client retries
after timeouts, the same payload can be queued twice — consumers must be
idempotent (upserts make that free).

## Authentication and tokens

- `Authorization: Bearer <token>`. The token identifies the **organization**.
- **Format:** `swm_cli_` followed by 64 or more lowercase hex characters
  (`hook` answers `401` to anything else). `hook` never forwards the token
  itself: messages carry its SHA-256 digest (`tokenHash`), which the consumer
  resolves to the organization. The CLI redacts `swm_cli_…` strings from
  prompts and commands.
- **The token is write-only and may be public.** Teams can commit it to a
  repository's `.swarmia.json` so every developer's CLI picks it up. Anyone who
  can read that repository — including the whole internet for a public one —
  can submit data with it. So a token must only allow submitting sessions for
  its own organization: no reads, no other endpoints. Expect junk submissions
  and validate accordingly (see Server responsibilities).
- Tokens are also distributed by MDM, user config files or the `SWARMIA_TOKEN`
  environment variable.

## Limits

| Limit | Client guarantee | Server should |
|---|---|---|
| Sessions per request | ≤ 25 | `hook` answers `413` beyond 25. |
| Request size (uncompressed JSON) | ≤ 4 MB, except a single session can exceed it | Decompressed before the handler; guard against decompression bombs. |
| Session size | Usually < 100 KB; long sessions with many prompts can reach a few MB | `hook` rejects a session whose message exceeds ~9 MiB (`session too large`). |
| Request rate | One request per batch per background run; background runs happen at session ends | Rate limit per token; answer `429` with `Retry-After`. |

## Server responsibilities

### `hook` (receiver) — as implemented

1. Serve only `POST /cli/sessions` (others: `404`/`405`); the body arrives
   gzip-decompressed.
2. Check the `Authorization` header carries a token in the CLI format → else
   `401`. The token is replaced by its SHA-256 digest (`tokenHash`) straight
   away; the plaintext never reaches messages or logs.
3. Whole-request checks → `400` / `413` as above.
4. For each session: check required fields and types (`invalid payload:
   <field>`), that `repo` is set (`repository unknown`), and the message size
   (`session too large`); then publish it as its own `cli-ingest/session`
   message carrying `schemaVersion`, `cliVersion`, `sentAt`, `userAgent`,
   `tokenHash` and the session exactly as received (unknown fields included).
5. Answer `200` with per-session results; leave out sessions whose publish
   failed; answer `503` if nothing could be published.
6. Never log prompts: the body is detached from the request before anything can
   throw.

### Consumer (downstream of the queue)

1. **Authenticate** `tokenHash` → organization; drop messages with unknown or
   revoked tokens.
2. **Validate further.** `hook` only checked field presence and types; ignore
   unknown fields (the client may add fields without bumping
   `schema_version`).
3. **Check the repository belongs to the organization.** Match `repo` against
   the organization's connected repositories (GitHub/GitLab integration) and
   drop sessions for others. This is what stops junk sent with a leaked token,
   and keeps developers' personal projects out. Record why sessions were
   dropped, per token — that is what the planned delivery status endpoint would
   report.
4. **Upsert** by (organization, `session_id`), replacing the stored payload,
   unless the stored version came from a later `sent_at`. Duplicate messages are
   expected.
5. **Map the developer**: `author_email` → Swarmia identity, as for commits.
6. **Join attribution with commits.**
   - Commits are pushed, but the webhook for a push may arrive after the session
     payload, and a pushed commit can still be force-pushed away. Store
     attribution for unknown SHAs and resolve it when the commit shows up.
   - Line numbers refer to the file as of that commit.
   - **Squash and rebase merges:** the commits attributed are the developer's
     own (pushed branch) commits. When a PR is squash-merged, the merge commit
     contains the same lines at possibly different positions. Carry ranges
     forward using the diff hunk headers between the PR's commits and the
     merge commit (from the GitHub/GitLab API; file contents aren't needed and
     shouldn't be stored).
   - The same line can be attributed to several sessions (many-to-many).
7. **Discount bad matcher versions:** keep `matcher_version` with the stored
   attribution so results from a version found to be wrong can be excluded or
   recomputed later.
8. **Protect prompts.** Prompts are sensitive: only the session's author, the
   organization's admins and Swarmia should be able to read them. Honour
   `prompts_withheld` (no prompts stored). Free text is untrusted input: escape
   it when rendering.
9. **Never expect code.** If you need code for analysis (e.g. to show an "AI
   blame" view), fetch it from the Git provider at display time using the SHA,
   path and line ranges.

## Future: delivery status

`hook` only answers for shape problems, so the client can't tell whether an
accepted session was stored or dropped downstream (repository not in the
organization, unknown or revoked token). A
separate, synchronous endpoint on `https://app.swarmia.com/` is planned for that:
called with the same write-only token, it would answer — per session id the
client sent, or summarised for the token — whether the session was received and
stored, or dropped and why. It must not return any session content, since the
token may be public. The CLI would surface the answer in `swarmia-cli report`
and `swarmia-cli config`. Not part of v1; the request format above doesn't
depend on it.

## Example exchange

Request (shown decompressed):

```http
POST /cli/sessions HTTP/1.1
Authorization: Bearer swm_cli_3f9a0c…
Content-Type: application/json
Content-Encoding: gzip
User-Agent: swarmia-cli/0.4.0

{
  "schema_version": 1,
  "cli_version": "0.4.0",
  "sent_at": "2026-10-06T22:05:00Z",
  "sessions": [
    {
      "session_id": "e2e5c1d0-5f7a-4c1e-9d2b-3a4b5c6d7e8f",
      "agent": "claude-code",
      "model": "claude-opus-4-8",
      "matcher_version": "0.1.0",
      "cli_version": "0.4.0",
      "repo": "github.com/acme/app",
      "author_email": "dev@acme.com",
      "started_at": "2026-10-06T21:58:40Z",
      "ended_at": "2026-10-06T22:04:51Z",
      "ended": true,
      "prompts": [{ "at": "2026-10-06T21:58:45Z", "text": "add a FromShell helper" }],
      "prompts_withheld": false,
      "tools": [
        { "at": "2026-10-06T21:59:02Z", "name": "Bash", "command": "cat > x.go <<EOF\n[2 lines omitted]\nEOF" }
      ],
      "commits": [
        {
          "sha": "5e96aa6d1b0e4f3c2a1908776655443322110099",
          "path": "x.go",
          "lines": [
            { "start": 1, "end": 1, "class": "exact" },
            { "start": 3, "end": 3, "class": "exact" }
          ]
        }
      ],
      "ai_lines_generated": 2,
      "ai_lines_committed": 2
    }
  ]
}
```

Response:

```http
HTTP/1.1 200 OK
Content-Type: application/json

{ "results": [ { "session_id": "e2e5c1d0-5f7a-4c1e-9d2b-3a4b5c6d7e8f", "status": "accepted" } ] }
```

## JSON Schema

Request body (after decompression):

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://hook.swarmia.com/cli/schemas/sessions-request-v1.json",
  "type": "object",
  "required": ["schema_version", "cli_version", "sent_at", "sessions"],
  "properties": {
    "schema_version": { "const": 1 },
    "cli_version": { "type": "string" },
    "sent_at": { "type": "string", "format": "date-time" },
    "sessions": { "type": "array", "minItems": 1, "items": { "$ref": "#/$defs/session" } }
  },
  "$defs": {
    "session": {
      "type": "object",
      "required": ["session_id", "agent", "matcher_version", "cli_version", "started_at", "ended_at",
                   "ended", "prompts", "prompts_withheld", "tools", "commits",
                   "ai_lines_generated", "ai_lines_committed"],
      "properties": {
        "session_id": { "type": "string", "minLength": 1 },
        "agent": { "type": "string" },
        "model": { "type": "string" },
        "matcher_version": { "type": "string" },
        "cli_version": { "type": "string" },
        "repo": { "type": "string" },
        "author_email": { "type": "string" },
        "started_at": { "type": "string", "format": "date-time" },
        "ended_at": { "type": "string", "format": "date-time" },
        "ended": { "type": "boolean" },
        "prompts": { "type": "array", "items": { "$ref": "#/$defs/prompt" } },
        "prompts_withheld": { "type": "boolean" },
        "tools": { "type": "array", "items": { "$ref": "#/$defs/tool" } },
        "commits": { "type": "array", "items": { "$ref": "#/$defs/commit" } },
        "ai_lines_generated": { "type": "integer", "minimum": 0 },
        "ai_lines_committed": { "type": "integer", "minimum": 0 }
      }
    },
    "prompt": {
      "type": "object",
      "required": ["at", "text"],
      "properties": {
        "at": { "type": "string", "format": "date-time" },
        "text": { "type": "string" }
      }
    },
    "tool": {
      "type": "object",
      "required": ["at", "name"],
      "properties": {
        "at": { "type": "string", "format": "date-time" },
        "name": { "type": "string" },
        "file": { "type": "string" },
        "command": { "type": "string" }
      }
    },
    "commit": {
      "type": "object",
      "required": ["sha", "path", "lines"],
      "properties": {
        "sha": { "type": "string", "pattern": "^[0-9a-f]{40}([0-9a-f]{24})?$" },
        "path": { "type": "string", "minLength": 1 },
        "lines": { "type": "array", "minItems": 1, "items": { "$ref": "#/$defs/range" } }
      }
    },
    "range": {
      "type": "object",
      "required": ["start", "end", "class"],
      "properties": {
        "start": { "type": "integer", "minimum": 1 },
        "end": { "type": "integer", "minimum": 1 },
        "class": { "enum": ["exact", "normalized", "modified"] }
      }
    }
  }
}
```

Response body (200):

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://hook.swarmia.com/cli/schemas/sessions-response-v1.json",
  "type": "object",
  "required": ["results"],
  "properties": {
    "results": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["session_id", "status"],
        "properties": {
          "session_id": { "type": "string" },
          "status": { "enum": ["accepted", "rejected"] },
          "error": { "type": "string" }
        }
      }
    }
  }
}
```

## Versioning

- Additive changes (new optional fields, new `agent` values, new redaction
  kinds, new tool names) don't change `schema_version`; the server must ignore
  unknown fields and accept unknown enum-like values except `class`.
- Breaking changes bump `schema_version`. The server should keep accepting the
  previous version for a transition period, since developers update the CLI at
  their own pace; answer `400` for versions it doesn't support.

## Testing against a local server

Point the CLI at a local implementation and send now:

```sh
SWARMIA_ENDPOINT=http://127.0.0.1:8080/cli SWARMIA_TOKEN=swm_cli_$(openssl rand -hex 32) swarmia-cli send
swarmia-cli report --json   # the exact payloads it would send
```
