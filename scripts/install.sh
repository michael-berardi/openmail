#!/bin/sh
# OpenMail installer for macOS and Linux: curl -fsSL https://github.com/michael-berardi/openmail/releases/latest/download/install.sh | sh
# Downloads a release, verifies it against SHA256SUMS, installs the CLI into ~/.local/share/openmail/<version> and puts an
# `openmail` shim in ~/.local/bin. Pass --app on macOS to install the verified OpenMail.app into ~/Applications as well.
# Env overrides: OPENMAIL_VERSION (pin), OPENMAIL_RELEASE_BASE (local/mirror base URL, used by tests),
# OPENMAIL_INSTALL_DIR, OPENMAIL_BIN_DIR and OPENMAIL_APPS_DIR (default ~/Applications on macOS).
set -eu

REPO="michael-berardi/openmail"
say() { printf '%s\n' "$*"; }
die() { printf 'openmail install: %s\n' "$*" >&2; exit 1; }

APP_MODE=0
if [ "$#" -gt 0 ]; then
  case "$1" in
    --app) APP_MODE=1 ;;
    -h|--help)
      say "usage: install.sh [--app]"
      say "  --app  on macOS, also install OpenMail.app into ~/Applications"
      exit 0 ;;
    *) die "unknown argument: $1 (usage: install.sh [--app])" ;;
  esac
  [ "$#" -eq 1 ] || die "usage: install.sh [--app]"
fi

abspath() { case "$1" in /*) printf '%s\n' "$1" ;; *) printf '%s\n' "$PWD/$1" ;; esac; }
INSTALL_PARENT=$(abspath "${OPENMAIL_INSTALL_DIR:-$HOME/.local/share/openmail}")
BIN_DIR=$(abspath "${OPENMAIL_BIN_DIR:-$HOME/.local/bin}")
APPS_DIR=$(abspath "${OPENMAIL_APPS_DIR:-$HOME/Applications}")
if [ -n "${OPENMAIL_RELEASE_BASE:-}" ]; then BASE="${OPENMAIL_RELEASE_BASE%/}"
elif [ -n "${OPENMAIL_VERSION:-}" ]; then BASE="https://github.com/$REPO/releases/download/v${OPENMAIL_VERSION#v}"
else BASE="https://github.com/$REPO/releases/latest/download"; fi

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

case "$(uname -s)" in
  Darwin) os=macos ;;
  Linux) os=linux ;;
  *) die "unsupported system $(uname -s); on Windows use install.ps1" ;;
esac
if [ "$APP_MODE" -eq 1 ] && [ "$os" != macos ]; then die "--app is only supported on macOS"; fi
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac

TMP=$(mktemp -d "${TMPDIR:-/tmp}/openmail-install.XXXXXX") || die "cannot create a temporary folder"
stage=""; backup=""; app_backup=""
cleanup() {
  rm -rf "$TMP"
  [ -z "$stage" ] || rm -rf "$stage"
  [ -z "$backup" ] || rm -rf "$backup"
  [ -z "$app_backup" ] || rm -rf "$app_backup"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# expect <file> <exact-asset-name> <sums-file>: abort unless the file matches that named line.
expect() {
  want=$(awk -v n="$2" '{ f=$2; sub(/^\*/, "", f); if (f == n) { print $1; exit } }' "$3")
  case "$want" in ''|*[!0-9a-fA-F]*) die "no usable checksum for $2 in SHA256SUMS" ;; esac
  got=$(sha256 "$1")
  [ "$got" = "$want" ] || die "checksum mismatch for $2; nothing was installed"
}
# pick <awk-regex>: return the exact asset name listed in SHA256SUMS.
pick() { awk -v pat="$1" '{ f=$2; sub(/^\*/, "", f); if (f ~ pat) { print f; exit } }' "$TMP/SHA256SUMS"; }
check_names() { # abort on absolute or parent-escaping archive members
  awk '{
    p = $0
    sub(/^\.\//, "", p)
    if (p ~ /^\// || p ~ /(^|\/)\.\.(\/|$)/) { print "unsafe archive path: " p > "/dev/stderr"; bad = 1 }
  } END { exit bad }' "$1" || die "archive contains unsafe paths"
}
check_version() { # check_version <binary> <version>
  out=$("$1" version 2>&1 </dev/null) || die "the downloaded CLI does not run: $out"
  case "$out" in "openmail $2"*|"openmail v$2"*) ;; *) die "the downloaded CLI reports '$out', not $2" ;; esac
}

if [ "$os" = macos ]; then
  cli_pattern='^openmail-cli-v[0-9]+[.][0-9]+[.][0-9]+-macos-universal[.]zip$'
  app_pattern='^OpenMail-v[0-9]+[.][0-9]+[.][0-9]+-macos-universal[.]zip$'
else
  cli_pattern="^openmail-cli-v[0-9]+[.][0-9]+[.][0-9]+-linux-${arch}[.]tar[.]gz$"
fi

say "Downloading OpenMail release metadata..."
fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS"
cli_asset=$(pick "$cli_pattern")
[ -n "$cli_asset" ] || die "no CLI asset for $os-${arch} in SHA256SUMS"
case "$cli_asset" in
  *-macos-universal.zip) version=${cli_asset#openmail-cli-v}; version=${version%-macos-universal.zip} ;;
  *-linux-*.tar.gz) version=${cli_asset#openmail-cli-v}; version=${version%-linux-*.tar.gz} ;;
  *) die "unexpected CLI asset name: $cli_asset" ;;
esac
case "$version" in [0-9]*.[0-9]*.[0-9]*) ;; *) die "cannot read version from $cli_asset" ;; esac
if [ -n "${OPENMAIL_VERSION:-}" ]; then
  requested=${OPENMAIL_VERSION#v}
  [ "$version" = "$requested" ] || die "release $version does not match requested version $requested"
fi

if [ "$APP_MODE" -eq 1 ]; then
  app_asset=$(pick "$app_pattern")
  [ -n "$app_asset" ] || die "no macOS app asset in SHA256SUMS"
  app_version=${app_asset#OpenMail-v}; app_version=${app_version%-macos-universal.zip}
  [ "$app_version" = "$version" ] || die "app asset version $app_version differs from CLI version $version"
fi

# Refuse foreign files before downloading or changing anything.
version_dir="$INSTALL_PARENT/$version"
shim="$BIN_DIR/openmail"
if [ -e "$version_dir" ] || [ -L "$version_dir" ]; then
  [ -f "$version_dir/.openmail-installed" ] || die "refusing to replace $version_dir: this installer did not create it"
  [ "$(sed -n '1p' "$version_dir/.openmail-installed" 2>/dev/null || true)" = 'OPENMAIL_INSTALLER_V1' ] ||
    die "refusing to replace $version_dir: invalid install marker"
fi
if [ -e "$shim" ] || [ -L "$shim" ]; then
  if [ ! -f "$shim" ] || [ -L "$shim" ] || ! grep -Fxq '# openmail-installer-shim' "$shim"; then
    die "refusing to replace $shim: this installer did not create it"
  fi
fi
if [ "$APP_MODE" -eq 1 ]; then
  app_dir="$APPS_DIR/OpenMail.app"
  app_marker="$INSTALL_PARENT/.app-installed"
  if [ -e "$app_dir" ] || [ -L "$app_dir" ]; then
    [ -f "$app_marker" ] || die "refusing to replace $app_dir: this installer did not create it"
    [ "$(sed -n '1p' "$app_marker" 2>/dev/null || true)" = "$app_dir" ] ||
      die "refusing to replace $app_dir: invalid app install marker"
  fi
fi

say "Downloading $cli_asset..."
fetch "$BASE/$cli_asset" "$TMP/$cli_asset"
expect "$TMP/$cli_asset" "$cli_asset" "$TMP/SHA256SUMS"
if [ "$APP_MODE" -eq 1 ]; then
  say "Downloading $app_asset..."
  fetch "$BASE/$app_asset" "$TMP/$app_asset"
  expect "$TMP/$app_asset" "$app_asset" "$TMP/SHA256SUMS"
fi

mkdir -p "$TMP/cli"
if [ "$os" = macos ]; then
  command -v unzip >/dev/null 2>&1 || die "needs unzip"
  unzip -Z1 "$TMP/$cli_asset" > "$TMP/cli.list" || die "cannot list the CLI archive"
else
  command -v tar >/dev/null 2>&1 || die "needs tar"
  tar -tzf "$TMP/$cli_asset" > "$TMP/cli.list" || die "cannot list the CLI archive"
fi
check_names "$TMP/cli.list"
if [ "$os" = macos ]; then
  unzip -q "$TMP/$cli_asset" -d "$TMP/cli" || die "cannot unpack the CLI archive"
else
  tar -xzf "$TMP/$cli_asset" -C "$TMP/cli" || die "cannot unpack the CLI archive"
fi
src_bin=$(find "$TMP/cli" -type f -name openmail | sed -n 1p)
[ -n "$src_bin" ] || die "the CLI archive does not contain an openmail binary"

mkdir -p "$INSTALL_PARENT" "$BIN_DIR"
stage="$INSTALL_PARENT/.openmail-install.$$"
[ ! -e "$stage" ] && [ ! -L "$stage" ] || die "temporary folder already exists: $stage"
mkdir "$stage"
cp "$src_bin" "$stage/openmail"
chmod 755 "$stage/openmail"
printf 'OPENMAIL_INSTALLER_V1\n' > "$stage/.openmail-installed"
for doc in LICENSE README.md; do
  doc_src=$(find "$TMP/cli" -type f -name "$doc" | sed -n 1p)
  [ -z "$doc_src" ] || cp "$doc_src" "$stage/$doc"
done
check_version "$stage/openmail" "$version"

if [ -e "$version_dir" ] || [ -L "$version_dir" ]; then
  backup="$INSTALL_PARENT/.openmail-old.$$"
  [ ! -e "$backup" ] && [ ! -L "$backup" ] || die "backup folder already exists: $backup"
  mv "$version_dir" "$backup" || die "cannot move the previous installation aside"
fi
if ! mv "$stage" "$version_dir"; then
  [ -z "$backup" ] || { mv "$backup" "$version_dir" && backup=""; }
  die "cannot install version $version"
fi
stage=""
if [ -n "$backup" ]; then rm -rf "$backup"; backup=""; fi

shim_tmp="$BIN_DIR/.openmail-shim.$$"
cat > "$shim_tmp" <<SHIM
#!/bin/sh
# openmail-installer-shim
exec "$version_dir/openmail" "\$@"
SHIM
chmod 755 "$shim_tmp"
mv -f "$shim_tmp" "$shim" || { rm -f "$shim_tmp"; die "cannot install the shim $shim"; }
check_version "$shim" "$version"
say "OpenMail $version installed: $version_dir/openmail (shim: $shim)"

if [ "$APP_MODE" -eq 1 ]; then
  mkdir -p "$TMP/app"
  unzip -Z1 "$TMP/$app_asset" > "$TMP/app.list" || die "cannot list the app archive"
  check_names "$TMP/app.list"
  unzip -q "$TMP/$app_asset" -d "$TMP/app" || die "cannot unpack the app archive"
  [ -x "$TMP/app/OpenMail.app/Contents/MacOS/OpenMail" ] || die "the app archive is not an OpenMail.app bundle"
  mkdir -p "$APPS_DIR"
  if [ -e "$app_dir" ] || [ -L "$app_dir" ]; then
    app_backup="${app_dir}.openmail-old.$$"
    [ ! -e "$app_backup" ] && [ ! -L "$app_backup" ] || die "backup app already exists: $app_backup"
    mv "$app_dir" "$app_backup" || die "cannot move the previous app aside"
  fi
  if ! mv "$TMP/app/OpenMail.app" "$app_dir"; then
    [ -z "$app_backup" ] || { mv "$app_backup" "$app_dir" && app_backup=""; }
    die "cannot install the macOS app"
  fi
  printf '%s\n' "$app_dir" > "$app_marker" || die "cannot record the app installation"
  # The new app is in place: drop the previous copy so no backup is left in Applications.
  if [ -n "$app_backup" ]; then rm -rf "$app_backup"; app_backup=""; fi
  say "OpenMail.app installed: $app_dir"
fi

case ":$PATH:" in *":$BIN_DIR:"*) ;; *) say "Add $BIN_DIR to your PATH:  export PATH=\"$BIN_DIR:\$PATH\"" ;; esac
