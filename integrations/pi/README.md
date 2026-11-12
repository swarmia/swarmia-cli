# Pi extension prototype

This prototype records Pi session boundaries and captures `bash`, `edit`, and
`write` tool calls. It does not yet capture prompts, replies, or token usage,
and is not installed by `swarmia-cli install`. Edit and write attribution is
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
project root. Pi's normal project discovery and trust rules apply.

Run the extension tests with `node --test integrations/pi/*.test.mjs`.
