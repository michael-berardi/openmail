#!/bin/bash
# Build, sign, notarize and checksum every release artifact into dist/release.
# usage: SIGN_ID="Developer ID Application: …" NOTARY_PROFILE=… scripts/release.sh v0.2.0
# SIGN_ID may also be omitted: the only Developer ID Application identity in the login keychain is used.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:?usage: release.sh vX.Y.Z}"
SIGN_ID="${SIGN_ID:-$(security find-identity -v -p codesigning 2>/dev/null | awk -F'"' '/Developer ID Application/ { print $2; exit }')}"
case "$SIGN_ID" in
  "Developer ID Application"*) ;;
  *) { echo "release: set SIGN_ID to a Developer ID Application identity from 'security find-identity -v -p codesigning'" >&2; exit 1; } ;;
esac
PROFILE="${NOTARY_PROFILE:?set NOTARY_PROFILE to the notarization keychain profile}"
OUT=dist/release; rm -rf "$OUT" && mkdir -p "$OUT"
LDF="-s -w -X github.com/michael-berardi/openmail/internal/core.Version=$VERSION"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT

echo "== CLI (macOS universal)"
for a in arm64 amd64; do GOOS=darwin GOARCH=$a CGO_ENABLED=0 go build -trimpath -ldflags "$LDF" -o "$T/openmail-$a" ./cmd/openmail; done
CLI="$T/openmail-cli-$VERSION"; mkdir -p "$CLI" && lipo -create "$T/openmail-arm64" "$T/openmail-amd64" -output "$CLI/openmail"
codesign --force --options runtime --timestamp --sign "$SIGN_ID" "$CLI/openmail"
cp LICENSE README.md "$CLI/"
ditto -c -k --keepParent "$CLI" "$OUT/openmail-cli-$VERSION-macos-universal.zip"
xcrun notarytool submit "$OUT/openmail-cli-$VERSION-macos-universal.zip" --keychain-profile "$PROFILE" --wait

echo "== Desktop app (macOS universal)"
VERSION="$VERSION" UNIVERSAL=1 SIGN_ID="$SIGN_ID" ./scripts/bundle-macos.sh
ditto -c -k --keepParent dist/OpenMail.app "$T/app.zip"
xcrun notarytool submit "$T/app.zip" --keychain-profile "$PROFILE" --wait
xcrun stapler staple dist/OpenMail.app
spctl --assess --type execute --verbose dist/OpenMail.app
ditto -c -k --keepParent dist/OpenMail.app "$OUT/OpenMail-$VERSION-macos-universal.zip"

echo "== CLI (Linux, Windows)"
for t in linux/amd64 linux/arm64; do
  d="$T/${t/\//-}"; mkdir -p "$d"; GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build -trimpath -ldflags "$LDF" -o "$d/openmail" ./cmd/openmail
  cp LICENSE README.md "$d/"; tar -C "$d" -czf "$OUT/openmail-cli-$VERSION-${t/\//-}.tar.gz" .
done
d="$T/windows"; mkdir -p "$d"; GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$LDF" -o "$d/openmail.exe" ./cmd/openmail
cp LICENSE README.md "$d/"; (cd "$d" && zip -q "$OLDPWD/$OUT/openmail-cli-$VERSION-windows-amd64.zip" openmail.exe LICENSE README.md)

# Ship the installers with the signed assets so one-line installs and SHA-256 verification stay in sync.
cp scripts/install.sh scripts/install.ps1 "$OUT/"
chmod 755 "$OUT/install.sh"
(cd "$OUT" && shasum -a 256 * > SHA256SUMS)
cat "$OUT/SHA256SUMS"
