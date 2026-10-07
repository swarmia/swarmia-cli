# Releasing

A release is a tag. Pushing `v1.2.3` (or a prerelease, `v1.2.3-rc.1`) on
`main` runs [the release workflow](.github/workflows/release.yml):

1. **Build:** checks that the tag is a version and is on `main`, runs the
   tests, and builds the npm packages with `make npm-pack VERSION=1.2.3`: the
   four platform binaries, with the version built in, and the five packages,
   versioned from the tag. The packages in `npm/` carry a placeholder version
   (`0.0.0`); the tag is the only place a version lives.
2. **Smoke test** on Linux x64 and arm64 and macOS arm64 and x64: installs the
   packed packages with npm and with pnpm (isolated layout), and checks
   `swarmia-cli version`, a project-scope install whose hook logs an event, and
   a local-scope install (`scripts/npm-smoke-test.sh`).
3. **Publish** to npm, once approved in the `release` environment: the four
   platform packages, then `@swarmia/cli`, with provenance. Prereleases go to
   the `next` dist-tag, releases to `latest`. Versions already published are
   skipped, so a failed run can be rerun.
4. **GitHub release** with generated notes, the binaries as
   `swarmia-cli_<version>_<os>-<arch>.tar.gz`, and `SHA256SUMS`.

## Cutting a release

```sh
git checkout main && git pull
make npm-smoke-test VERSION=1.2.3      # optional: the same checks, locally
git tag v1.2.3
git push origin v1.2.3
```

Then approve the `release` environment's deployment in the workflow run. A
published npm version can't be replaced: if something is wrong after publishing,
release the next patch version. To try a version out first, release
`v1.2.3-rc.1`, and install it with `pnpm add -D @swarmia/cli@next`.

Repositories with pnpm's `minimumReleaseAge` won't install a version until it's
that old; `minimumReleaseAgeExclude: ['@swarmia/*']` lifts it for these
packages.

## One-time setup

**GitHub** (repository settings):

- **Environment `release`:** required reviewers (the maintainers), and
  deployment limited to tags matching `v*`.
- **Tag ruleset** for `v*`: only maintainers can create, update or delete
  release tags.
- **Public repository:** npm provenance requires it, so the first release comes
  after open-sourcing.

**npm** (the `@swarmia` organization):

1. **First release (0.1.0):** npm may only let you configure trusted
   publishing for packages that already exist. If so, create a granular access
   token that can publish to `@swarmia` packages, expiring in a day or two, and
   save it as the `NPM_TOKEN` secret of the `release` environment. Release
   `v0.1.0` as above.
2. **Trusted publishing:** for each of the five packages (`@swarmia/cli`,
   `@swarmia/cli-darwin-arm64`, `@swarmia/cli-darwin-x64`,
   `@swarmia/cli-linux-arm64`, `@swarmia/cli-linux-x64`), add a trusted
   publisher on npmjs.com: GitHub Actions, repository `swarmia/swarmia-cli`,
   workflow `release.yml`, environment `release`. Then, under the package's
   publishing access, require two-factor authentication and disallow tokens.
3. Delete the `NPM_TOKEN` secret and revoke the token. From then on the
   workflow publishes with short-lived credentials from GitHub, and nothing
   else can publish.
