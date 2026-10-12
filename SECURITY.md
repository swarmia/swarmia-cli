# Security policy

## Reporting a vulnerability

Please don't open a public issue. Email
[security@swarmia.com](mailto:security@swarmia.com) instead.

We'll acknowledge the report within a few working days, keep you updated while
we work on a fix, and credit you in the advisory unless you'd rather we didn't.

## Supported versions

Only the latest release gets security fixes. Upgrade with
`pnpm add -D @swarmia/cli@latest` (or `npm install -D @swarmia/cli@latest`).

## Scope

swarmia-cli runs as a hook inside your coding agent, so it's particularly
interested in reports of:

- source code, secrets or unredacted prompts leaving your machine
- data sent to Swarmia without a configured token
- a way to make the hooks run code an attacker controls
- tampering with the published npm packages or GitHub release binaries
