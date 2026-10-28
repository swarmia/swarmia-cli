# AGENTS.md

This file provides guidance to coding agents working with code in this repository.

## Open source

This is a public, open-source repository. Never include customer names, customer data, internal Swarmia backend details, or other sensitive information in code, comments, tests, docs, commit messages or PR descriptions.

## Commands

- `make build` — build `./swarmia-cli`
- `make test` — run unit tests (`go test ./...`); single test: `go test ./internal/attribution -run TestName`
- `make vet` — `go vet`, including the `e2e`-tagged package
- `make e2e` — end-to-end suite against a real coding agent (slow; see `docs/e2e.md`)

## Architecture

A Go CLI that hooks into coding agents (Claude Code, Codex), records what the agent writes, works out locally which committed lines came from which session, and — only if a token is configured — sends line ranges and session metadata to Swarmia. No source code leaves the machine.

The pipeline is **capture → attribute → deliver**:

- `cmd/` — CLI commands (`log-hook`, `process`, `report`, `send`, `install`). Agent hooks call `log-hook`, which appends events to a per-user local log; session end triggers a background `process`.
- `internal/` — the logic: event log and snapshots, git matching and attribution, redaction, payload building and delivery.

See `README.md` for design principles (privacy, redaction) and `docs/` for deeper topics.
