#!/bin/bash
set -eu
BIN="$(dirname "$0")/../omarchy-nbd-bridge"
URL="http://127.0.0.1:8000/airootfs-full.sfs"
LISTEN="127.0.0.1:10809"
BRIDGE_PID=""

cleanup() {
    nbd-client -d /dev/nbd0 2>/dev/null || true
    [ -n "$BRIDGE_PID" ] && kill "$BRIDGE_PID" 2>/dev/null || true
}
trap cleanup EXIT

dmesg -c >/dev/null 2>&1 || true
modprobe nbd nbds_max=16

"$BIN" --url "$URL" --listen "$LISTEN" --cache-size 256M &
BRIDGE_PID=$!
for _ in $(seq 1 50); do nc -z 127.0.0.1 10809 2>/dev/null && break; sleep 0.1; done

nbd-client 127.0.0.1 10809 -N archiso /dev/nbd0

echo "== first 64 bytes direct from /dev/nbd0 via dd =="
dd if=/dev/nbd0 bs=64 count=1 status=none | xxd

echo "== first 64 bytes direct from the source file, for comparison =="
curl -s -H 'Range: bytes=0-63' "$URL" | xxd

echo "== bytes at a mid-file offset (10MB in), 64 bytes, via dd (skip in 64-byte blocks) =="
dd if=/dev/nbd0 bs=64 skip=163840 count=1 status=none | xxd

echo "== same offset direct from source =="
curl -s -H 'Range: bytes=10485760-10485823' "$URL" | xxd

echo "== attempting mount, capturing dmesg regardless of outcome =="
mkdir -p /mnt/bridge-diag
mount -t squashfs -o ro /dev/nbd0 /mnt/bridge-diag && echo "MOUNT OK" || echo "MOUNT FAILED"
umount /mnt/bridge-diag 2>/dev/null || true

echo "== dmesg tail =="
dmesg | tail -40
