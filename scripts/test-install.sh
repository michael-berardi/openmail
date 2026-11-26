#!/bin/sh
# Exercise installation without compiling or touching the user's home directory.
# Also runs install.sh end to end against a local release directory assembled from published assets.
set -eu
cd "$(dirname "$0")/.."
work=$(mktemp -d "./.test-install.XXXXXX")
release_work=""
server_pid=""
cleanup() {
  [ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
  rm -rf "$work"
  [ -z "$release_work" ] || rm -rf "$release_work"
}
trap cleanup EXIT HUP INT TERM
cp Makefile "$work/Makefile"
mkdir -p "$work/bin"
printf '#!/bin/sh\nprintf "fixture\\n"\n' > "$work/bin/openmail"
home="$PWD/$work/home with spaces"
make -s -C "$work" -o build install HOME="$home"
test -x "$home/.local/bin/openmail"
test "$("$home/.local/bin/openmail")" = fixture
# Reinstalling must replace the existing binary as well.
printf '#!/bin/sh\nprintf "updated\\n"\n' > "$work/bin/openmail"
make -s -C "$work" -o build install HOME="$home"
test "$("$home/.local/bin/openmail")" = updated
printf 'install: fresh home, spaces and reinstall passed\n'

fail() { printf 'release installer test: %s\n' "$*" >&2; exit 1; }
run_installer() { # run_installer <home> [installer args...]
  h=$1; shift
  HOME="$h" OPENMAIL_VERSION="$version" sh "$PWD/scripts/install.sh" "$@" > "$release_work/last.log" 2>&1 ||
    { cat "$release_work/last.log" >&2; fail "installer failed for $h"; }
}
check_shim() { # check_shim <shim>
  got=$("$1" version 2>&1 </dev/null) || fail "$1 version failed"
  case "$got" in "openmail $version"*|"openmail v$version"*) ;; *) fail "$1 reports '$got'" ;; esac
}

test_release_installer() {
  version="${OPENMAIL_TEST_VERSION:-0.1.1}"
  for tool in curl unzip python3; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'release installer test skipped: %s missing\n' "$tool"; return 0; }
  done
  release_work=$(mktemp -d "./.test-release.XXXXXX")
  release_root="$PWD/$release_work"
  ./scripts/build-release.sh "$version" "$release_root/release" > "$release_root/build.log" 2>&1 ||
    { cat "$release_root/build.log" >&2; fail "could not assemble local release"; }

  port_file="$release_root/port"
  ( cd "$release_work/release" && exec python3 -c '
import http.server, socketserver, sys
server = socketserver.TCPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler)
with open(sys.argv[1], "w") as f:
    f.write(str(server.server_address[1]) + "\n")
server.serve_forever()
' "$port_file" ) > "$release_work/http.log" 2>&1 &
  server_pid=$!
  tries=0
  while [ ! -s "$port_file" ] && [ "$tries" -lt 100 ]; do sleep 0.1; tries=$((tries + 1)); done
  [ -s "$port_file" ] || { cat "$release_work/http.log" >&2; fail "local release server did not start"; }
  OLD_BASE="${OPENMAIL_RELEASE_BASE:-}"
  OPENMAIL_RELEASE_BASE="http://127.0.0.1:$(cat "$port_file")"
  export OPENMAIL_RELEASE_BASE

  install_home="$release_root/home with spaces"; mkdir -p "$install_home"
  run_installer "$install_home"
  check_shim "$install_home/.local/bin/openmail"
  test -x "$install_home/.local/share/openmail/$version/openmail" || fail "versioned CLI not installed"
  [ "$(sed -n '1p' "$install_home/.local/share/openmail/$version/.openmail-installed")" = OPENMAIL_INSTALLER_V1 ] ||
    fail "missing install marker"
  # Reinstalling our own version is allowed.
  run_installer "$install_home"
  check_shim "$install_home/.local/bin/openmail"

  foreign_home="$release_root/foreign"; mkdir -p "$foreign_home/.local/bin"
  printf '#!/bin/sh\necho hand-installed\n' > "$foreign_home/.local/bin/openmail"
  if HOME="$foreign_home" OPENMAIL_VERSION="$version" sh "$PWD/scripts/install.sh" > "$release_work/refuse.log" 2>&1; then
    fail "installer replaced a shim it did not create"
  fi
  grep -q 'refusing to replace' "$release_work/refuse.log" || fail "unexpected refusal message"

  if [ "$(uname -s)" = Darwin ]; then
    app_home="$release_root/app home"; mkdir -p "$app_home"
    run_installer "$app_home" --app
    check_shim "$app_home/.local/bin/openmail"
    test -x "$app_home/Applications/OpenMail.app/Contents/MacOS/OpenMail" || fail "OpenMail.app not installed"
    [ "$(sed -n '1p' "$app_home/.local/share/openmail/.app-installed")" = "$app_home/Applications/OpenMail.app" ] ||
      fail "missing app install marker"
    foreign_app_home="$release_root/foreign-app"; mkdir -p "$foreign_app_home/Applications/OpenMail.app"
    if HOME="$foreign_app_home" OPENMAIL_VERSION="$version" sh "$PWD/scripts/install.sh" --app > "$release_work/refuse-app.log" 2>&1; then
      fail "installer replaced an app it did not create"
    fi
    grep -q 'refusing to replace' "$release_work/refuse-app.log" || fail "unexpected app refusal message"
    printf 'release installer test: --app, reinstall and foreign-file refusal passed\n'
  fi
  if [ -n "$OLD_BASE" ]; then OPENMAIL_RELEASE_BASE="$OLD_BASE"; else unset OPENMAIL_RELEASE_BASE; fi
  kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  server_pid=""
  printf 'release installer test: local release dir, SHA-256, versioned install and shim passed\n'
}

test_release_installer
