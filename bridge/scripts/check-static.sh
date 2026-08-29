#!/bin/sh
# M4 in DESIGN.md: verifies the compiled binary is fully static (no
# dynamic library dependencies at all -- required to be droppable into
# the initramfs with zero package manager available) and reports its
# size so you can track it as you add features.
#
# Usage: ./scripts/check-static.sh path/to/omarchy-nbd-bridge
#
# A correct build command for this is something like:
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o omarchy-nbd-bridge ./cmd/omarchy-nbd-bridge

set -eu

bin="${1:?usage: check-static.sh path/to/binary}"

echo "== file type =="
file "$bin"

echo
echo "== dynamic dependencies (want: none) =="
if ldd "$bin" 2>&1 | grep -q "not a dynamic executable"; then
    echo "OK: fully static"
else
    echo "FAIL: binary has dynamic dependencies -- won't run in the initramfs, which has no dynamic linker for anything beyond what's already there:"
    ldd "$bin" || true
    exit 1
fi

echo
echo "== size =="
ls -lh "$bin"
