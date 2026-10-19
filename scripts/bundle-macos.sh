#!/bin/bash
# Build dist/OpenMail.app (arm64 or the host architecture) from cmd/openmail-desktop.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)}"
APP=dist/OpenMail.app
rm -rf "$APP" && mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

build() { # arch output
  CGO_ENABLED=1 GOARCH="$1" CC="clang -arch $([ "$1" = amd64 ] && echo x86_64 || echo arm64)" go build -tags production -trimpath \
    -ldflags "-s -w -X github.com/michael-berardi/openmail/internal/core.Version=$VERSION" \
    -o "$2" ./cmd/openmail-desktop
}
if [ "${UNIVERSAL:-0}" = 1 ]; then
  T=$(mktemp -d); build arm64 "$T/arm64"; build amd64 "$T/amd64"
  lipo -create "$T/arm64" "$T/amd64" -output "$APP/Contents/MacOS/OpenMail"; rm -rf "$T"
else
  build "$(go env GOARCH)" "$APP/Contents/MacOS/OpenMail"
fi

ICONSET=$(mktemp -d)/AppIcon.iconset && mkdir -p "$ICONSET"
for s in 16 32 64 128 256 512 1024; do rsvg-convert -w $s -h $s assets/icon.svg -o "$ICONSET/p$s.png"; done
cp "$ICONSET/p16.png"   "$ICONSET/icon_16x16.png";      cp "$ICONSET/p32.png"  "$ICONSET/icon_16x16@2x.png"
cp "$ICONSET/p32.png"   "$ICONSET/icon_32x32.png";      cp "$ICONSET/p64.png"  "$ICONSET/icon_32x32@2x.png"
cp "$ICONSET/p128.png"  "$ICONSET/icon_128x128.png";    cp "$ICONSET/p256.png" "$ICONSET/icon_128x128@2x.png"
cp "$ICONSET/p256.png"  "$ICONSET/icon_256x256.png";    cp "$ICONSET/p512.png" "$ICONSET/icon_256x256@2x.png"
cp "$ICONSET/p512.png"  "$ICONSET/icon_512x512.png";    cp "$ICONSET/p1024.png" "$ICONSET/icon_512x512@2x.png"
rm "$ICONSET"/p*.png
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>OpenMail</string>
  <key>CFBundleDisplayName</key><string>OpenMail</string>
  <key>CFBundleIdentifier</key><string>com.libertydesign.openmail</string>
  <key>CFBundleExecutable</key><string>OpenMail</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${VERSION#v}</string>
  <key>CFBundleVersion</key><string>$VERSION</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.productivity</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSHumanReadableCopyright</key><string>MIT License</string>
  <key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/></dict>
  <key>CFBundleURLTypes</key><array><dict>
    <key>CFBundleURLName</key><string>Email</string>
    <key>CFBundleURLSchemes</key><array><string>mailto</string></array>
  </dict></array>
</dict></plist>
PLIST
# macOS re-tags freshly written files with com.apple.provenance, which codesign rejects; clear and retry.
for try in 1 2 3 4 5; do
  xattr -cr "$APP"
  if [ -n "${SIGN_ID:-}" ]; then
    codesign --force --deep --options runtime --timestamp --sign "$SIGN_ID" "$APP" >/dev/null 2>&1 && break
  else
    codesign --force --deep --sign - "$APP" >/dev/null 2>&1 && break
  fi
  [ "$try" = 5 ] && { echo "codesign failed after 5 attempts" >&2; exit 1; }
  sleep 1
done
echo "built $APP ($(du -sh "$APP" | cut -f1))"
