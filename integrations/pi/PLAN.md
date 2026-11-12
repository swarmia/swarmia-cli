# Pi support plan

## Goal

Support project-scoped Pi sessions in Swarmia CLI: capture agent-authored
changes and session metadata locally, attribute committed lines to their Pi
sessions, and use the existing privacy-preserving delivery pipeline. Projects
must opt in; installing the CLI globally must not enable tracking globally.

## Done criteria

A trusted, opted-in project can install the integration using `swarmia-cli
install`, start or resume Pi, make edits through its built-in tools or shell,
and see accurately attributed committed lines in `swarmia-cli report`. Session
metadata and usage are captured without duplicate events, existing privacy
controls apply, failures do not interfere with Pi, and automated tests cover the
end-to-end flow.

## Status and tasks

### Prototype capture and attribution

- [x] Add a project-scoped Pi extension prototype that invokes the local CLI.
- [x] Capture session boundaries and bracket bash calls for shell snapshots.
- [x] Capture successful `edit` and `write` calls using file pre-images; skip
  unreliable pre-images and overlapping calls to the same file.
- [x] Add Pi event normalization and edit attribution with unit tests.
- [x] Manually validate two Pi edit events and four attributed lines in a local
  project report. This did not validate attribution to a commit.
- [x] Validate `write`, a failed call, nested tool calls, and attribution to
  local commits in real Pi sessions in isolated test repositories.
- [x] Exercise overlapping calls, cancellation, reload and changed file content;
  reject reconstructions that disagree with the observed post-image.
- [x] Normalize supported Pi paths and line endings, test BOM/CRLF, and validate
  a write and commit in a second repository from a real Pi session.

**Prototype limitation:** Pi's mutation queue is internal to tool execution;
pre/post extension events cannot lock out external writers or a later handler
that changes a write's input. Post-image verification catches many races, but
cannot prove a write's pre-image was current. Fuzzy Pi edits are skipped when
exact reconstruction does not match. Production-quality concurrency guarantees
remain a release-readiness task below.

### Session metadata and privacy

- [ ] Capture user prompts and assistant responses, including queued messages,
  retries, session resume and forks; respect `SWARMIA_PROMPTS=0`.
- [ ] Read Pi transcript usage incrementally without double-counting; handle
  model switches and sessions without a transcript.
- [ ] Verify lifecycle semantics for shutdown, reload, session replacement,
  crash recovery and long-running sessions.
- [ ] Check that no tool output or source code is delivered and that local
  captures and temporary state follow existing privacy and retention rules.

### Installation and distribution

- [ ] Embed or otherwise distribute the extension with standalone and npm CLI
  builds, and install it in opted-in projects via `swarmia-cli install`.
- [ ] Make installation idempotent, preserve unrelated project configuration,
  and define safe extension upgrades and removal.
- [ ] Resolve the project-local binary reliably, including subdirectory starts;
  never fall back to a global installation for project-scoped tracking.
- [ ] Verify project trust and `/reload` behavior, and prevent duplicate capture
  if multiple extension copies are loaded.
- [ ] Document setup, supported scopes, limitations and troubleshooting.

### Validation and release readiness

- [x] Run Go unit tests and prototype extension tests.
- [ ] Add integration coverage for the extension-to-CLI contract, concurrent
  calls, nested tools, multiple repositories and installation.
- [ ] Add a real-Pi end-to-end scenario covering capture, commit attribution,
  processing and privacy-preserving payloads.
- [ ] Resolve the pre-image race for concurrent external writers and later
  input-mutating extensions, or explicitly accept and document its accuracy
  impact before production use.
- [ ] Measure hook latency and verify missing binaries, timeouts and failures do
  not block Pi or leak tool output.
- [ ] Review the external agent identifier and payload compatibility before
  enabling delivery.
