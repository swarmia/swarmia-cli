#!/bin/sh
# npm-stage.sh <version> <dir>: copies the npm packages (npm/) to <dir> for a
# release, versioned <version>: each package's version, and @swarmia/cli's pins
# on the platform packages. Adds LICENSE to each and READMEs; the binaries are
# built into <dir>/platforms/*/bin by `make npm-pack`. The packages in npm/
# carry a placeholder version: the release tag is the only source of truth.
set -eu

version=$1
dir=$2

# npm needs semver: 1.2.3 or 1.2.3-rc.1, no leading v.
if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "npm-stage: '$version' isn't a release version (like 1.2.3 or 1.2.3-rc.1); pass VERSION=..." >&2
  exit 1
fi

root=$(cd "$(dirname "$0")/.." && pwd)
rm -rf "$dir"
mkdir -p "$dir"
cp -R "$root/npm/." "$dir/"
rm -rf "$dir"/platforms/*/bin # local builds, if any

for pkg in "$dir/cli" "$dir"/platforms/*; do
  node -e '
    const fs = require("fs")
    const [file, version] = process.argv.slice(1)
    const pkg = JSON.parse(fs.readFileSync(file, "utf8"))
    pkg.version = version
    for (const dep of Object.keys(pkg.optionalDependencies || {})) pkg.optionalDependencies[dep] = version
    fs.writeFileSync(file, JSON.stringify(pkg, null, 2) + "\n")
  ' "$pkg/package.json" "$version"
  cp "$root/LICENSE" "$pkg/"
done

cp "$root/README.md" "$dir/cli/"
for pkg in "$dir"/platforms/*; do
  name=$(node -p 'JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")).name' "$pkg/package.json")
  printf '# %s\n\nThe swarmia-cli binary for one platform. Install [@swarmia/cli](https://www.npmjs.com/package/@swarmia/cli) instead: it installs this package where it applies.\n' "$name" >"$pkg/README.md"
done
