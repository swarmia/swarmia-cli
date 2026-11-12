# Pi extension prototype

This prototype records Pi session boundaries, prompts and final replies, and
captures `bash`, `edit`, and `write` tool calls. It reads assistant-message
and cache-warming usage from persisted Pi transcripts. Tool-result, compaction,
branch-summary and extension-defined usage are omitted because Pi does not
reliably record the models and number of calls behind those aggregated values.
Ephemeral sessions have no transcript usage; a fork's usage is unavailable if
its parent transcript is missing. It is not installed by
`swarmia-cli install`. As with other agents,
`SWARMIA_PROMPTS=0` withholds prompts and replies from delivery, not the local
event log. The prototype stores file pre/post-images in that private log until
processing; existing session retention and privacy rules apply. Pi retains its
own transcripts independently. Prompts and replies can contain code outside
code blocks, as described in the main README. Edit and write attribution is
best-effort: unreadable or large files, overlapping calls to the same file, and
edits whose reconstructed output differs from the observed file are skipped.
Pi's tool mutation queue does not cover extension pre/post events: external
writers can still race with a captured pre-image, especially for `write`.
Fuzzy edits may also be skipped. Do not treat this prototype as a complete or
race-free attribution source.

For an opt-in experiment, build the current CLI in a repository root, then copy
`swarmia.js` to that repository's `.pi/extensions/swarmia.js`. Trust the project
in Pi and start a new session or reload extensions. The extension runs only a
project-local `swarmia-cli` or `node_modules/.bin/swarmia-cli`; it does not fall
back to a globally installed binary. Remove the extension file to disable it.
The extension location must be exactly `.pi/extensions/swarmia.js` under the
project root. Pi's normal project discovery and trust rules apply. Reload
flushes the session without ending it; an abrupt process termination cannot
emit a session-end event. A later report or log-size-triggered process can fold
captured events, but cannot recover events Pi never emitted.

Run the extension tests with `node --test integrations/pi/*.test.mjs`.
