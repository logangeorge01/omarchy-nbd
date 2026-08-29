#!/bin/bash
# Builds a custom UEFI-bootable iPXE ISO with autoboot.ipxe embedded --
# replaces netboot.xyz entirely. netboot.xyz was only ever providing one
# thing in this project: a way to reach an iPXE prompt (its own menu,
# binary, and asset mirror stopped being part of the actual boot chain a
# while ago, once omarchy-nbd.ipxe and everything it references moved to
# real static hosting). This IS our own iPXE, built from the same
# open-source project (github.com/ipxe/ipxe) netboot.xyz itself builds on,
# with our own script baked in at build time via iPXE's EMBED= mechanism
# -- boots straight to dhcp + chain, zero menu, zero manual keypresses,
# zero third-party dependency.
#
# Run inside WSL (or any Linux box) -- needs a normal C build toolchain
# plus a few packages this project's earlier Go/cgo work didn't already
# pull in:
#   sudo apt-get install -y build-essential liblzma-dev xorriso mtools \
#       genisoimage isolinux syslinux-common
#
# Usage: ./build-ipxe.sh [output-path]
#   defaults to ../omarchy-autoboot.iso (i.e. boot/omarchy-autoboot.iso)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${1:-$SCRIPT_DIR/omarchy-autoboot.iso}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "== cloning iPXE (shallow) =="
git clone --depth 1 https://github.com/ipxe/ipxe.git "$WORK/ipxe"

cp "$SCRIPT_DIR/autoboot.ipxe" "$WORK/autoboot.ipxe"

echo "== building bin-x86_64-efi/ipxe.iso with autoboot.ipxe embedded =="
make -C "$WORK/ipxe/src" bin-x86_64-efi/ipxe.iso "EMBED=$WORK/autoboot.ipxe"

cp "$WORK/ipxe/src/bin-x86_64-efi/ipxe.iso" "$OUT"
echo
echo "== done: $OUT =="
ls -la "$OUT"
echo
echo "Attach this as the VM's DVD instead of netboot.xyz.iso -- it boots"
echo "straight to dhcp + chain https://lagsoftware.com/files/omarchy-nbd.ipxe"
echo "with no menu and no manual keypresses. To change the target URL,"
echo "edit autoboot.ipxe and rebuild -- EMBED bakes the script in at"
echo "build time, it can't be edited after the fact the way the rest of"
echo "this project's iPXE scripts (fetched over HTTP at boot) can."
