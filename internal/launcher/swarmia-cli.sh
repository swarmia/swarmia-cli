#!/bin/sh
# Runs swarmia-cli @VERSION@, downloading it from its GitHub release on first
# use. Written by `swarmia-cli install --launcher`: commit it, and rerun that
# with a newer swarmia-cli to upgrade. https://github.com/swarmia/swarmia-cli
#
# Coding agents run hooks on every tool call and wait for them, so once the
# binary is downloaded this forks nothing. Hooks (log-hook) never wait for a
# download either: they start one in the background and do nothing until it's
# done. Other commands download first.
#
# Downloads go to ${XDG_CACHE_HOME:-~/.cache}/swarmia/cli/<version>/ and must
# match the checksums below. To download from a mirror of the GitHub releases
# (<url>/v<version>/<file>), set SWARMIA_CLI_DOWNLOAD_URL; curl and wget use
# your proxy settings.

version=@VERSION@
sha256_darwin_arm64=@SHA256_DARWIN_ARM64@
sha256_darwin_x64=@SHA256_DARWIN_X64@
sha256_linux_arm64=@SHA256_LINUX_ARM64@
sha256_linux_x64=@SHA256_LINUX_X64@

cache=${XDG_CACHE_HOME:-$HOME/.cache}/swarmia/cli/$version
[ ! -x "$cache/swarmia-cli" ] || exec "$cache/swarmia-cli" "$@"

# download fetches this platform's release, checks it and caches its binary.
download() {
  case $(uname -s)-$(uname -m) in
    Darwin-arm64) platform=darwin-arm64 sum=$sha256_darwin_arm64 ;;
    Darwin-x86_64) platform=darwin-x64 sum=$sha256_darwin_x64 ;;
    Linux-aarch64 | Linux-arm64) platform=linux-arm64 sum=$sha256_linux_arm64 ;;
    Linux-x86_64) platform=linux-x64 sum=$sha256_linux_x64 ;;
    *)
      echo "swarmia-cli: no release for $(uname -s) $(uname -m)" >&2
      return 1
      ;;
  esac
  file=swarmia-cli_${version}_$platform.tar.gz
  url=${SWARMIA_CLI_DOWNLOAD_URL:-https://github.com/swarmia/swarmia-cli/releases/download}
  url=${url%/}/v$version/$file

  # Next to the cache, so the binary moves into place in one rename.
  mkdir -p "${cache%/*}" && tmp=$(mktemp -d "${cache%/*}/.download.XXXXXX") || return 1
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 2 -o "$tmp/$file" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$tmp/$file" "$url"
  else
    echo "swarmia-cli: downloading needs curl or wget" >&2
    false
  fi || {
    echo "swarmia-cli: can't download $url" >&2
    rm -rf "$tmp"
    return 1
  }

  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/$file")
  else
    got=$(shasum -a 256 "$tmp/$file")
  fi
  if [ "${got%% *}" != "$sum" ]; then
    echo "swarmia-cli: $url doesn't match its checksum in $0" >&2
    rm -rf "$tmp"
    return 1
  fi

  if tar -xzf "$tmp/$file" -C "$tmp" swarmia-cli && mkdir -p "$cache" &&
    mv -f "$tmp/swarmia-cli" "$cache/swarmia-cli"; then
    rm -rf "$tmp"
  else
    echo "swarmia-cli: can't unpack $file into $cache" >&2
    rm -rf "$tmp"
    return 1
  fi
}

if [ "$1" = log-hook ]; then
  # One download at a time. A failed one keeps the lock, so the hooks that
  # follow don't all retry: it expires after 10 minutes.
  lock=$cache.lock
  if [ -d "$lock" ] && [ -n "$(find "$lock" -prune -mmin +10 2>/dev/null)" ]; then
    rmdir "$lock" 2>/dev/null
  fi
  if mkdir -p "${cache%/*}" 2>/dev/null && mkdir "$lock" 2>/dev/null; then
    (download && rmdir "$lock" && rm -f "$cache.log") </dev/null >"$cache.log" 2>&1 &
  fi
  exit 0
fi

echo "swarmia-cli: downloading swarmia-cli $version" >&2
download || exit 1
rmdir "$cache.lock" 2>/dev/null
rm -f "$cache.log"
exec "$cache/swarmia-cli" "$@"
