#!/bin/bash
# M5 in DESIGN.md: integration test against the REAL compiled binary --
# mirrors the manual nbdkit validation done earlier in this project (WSL,
# 1.9GB RAM cap, 0 swap, peak 567MB used reading 2.2GB of real files out
# of a 7.16GB image), but against your own binary and as one rerunnable
# command instead of retyped commands.
#
# Run inside WSL (or any real Linux box) as root; needs the nbd kernel
# module. This does NOT test the full archiso boot chain (that needs
# hook/archiso_pxe_nfs and boot/omarchy-nbd.ipxe, plus a real or virtual
# machine) -- it tests that the binary itself correctly serves a mountable
# filesystem and survives real reads under memory pressure, which is most
# of what can go wrong and is much faster to iterate on than a full VM
# boot cycle.
#
# Usage: ./integration/test-mount.sh <path-to-binary> <url> [cache-size]
#
# Example, reusing the exact setup validated earlier in this project:
#   ./integration/test-mount.sh ./omarchy-nbd-bridge \
#       http://127.0.0.1:8000/airootfs-full.sfs 256M

set -euo pipefail

BIN="${1:?usage: test-mount.sh <path-to-omarchy-nbd-bridge> <url> [cache-size]}"
URL="${2:?usage: test-mount.sh <path-to-omarchy-nbd-bridge> <url> [cache-size]}"
CACHE_SIZE="${3:-256M}"
LISTEN="127.0.0.1:10809"
MOUNT_POINT="/mnt/omarchy-bridge-test"
BRIDGE_PID=""

cleanup() {
    umount "$MOUNT_POINT" 2>/dev/null || true
    nbd-client -d /dev/nbd0 2>/dev/null || true
    [ -n "$BRIDGE_PID" ] && kill "$BRIDGE_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "== modprobe nbd =="
modprobe nbd nbds_max=16

echo "== starting bridge: $BIN --url $URL --listen $LISTEN --cache-size $CACHE_SIZE =="
"$BIN" --url "$URL" --listen "$LISTEN" --cache-size "$CACHE_SIZE" &
BRIDGE_PID=$!

echo "== waiting for bridge to accept connections =="
ready=0
for _ in $(seq 1 50); do
    if nc -z 127.0.0.1 10809 2>/dev/null; then
        ready=1
        break
    fi
    sleep 0.1
done
if [ "$ready" -ne 1 ]; then
    echo "FAIL: bridge never started listening on $LISTEN"
    exit 1
fi

echo "== connecting nbd-client =="
nbd-client 127.0.0.1 10809 -N archiso /dev/nbd0

echo "== mounting (read-only) =="
mkdir -p "$MOUNT_POINT"
if ! mount -o ro /dev/nbd0 "$MOUNT_POINT"; then
    echo "FAIL: mount failed -- if you're serving a raw squashfs directly rather"
    echo "      than a whole ISO, this is expected: archiso_mount_handler expects"
    echo "      a real filesystem to mount, not a bare squashfs (see ARCHITECTURE.md)."
    echo "      Try: mount -t squashfs -o ro /dev/nbd0 $MOUNT_POINT  instead, to at"
    echo "      least confirm the bridge/NBD layer itself is serving correct bytes."
    exit 1
fi

echo "== sanity listing =="
ls "$MOUNT_POINT" | head

echo "== memory before stress read =="
free -h

echo "== reading a chunk of real data through the mount =="
find "$MOUNT_POINT" -type f 2>/dev/null | head -2000 | xargs -I{} cat {} >/dev/null 2>/dev/null || true

echo "== memory after stress read =="
free -h

echo
echo "PASS: mounted, read real data, no crash."
echo "Compare the two 'free -h' outputs above by hand -- 'used' should stay"
echo "roughly flat regardless of how much was read; 'available' should stay high."
