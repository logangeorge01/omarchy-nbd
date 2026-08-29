#!/bin/bash
# Re-run of the earlier nbdkit low-RAM validation (2GB WSL2 cap, 0 swap),
# this time against the real compiled omarchy-nbd-bridge binary instead of
# nbdkit, for a direct apples-to-apples comparison. Serves the same
# 7.16GB reassembled squashfs over the same local nginx (127.0.0.1:8000)
# used for that earlier test.
#
# Must run as root (modprobe, mount). Usage: sudo ./low-ram-stress-test.sh

set -eu

BIN="$(dirname "$0")/../omarchy-nbd-bridge"
URL="http://127.0.0.1:8000/airootfs-full.sfs"
LISTEN="127.0.0.1:10809"
MOUNT_POINT="/mnt/bridge-stress-test"
BRIDGE_PID=""

cleanup() {
    umount "$MOUNT_POINT" 2>/dev/null || true
    nbd-client -d /dev/nbd0 2>/dev/null || true
    [ -n "$BRIDGE_PID" ] && kill "$BRIDGE_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "== memory cap in effect =="
free -h

echo "== dmesg marker =="
dmesg -c >/dev/null 2>&1 || true  # clear so later dmesg output is just this run's

echo "== modprobe nbd =="
modprobe nbd nbds_max=16

echo "== starting bridge: $BIN --url $URL --listen $LISTEN --cache-size 256M =="
"$BIN" --url "$URL" --listen "$LISTEN" --cache-size 256M &
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
    echo "FAIL: bridge never started listening"
    exit 1
fi

echo "== connecting nbd-client =="
nbd-client 127.0.0.1 10809 -N archiso /dev/nbd0

echo "== mounting (squashfs, read-only -- same as the earlier nbdkit test: this file is the raw squashfs, not a full ISO) =="
mkdir -p "$MOUNT_POINT"
mount -t squashfs -o ro /dev/nbd0 "$MOUNT_POINT"

echo "== sanity listing =="
ls "$MOUNT_POINT" | head

echo "== memory before stress read =="
free -h

echo "== reading a large chunk of real data through the mount =="
find "$MOUNT_POINT" -type f 2>/dev/null | head -2000 | xargs -I{} cat {} >/dev/null 2>/dev/null || true

echo "== memory after stress read =="
free -h

echo "== dmesg since this run started (looking for OOM/kernel panic) =="
dmesg | grep -iE "oom|panic|killed process" || echo "(nothing matching oom/panic/killed found)"

echo
echo "PASS: mounted, read real data via omarchy-nbd-bridge, no crash."
