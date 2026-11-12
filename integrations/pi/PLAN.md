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
exact reconstruction does not match. These accuracy limits are accepted for this integration; data-quality work
will measure and document their impact rather than override Pi's tools.

### Session metadata and privacy

- [x] Capture user prompts and final assistant responses, including queued
  messages; ignore failed retries and honor `SWARMIA_PROMPTS=0` during delivery.
  Unit tests cover retries; real Pi runs covered a queued message, resume,
  fork and an opt-out payload.
- [x] Read assistant-message usage incrementally, group by model, and handle
  missing transcripts and copied fork history without double-counting. Validated
  real resumed and forked Pi sessions against their transcript totals.
- [x] Investigate tool and compaction usage: count separately recorded Pi cache
  warming; omit aggregated tool, compaction and branch-summary usage because
  their model and request counts are not reliably recorded. Document the gap.
- [x] Verify shutdown, reload, session replacement, crash and long-running
  behavior. Reload flushes without ending the session; a hard crash cannot emit
  an end event, and pending events are folded on a later processing run.
- [x] Verify tool output and captured file contents are excluded from payloads,
  prompt opt-out applies, and local logs and session files are private. Existing
  retention and free-text privacy limitations remain documented.

### Installation and distribution

- [ ] Embed or otherwise distribute the extension with standalone and npm CLI
  builds, and install it in opted-in projects via `swarmia-cli install`.
- [ ] Make installation idempotent, preserve unrelated project configuration,
  and define safe extension upgrades and removal.
- [ ] Resolve the project-local binary reliably and never fall back to a global
  installation for project-scoped tracking. Pi must be started in the project
  root: its project-extension discovery does not search ancestor directories.
- [ ] Verify project trust and `/reload` behavior, and prevent duplicate capture
  if multiple extension copies are loaded.
- [ ] Document setup, supported scopes, limitations and troubleshooting.

### Data quality

- [x] Accept the pre-image race rather than override Pi's built-in tools or add
  a global bootstrap. Capture remains best-effort.
- [ ] Measure attribution precision and recall in representative real-Pi
  scenarios, including concurrent edits, fuzzy edits, shell changes and commits.
- [ ] Surface or document skipped and uncertain captures so missing attribution
  is distinguishable from a session with no edits.
- [ ] Document coverage limits, including project-root startup and omitted usage
  whose model or call counts Pi does not reliably record.

### Validation and release readiness

- [x] Run Go unit tests and prototype extension tests.
- [ ] Add integration coverage for the extension-to-CLI contract, concurrent
  calls, nested tools, multiple repositories and installation.
- [ ] Add a real-Pi end-to-end scenario covering capture, commit attribution,
  processing and privacy-preserving payloads.
- [ ] Measure hook latency and verify missing binaries, timeouts and failures do
  not block Pi or leak tool output.
- [ ] Review the external agent identifier and payload compatibility before
  enabling delivery.
