#!/bin/sh
# npm-smoke-test.sh <dir> <version>: installs the packed npm packages in <dir>
# (from `make npm-pack`) into a scratch repository with npm and with pnpm,
# as a devDependency, and checks that they run on this machine: the version,
# a project-scope install whose hook logs an event, and a local-scope install
# that points at the package's launcher. Run before publishing, on each
# platform we can.
set -eu

dir=$(cd "$1" && pwd)
version=$2
cli="$dir/swarmia-cli-$version.tgz"
[ -f "$cli" ] || { echo "smoke: no $cli" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export SWARMIA_STATE_DIR="$work/state"
unset CLAUDE_PROJECT_DIR

fail() { echo "smoke ($pm): $*" >&2; exit 1; }

for pm in npm pnpm; do
  repo="$work/$pm repo" # a space, as in real paths
  mkdir -p "$repo/.claude"
  cd "$repo"
  git init -q

  # The platform packages aren't on the registry yet: point every one at its
  # tarball.
  overrides=
  for p in "$dir"/swarmia-cli-*-"$version".tgz; do
    name=${p##*/swarmia-cli-}
    name="@swarmia/cli-${name%-"$version".tgz}"
    overrides="$overrides\"$name\": \"file:$p\","
  done
  overrides=${overrides%,}
  printf '{ "name": "smoke", "private": true, "devDependencies": { "@swarmia/cli": "file:%s" }, "overrides": { %s } }\n' "$cli" "$overrides" >package.json
  if [ "$pm" = pnpm ]; then
    printf 'nodeLinker: isolated\noverrides: { %s }\n' "$overrides" >pnpm-workspace.yaml
    pnpm install --silent >/dev/null
    run() { pnpm exec swarmia-cli "$@"; }
  else
    npm install --silent --no-audit --no-fund >/dev/null
    run() { npx --no swarmia-cli "$@"; }
  fi

  got=$(run version | head -1)
  [ "$got" = "swarmia-cli $version" ] || fail "version says '$got', want 'swarmia-cli $version'"

  run install >/dev/null
  hook=$(node -p 'require("./.claude/settings.json").hooks.SessionStart[0].hooks[0].command')
  case $hook in
    *'node_modules/@swarmia/cli/bin/swarmia-cli'*) ;;
    *) fail "project hook doesn't run the package's launcher: $hook" ;;
  esac
  echo '{"hook_event_name":"SessionStart","session_id":"smoke"}' | CLAUDE_PROJECT_DIR=$repo sh -c "$hook" ||
    fail "hook failed"
  events="$(run state-dir)/events.jsonl"
  grep -q '"session_id":"smoke"' "$events" || fail "hook logged no event in $events"

  run install --scope local >/dev/null 2>&1
  local=$(node -p 'require("./.claude/settings.local.json").hooks.SessionStart[0].hooks[0].command')
  case $local in
    *'${CLAUDE_PROJECT_DIR}/node_modules/@swarmia/cli/bin/swarmia-cli'*) ;;
    *) fail "local hook doesn't run the package's launcher: $local" ;;
  esac

  echo "smoke ($pm): ok"
done
