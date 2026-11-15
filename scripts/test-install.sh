#!/bin/sh
# Exercise installation without compiling or touching the user's home directory.
set -eu
cd "$(dirname "$0")/.."
work=$(mktemp -d "./.test-install.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
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
