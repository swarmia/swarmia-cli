# Binary distribution

How else we could package swarmia-cli, beyond the npm package and GitHub
release tarballs. Research notes from October 2026.

**Status (November 2026):** the README offers the npm package, release
tarballs and mise. `go install` is no longer recommended: its binary has no
version built in, and releases are the supported channel. Personal-scope hooks
point at a package manager's link to the current version (mise's `latest`,
Homebrew's `bin/`) rather than the versioned real path, so the channels in
option 3 and 4 survive upgrades. The rest is undecided.

## Two separate problems

1. **Getting the binary onto a machine.**
2. **Giving project hooks a stable, pinned command.** Project-scope hooks are
   committed in `.claude/settings.json` / `.codex/hooks.json`, so the command
   must work for every teammate in a fresh clone, at the version the repository
   chose. Today that's `node_modules/@swarmia/cli/bin/swarmia-cli`.

npm solves both for JavaScript repositories. Most other channels (Homebrew,
MDM) only solve the first. For a Java, Go or Python repository, the second is
the real gap.

## Options

### 1. Agent plugins (Claude Code, later Codex)

Distribute through the agents themselves, independent of the repository's
language.

- A Claude Code plugin ships `hooks/hooks.json` whose commands reference
  `"${CLAUDE_PLUGIN_ROOT}"/...`. It has `${CLAUDE_PLUGIN_DATA}` (per user, kept
  across plugin updates) to cache the binary in, `version` to pin releases,
  and `userConfig` with `sensitive: true` for the token.
- A team commits `extraKnownMarketplaces` and `enabledPlugins` in
  `.claude/settings.json`; teammates are asked once to install it. Claude Code
  never installs plugins silently.
- Admins can force-enable it through managed settings, and it still runs under
  `allowManagedHooksOnly`.
- The binary comes from a small launcher in the plugin that downloads the
  pinned release, checks it against `SHA256SUMS` and caches it in
  `CLAUDE_PLUGIN_DATA` (the same launcher as option 2). Committing the four
  platform binaries to the marketplace repository is the alternative.
- Publishing needs no approval from Anthropic. A git repository with a
  `.claude-plugin/marketplace.json` listing the plugin is a published
  marketplace; anyone who can clone it can install from it, so a private
  repository is a private marketplace.
- Auto-update is off by default for third-party marketplaces: users run
  `claude plugin update` or turn it on. With a pinned binary behind the hooks,
  that's a feature.
- Anthropic's directory (claude.ai/directory) is optional and reviewed: submit
  from the developer portal (claude.ai/directory/manage) with a paid claude.ai
  plan (an Owner on Team / Enterprise); each version is reviewed before it's
  published. A listing reaches claude.ai, Cowork and Claude Code (as
  `<name>@synced`). The official `claude-plugins-official` marketplace takes no
  portal submissions; that goes through an Anthropic partner contact.
- claude.ai and Cowork, including organization sync, don't install plugins
  with a top-level `bin/` directory, and our hooks only matter in Claude Code
  anyway. Keep executables out of `bin/` (say, `scripts/`). A directory
  listing is for discovery; customers would install from our own marketplace.
- Codex reportedly has plugin-bundled hooks since 0.128 and repository
  marketplaces at `.agents/plugins/marketplace.json`, but only third-party
  sources say so. Verify before relying on it.
- Cost: two plugin formats to maintain; Codex still needs each user to trust
  the hooks.

### 2. A committed launcher script (the `gradlew` / `mvnw` pattern)

The direct answer for Java and any other non-npm repository.

- `swarmia-cli install --launcher` (hypothetical) writes a small POSIX
  `.swarmia/swarmia-cli` holding the pinned version and per-platform SHA256s.
  On first use it fetches the GitHub release into `~/.cache`; project hooks
  point at it.
- Java developers already trust this pattern (`gradlew`); Meta's DotSlash is a
  general version of it.
- Fits what exists: `detectBinaryRef` already prefers a repository-relative
  binary, and hooks already do nothing until the binary exists.
- Watch-outs:
  - Never download inline in a hook (latency budget): start a detached
    download and do nothing until it's ready.
  - Support corporate proxies and mirrors with an override URL.
  - It's a download, not data leaving the machine, but document it plainly.

### 3. mise, aqua, Nix

Nearly free.

- `mise use github:swarmia/swarmia-cli@1.2.3` works (verified with 0.3.3):
  the release assets follow `name_version_os-arch.tar.gz`, which mise matches
  automatically. Documented in the README.
- An entry in the aqua registry adds checksum verification; mise prefers aqua
  for new tools.
- A committed `mise.toml` pins the version per repository; hooks call
  `swarmia-cli` on `$PATH`.
- Only helps teams already using mise, but costs about one PR.

### 4. Homebrew tap (`swarmia/tap`)

For individuals (local and user scope, trying it out), not project hooks: no
per-repository pinning.

- GoReleaser can generate it. It has hard-deprecated `brews` (formulae of
  prebuilt binaries) in favor of casks.
- Since Homebrew 5.0, casks must be codesigned and notarized; unsigned casks
  are removed from the official tap from September 2026.
- Casks are macOS-only. A formula in our own tap that builds from source with
  Go avoids notarization and keeps Linuxbrew.
- homebrew-core later, once the project is notable enough.

### 5. Enterprise / MDM rollout

Likely what larger customers ask for.

- A signed `.pkg`, plus `.deb` / `.rpm` (GoReleaser's nfpm).
- A drop-in `managed-settings.d/` file with the Claude Code hooks; Nightfall
  ships its hooks this way. Codex has managed hooks in `requirements.toml` /
  MDM; third-party docs disagree on the key names, so check OpenAI's reference.
- This covers every repository on the machine, the same concern the README
  raises about user scope. It needs organization-side repository filtering
  before sending, not after.

### 6. PyPI wheels

Cheap, for Python teams.

- `go-to-wheel` (Simon Willison) or `bin2whl` wrap the binary in per-platform
  wheels.
- Pinned as a uv / poetry dev dependency; hooks call
  `.venv/bin/swarmia-cli`.
- Using PyPI for non-Python binaries is debated by its maintainers, but
  tolerated today.

### 7. Maven / Gradle artifact

Skip. Possible (protoc ships as
`com.google.protobuf:protoc:exe:osx-aarch_64`), but hooks run outside the
build, so the binary wouldn't exist until someone ran a build task. Option 2
is better for Java.

### Also

- Devcontainer feature, for Codespaces.
- Scoop / winget once Windows is supported.
- Not Docker: hooks must start in milliseconds.

## Suggested order

1. **mise / aqua.** mise done; an aqua registry entry would add checksum
   verification.
2. **Committed launcher.** Covers Java, Go and Python repositories at project
   scope. The release workflow already produces the tarballs and
   `SHA256SUMS` it needs.
3. **Claude Code plugin**, reusing the launcher to fetch the binary.
4. **GoReleaser** for release builds once Homebrew, deb/rpm and signing are
   wanted: one config for all of them. npm packaging stays custom.
5. **Apple signing and notarization**, needed for the Homebrew cask and MDM
   `.pkg`. Start enrollment early (see below).

## Apple signing

Not strictly required, but needed in practice for enterprise rollout.

### Why it isn't strictly required

- Gatekeeper only checks files marked as downloaded from the internet
  (quarantined). Packages installed by Jamf, Kandji or Intune agents usually
  aren't, nor are npm or `curl` installs, which is why the unsigned npm package
  works today.
- Apple Silicon refuses arm64 code with no signature at all, but Go's linker
  ad-hoc signs darwin/arm64 builds. No Apple account needed.

### Why we want it anyway

- **Endpoint security.** EDR (CrowdStrike, SentinelOne) and allowlisting
  (Santa) often flag or block unsigned binaries, and allowlists are commonly
  per publisher (Apple Team ID); unsigned binaries have none. Ours runs on
  every tool call, so it gets noticed. Expect security reviews to ask for our
  Team ID.
- **Some MDM install paths need a signed package.** Apple's native MDM install
  command (`InstallEnterpriseApplication`) requires a signed `.pkg`; Intune's
  line-of-business `.pkg` apps have historically required one (unverified for
  its newer package type). Jamf policies accept unsigned packages.
- **The Homebrew cask** needs signing and notarization anyway.

### What it takes

- **Apple Developer Program as an organization:** $99 a year, needs a D-U-N-S
  number; verification can take a while. Check whether Swarmia already has
  one.
- **Two certificates:** *Developer ID Application* for the binary,
  *Developer ID Installer* for the `.pkg`.
- **Notarization with `notarytool`.** A bare binary in a tarball can be
  notarized but not stapled, so Gatekeeper checks it online; a `.pkg` can be
  stapled and passes offline.
- **Works from Linux CI.** `rcodesign` (apple-codesign) or GoReleaser's
  notarize step sign and notarize without a Mac runner, so the release job can
  stay on `ubuntu-latest`. The certificate and App Store Connect API key would
  be secrets in the `release` environment.
- Sign and notarize the macOS binaries in every release, including those in
  the npm packages, so every channel ships the same signed binary.

Windows would need a separate Authenticode certificate.

## Sources

- [GoReleaser deprecations (brews → casks)](https://goreleaser.com/deprecations/),
  [GoReleaser v2.10](https://goreleaser.com/blog/goreleaser-v2.10/)
- [Homebrew 5.0.0](https://workbrew.com/blog/homebrew-5-0-0),
  [What Homebrew 5 means for your Mac fleet](https://workbrew.com/blog/what-homebrew-5-0-0-means-for-your-mac-fleet)
- [Publish and distribute a Claude Code plugin](https://code.claude.com/docs/en/plugins/publish),
  [Claude Code plugin manifest reference](https://code.claude.com/docs/en/plugins-reference),
  [plugins overview](https://code.claude.com/docs/en/plugins/overview),
  [plugin relevance](https://code.claude.com/docs/en/plugin-relevance),
  [settings](https://code.claude.com/docs/en/settings)
- [Nightfall hooks via managed settings](https://help.nightfall.ai/data-exfiltration-prevention/ai-agent-security/ai-governance/auditability-and-control/setup-and-installation/nightfall-hooks-for-claude-code)
- [OpenAI managed configuration](https://developers.openai.com/de-DE/docs/enterprise/managed-configuration),
  [Codex hooks](https://developers.openai.com/codex/hooks.md),
  [Codex managed config (third-party)](https://codex.danielvaughan.com/2026/04/27/codex-cli-enterprise-managed-configuration-requirements-toml-admin-policies/),
  [Codex plugin-bundled hooks (third-party)](https://codex.danielvaughan.com/2026/05/01/codex-cli-plugin-bundled-hooks-reusable-quality-gates-marketplace/)
- [mise GitHub backend](https://mise.jdx.dev/dev-tools/backends/github.html),
  [mise aqua backend](https://mise.jdx.dev/dev-tools/backends/aqua.html)
- [Distributing Go binaries via PyPI](https://simonwillison.net/2026/Feb/4/distributing-go-binaries/),
  [PyPI as binary storage discussion](https://discuss.python.org/t/use-of-pypi-as-a-generic-storage-platform-for-binaries/106044/57),
  [bin2whl](https://pypi.org/project/bin2whl/)
- [DotSlash](https://engineering.fb.com/2024/02/06/developer-tools/dotslash-simplified-executable-deployment/)
- [protoc's Maven classifier pattern](https://github.com/grpc/grpc-java/issues/7690)
