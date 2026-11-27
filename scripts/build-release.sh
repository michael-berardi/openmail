#!/bin/sh
# Assemble a local release directory for end-to-end installer tests from published assets plus the current installers.
# Usage: scripts/build-release.sh [version] [out-dir]   (default: 0.2.1, dist/release)
# This is a test fixture builder, not the signing/notarizing production path: scripts/release.sh makes release assets.
set -eu
cd "$(dirname "$0")/.."

VERSION="${1:-${OPENMAIL_TEST_VERSION:-0.2.1}}"
VERSION="${VERSION#v}"
OUT="${2:-dist/release}"
BASE="${OPENMAIL_RELEASE_BASE:-https://github.com/michael-berardi/openmail/releases/download/v$VERSION}"
BASE="${BASE%/}"
REPO="michael-berardi/openmail"
say() { printf '%s\n' "$*"; }
die() { printf 'build-release: %s\n' "$*" >&2; exit 1; }

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1" || die "download failed: $1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1" || die "download failed: $1"; }
else die "needs curl or wget"; fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{ print $1 }'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{ print $1 }'; }
else die "needs sha256sum or shasum"; fi

ASSETS="openmail-cli-v$VERSION-macos-universal.zip
openmail-cli-v$VERSION-linux-amd64.tar.gz
openmail-cli-v$VERSION-linux-arm64.tar.gz
openmail-cli-v$VERSION-windows-amd64.zip
OpenMail-v$VERSION-macos-universal.zip"

rm -rf "$OUT"
mkdir -p "$OUT"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/openmail-build.XXXXXX") || die "cannot create a temporary folder"
trap 'rm -rf "$tmp"' EXIT

fetch "$BASE/SHA256SUMS" "$tmp/published-SHA256SUMS"
for asset in $ASSETS; do
  say "fetching $asset"
  fetch "$BASE/$asset" "$OUT/$asset"
  want=$(awk -v n="$asset" '{ f=$2; sub(/^\*/, "", f); if (f == n) { print $1; exit } }' "$tmp/published-SHA256SUMS")
  [ -n "$want" ] || die "no published checksum for $asset"
  [ "$(sha256 "$OUT/$asset")" = "$want" ] || die "published checksum mismatch for $asset"
done
cp scripts/install.sh scripts/install.ps1 "$OUT/"
chmod 755 "$OUT/install.sh"
( cd "$OUT" && {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "openmail-cli-v$VERSION-macos-universal.zip" \
      "openmail-cli-v$VERSION-linux-amd64.tar.gz" \
      "openmail-cli-v$VERSION-linux-arm64.tar.gz" \
      "openmail-cli-v$VERSION-windows-amd64.zip" \
      "OpenMail-v$VERSION-macos-universal.zip" install.sh install.ps1
  else
    shasum -a 256 "openmail-cli-v$VERSION-macos-universal.zip" \
      "openmail-cli-v$VERSION-linux-amd64.tar.gz" \
      "openmail-cli-v$VERSION-linux-arm64.tar.gz" \
      "openmail-cli-v$VERSION-windows-amd64.zip" \
      "OpenMail-v$VERSION-macos-universal.zip" install.sh install.ps1
  fi
} > SHA256SUMS )
say "assembled $OUT from $REPO v$VERSION plus the current installers"
ls -l "$OUT"
