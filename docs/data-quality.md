# Data quality: known issues

What `report` and the payload get wrong today, and which way each issue skews
the numbers. Found by dogfooding on a large repository where every committed
line was written by an agent, so any line classed `modified` (human-edited) or
left unattributed points at a gap in recording or matching.

The rule we hold to, borrowed from git-ai: **attribution gaps are acceptable;
misattribution is not.** When we can't tell whose lines a change holds, we
record nothing rather than guess.

The numbers involved:

- `ai_lines_generated`: lines the agent wrote that were still there after its
  last edit to each file (the denominator).
- `ai_lines_committed`: of those, how many commits hold `exact` or
  `normalized` (the numerator).
- Per committed line: `exact`, `normalized`, `modified`, or not attributed.

## Recording: shell commands

Lines written by shell commands are found by snapshotting the worktree before
and after each command (see `internal/snapshot`).

### Content restored by git without a trace is credited to the agent

**Skews:** `ai_lines_generated` up, and can credit one session with lines
another session (or a person) wrote.

Lines a command restores from git are excluded when git leaves evidence of
where they came from: HEAD before and after the command, the commits a stopped
rebase, cherry-pick, revert or merge is applying (`REBASE_HEAD` and the like),
and a stash entry the command popped. Some commands restore content and leave
nothing behind:

- `git stash apply` (the entry stays, so the stash ref doesn't move)
- `git cherry-pick --no-commit`, `git checkout <rev> -- <path>`,
  `git restore --source`, `git apply`

Fixing these means parsing the command for the revision it reads from, or
treating any content found in recent commits as not authored. Both have false
negatives; neither is done.

### Lines a shell command wrote are never subtracted

**Skews:** `ai_lines_generated` up, so survival down.

For the agent's own edit tools, the count is recomputed from the file's content
after each edit, so lines it wrote and then replaced don't count. For shell
commands the file's content isn't kept, so their added lines are only ever
added on: a debug line written with `sed` and removed again still counts, and
a line rewritten several times counts once per version.
Fixing it means recomputing the count from the worktree after shell commands
too, which the snapshot already reads.

### Background commands go unrecorded

**Skews:** `ai_lines_generated` and `ai_lines_committed` down.

A shell command the agent runs in the background reports back as soon as it's
started, so the snapshot after it sees none of its changes. The next command's
snapshot treats them as already there, so they're never credited. Typical
case: a slow `pnpm add` that edits `package.json`.

### Commands over the time budget, or that fail closed, go unrecorded

**Skews:** both counts down. **Visible:** `report` counts these commands per
session.

Each snapshot gets at most 500 ms. Large changes (a codemod over hundreds of
files, a big checkout) can exceed it. A command that leaves a rebase stopped
without naming the commit it applies (a `break`) is not recorded on purpose
(`snapshot: git-operation`).

### Other recording gaps

- **Parallel shell commands and human saves while a command runs** are
  credited to the command: the snapshot can't tell them apart.
- **Files over 1 MB and binary files** are skipped. That's intended for
  lockfiles and bundles, but it also skips large hand-written files.
- **Events the hooks never see:** a command that resets the agent's settings
  file to a version without the hooks isn't logged, nor is anything after it
  until the hooks are back. Sessions on other machines or in the cloud aren't
  seen either: their commits arrive by `git pull` and stay unattributed, which
  is correct, but the report doesn't say why.
- **Recorded data isn't repaired.** Fixes to recording apply to commands run
  after the upgrade. Sessions already recorded keep what was recorded then,
  e.g. a stopped rebase credited to the session that ran it (fixed in the
  snapshot, still present in older sessions).

## Matching: committed lines

### Reflowed code is classed `modified`

**Skews:** `modified` up (a false claim of human editing), survival down.

The alignment matches one line wrapped into up to four, or up to four joined
into one. A formatter that re-splits a multi-line expression differently (the
agent wrote a call one argument per line; the formatter puts the arguments on
the first line and wraps only the last object) is an m:n reflow, and its lines
come out `modified`. Comparing the concatenated text of a short run of lines on
each side would cover it.

### `modified` rests on similarity

**Skews:** `modified` up.

`modified` means at least half the line's tokens match one of the agent's
lines. That's an inference from similarity, which git-ai refuses to make. In
the dogfooding data, after the reorder fix, every remaining `modified` line was
a formatter artifact or another agent's edit, never a human one. Open question:
keep reporting `modified`, rename it to something weaker ("changed after the
agent"), or report such lines as unknown.

### Renames lose authorship

**Skews:** `ai_lines_committed` down.

Lines moved to a new path (`git mv` and an edit) count only the edit. Lines a
commit only moved aren't "introduced" by it, and the hunks recorded for the old
path aren't matched against the new one.

## Totals across sessions

### The same lines can be credited to two sessions

**Skews:** organization-level totals up, if summed per session.

Within a session each line counts once, however many commits hold it. Across
sessions nothing prevents two sessions from claiming the same committed line,
for example when one session's shell command restores lines another session
wrote (see "Content restored by git without a trace"). A server summing across
sessions should count each (commit, path, line) once.

## Turns

### An interrupted turn can take the next automatic turn's end and reply

**Skews:** the agent's working time up and the developer's down, for that
turn; the reply is paired with the wrong prompt; automatic turns down by one.

A turn the developer interrupts never ends: Claude Code doesn't run its Stop
hook on an interrupt, and has no interrupt hook. A message the agent submits
itself (a background task's notification) that arrives while a turn has no end
is folded into that turn, because it can also arrive mid-turn: Claude Code runs
the prompt hook for it between tool calls (confirmed by
`TestClaudeCodeMidTurnNotification`, see `docs/e2e.md`). From hook events
alone, an interrupted turn looks like one still running.

So if the developer interrupts while a background task (a test run, a build)
is still going, and it finishes before their next prompt, the interrupted turn
gets the end and reply of the agent's answer to the notification ("The tests
passed."), instead of no end, and there's no automatic turn for it. If the
developer prompts again first, which is common after an interrupt, nothing
goes wrong.

Not reproduced end to end: headless `claude -p` can't be interrupted. The fix
is to read the interrupt from the transcript, where Claude Code records it as
a user entry (`[Request interrupted by user]`), and end the open turn there.
Codex isn't affected: swarmia-cli takes every Codex prompt for the
developer's, so nothing is folded into an open turn. (Codex also has an
`Interrupt` hook, which swarmia-cli doesn't install yet.)
