# GitHub Copilot

How swarmia-cli captures GitHub Copilot sessions, and what it doesn't cover.
Verified against Copilot CLI 1.0.112 (November 2026); the end-to-end suite
runs Copilot CLI's `latest` and `prerelease` releases (see `docs/e2e.md`).

## What's covered

Copilot CLI, VS Code (the Copilot session target) and the GitHub Copilot app
run the same agent, with the same hooks; JetBrains' ACP chat runs `copilot
--acp`. swarmia-cli supports that agent, as `copilot` in payloads. Only Copilot
CLI is tested end to end.

Not covered: inline completions and next-edit suggestions, which have no
hooks; VS Code's older "Local" agent (deprecated, removed in February 2027);
Visual Studio, Xcode and the JetBrains plugin's own chat; and Copilot cloud
agent (below).

## Hooks

`swarmia-cli install` adds them where a repository is set up for Copilot
(`.github/hooks`, `.github/copilot` or `.github/copilot-instructions.md`;
create `.github/hooks` to opt in):

| Scope | File |
|---|---|
| `project` | `.github/hooks/swarmia.json`, a file of its own among the repository's hooks |
| `local` | the `hooks` of `.github/copilot/settings.local.json` (gitignore it; install says if it isn't) |
| `user` | `~/.copilot/hooks/swarmia.json` (`$COPILOT_HOME/hooks/`) |

Copilot lists hooks under each event directly (no matcher groups), with a
`version` in hooks files. swarmia-cli's run `log-hook --agent copilot` from
the `bash` field, under Claude Code's event names (`PreToolUse`, not
`preToolUse`): so named, Copilot sends Claude-shaped payloads (`session_id`,
`tool_name`, `tool_input`, `tool_use_id`, `transcript_path` on `Stop`) and
reports its tools under Claude Code's names.

Copilot runs a repository's hooks once its folder is trusted
(`trustedFolders` in `~/.copilot/config.json`, which it asks about), and
passes the repository root as `$COPILOT_PROJECT_DIR` (and
`$CLAUDE_PROJECT_DIR`).

**A failing `PreToolUse` hook denies the tool call** (only a timeout lets it
through), so the hook command must never fail. Project hooks look for the
repository's swarmia-cli and quietly do nothing without it (a teammate's
fresh clone; Copilot cloud agent's sandbox); hooks running a binary by path or
from `$PATH` check that it's there first. Only `bash`, the Unix shell's
command, is set, not `powershell` or the cross-platform `command`: on Windows
the hooks should have nothing to run (unverified).

### Claude Code's hooks

Copilot also runs a repository's `.claude/settings.json` and
`.claude/settings.local.json` hooks, sending them its own payloads. So a
repository with swarmia-cli's Claude Code hooks alone already captures
Copilot sessions. `log-hook --agent claude-code` recognizes a Copilot payload
(it has a `timestamp`, which Claude Code's don't) and logs it as Copilot's,
unless Copilot's own swarmia-cli hooks (any scope) log it, so that with both
installed every event is logged once.

## Payloads

| Hook | Notes |
|---|---|
| `SessionStart` | `source`: `new`, `resume`, … |
| `UserPromptSubmit` | `prompt`; also in `-p` |
| `PreToolUse` | `tool_name`, `tool_input`, `tool_use_id` |
| `PostToolUse` | adds `tool_result` {`result_type`, `text_result_for_llm`} |
| `PostToolUseFailure` | a tool error (`error`: "Path already exists") |
| `Stop` | `transcript_path`; no `last_assistant_message` |
| `SessionEnd` | `reason`; `-p` sends it too |

No payload names the model: it comes from usage.

### Tools

| Copilot's tool | Reported as | `tool_input` | Captured as |
|---|---|---|---|
| `create` | `Write` | `path`, `file_text` | the whole file: `create` refuses to overwrite one |
| `edit`, `str_replace_editor` | `Edit` | `path`, `old_str`, `new_str` | `old_str` → `new_str`, diffed on its own |
| `apply_patch` (GPT models) | `Edit` | the patch, a bare string; paths absolute or relative to `cwd` | as Codex's patches |
| `bash` | `Bash` | `command`, `mode`, `shellId` | worktree snapshots, like Claude Code's |
| `view`, `grep`, `glob`, `task`, … | `Read`, `Grep`, `Glob`, `Agent`, … | | |
| `read_bash`, `write_bash`, `stop_bash` | as is | `shellId` | |

A shell command that exits nonzero is a successful call: only the result text
says how it exited (`<shellId: 1 completed with exit code 1>`), which is read
as `failed` with the exit code.

**Background shells** (`mode: async`) keep running after their
`PostToolUse` ("command started in background"). Their snapshot is set aside
then, and their changes recorded when `read_bash` reports the command
finished, if within 10 minutes: everything that changed meanwhile is taken as
the command's, and the longer it ran, the likelier that includes the
developer's edits. A command stopped with `stop_bash` (a server, a watcher)
isn't diffed.

**Subagents** send events under their own session id, with
`parent_agent_id`; they're folded into the parent's session, a subagent's
prompt as one the agent wrote, and its `Stop` as a `SubagentStop`.

## Transcript and usage

`Stop`'s `transcript_path` is the session's events,
`~/.copilot/session-state/<session>/events.jsonl`, which a resumed session
continues. Each model request has a `session.usage_record` there (a
subagent's marked with `agentId`): Copilot counts cache reads and writes within
`inputTokens`, and reasoning within `outputTokens`. Model ids are Copilot's
(`claude-sonnet-5.5`, `gpt-5.6-sol`).

The turn's reply isn't in the `Stop` payload, so `log-hook` reads it from the
events: the main agent's last message with text before the `hook.start` of
the `Stop` hook. Copilot writes events a little after it runs the hook, so
`log-hook` waits for that record (up to a second).

## Copilot cloud agent

It runs a repository's `.github/hooks/*.json` in an ephemeral sandbox per
job, so committed project hooks fire there too. Nothing can be matched or
delivered from there (the sandbox is gone when the job ends, its network
reaches GitHub only, and no developer's token is set), so the hooks must just
stay out of the way: without the repository's swarmia-cli (no `node_modules`
in the sandbox, usually) they do nothing.

## Not yet verified

- VS Code and the GitHub Copilot app: payloads and `$COPILOT_PROJECT_DIR`
  are expected to be the CLI's (one agent), but haven't been checked. git-ai
  waits 80 ms in VS Code's `PostToolUse`, which could fire before the file is
  written ([vscode#315926](https://github.com/microsoft/vscode/issues/315926));
  swarmia-cli reads edits from payloads, so that matters only for shell
  snapshots.
- Windows (swarmia-cli doesn't ship for it yet).
- Typed skills (`/skill-name`), which aren't reported for Copilot.
