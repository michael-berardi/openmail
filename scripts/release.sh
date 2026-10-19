#!/bin/bash
# Build, sign, notarize and checksum every release artifact into dist/release.
# usage: scripts/release.sh v0.1.0      (macOS with the Developer ID certificate and notary profile)
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:?usage: release.sh vX.Y.Z}"
SIGN_ID="${SIGN_ID:-Developer ID Application: Michael Berardi (T63VT9UAY2)}"
PROFILE="${NOTARY_PROFILE:-ImploseLabs-Notarization}"
OUT=dist/release; rm -rf "$OUT" && mkdir -p "$OUT"
LDF="-s -w -X github.com/michael-berardi/openmail/internal/core.Version=$VERSION"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT

echo "== CLI (macOS universal)"
for a in arm64 amd64; do GOOS=darwin GOARCH=$a CGO_ENABLED=0 go build -trimpath -ldflags "$LDF" -o "$T/openmail-$a" ./cmd/openmail; done
mkdir -p "$T/cli" && lipo -create "$T/openmail-arm64" "$T/openmail-amd64" -output "$T/cli/openmail"
codesign --force --options runtime --timestamp --sign "$SIGN_ID" "$T/cli/openmail"
cp LICENSE README.md "$T/cli/"
ditto -c -k --keepParent "$T/cli" "$OUT/openmail-$VERSION-macos-universal.zip"
xcrun notarytool submit "$OUT/openmail-$VERSION-macos-universal.zip" --keychain-profile "$PROFILE" --wait

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
  cp LICENSE README.md "$d/"; tar -C "$d" -czf "$OUT/openmail-$VERSION-${t/\//-}.tar.gz" .
done
d="$T/windows"; mkdir -p "$d"; GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$LDF" -o "$d/openmail.exe" ./cmd/openmail
cp LICENSE README.md "$d/"; (cd "$d" && zip -q "$OLDPWD/$OUT/openmail-$VERSION-windows-amd64.zip" openmail.exe LICENSE README.md)

(cd "$OUT" && shasum -a 256 * > SHA256SUMS)
cat "$OUT/SHA256SUMS"
