# Architecture — Omarchy network boot on low-RAM machines

## The problem this solves

Omarchy's live squashfs is ~7.16GB. The standard archiso HTTP netboot hook
(`archiso_pxe_http`) downloads the *entire* squashfs into a tmpfs before boot
can proceed — hardcoded, not configurable (see `copytoram="y"` forced in the
hook source). That guarantees OOM/kernel panic on anything without ~8GB+ free
RAM, regardless of how little of the live system you actually touch during a
session.

This project replaces that one hook with an NBD-backed equivalent: the squashfs
is mounted as a real block device, and only the bytes actually read (page by
page, on demand) are ever fetched — with a small bounded cache, never a full
copy. Validated in WSL2 capped at 1.9GB RAM / 0 swap: peak usage 567MB while
reading 2.2GB worth of real files out of the 7.16GB image. No OOM.

## Boot sequence, end to end

**This section is verified against Omarchy's actual initramfs, not inferred.**
We downloaded the real `initrd`, extracted it (it's two concatenated cpio
segments — an early microcode blob, then the real zstd-compressed archiso
initramfs), and read `/config`, `/init`, `archiso_pxe_nbd`, `archiso`
(the generic mount-handler hook), and `archiso_loop_mnt` directly. Two things
that changed from the original plan as a result:

1. **The NBD export must be the whole ISO, not just the squashfs.**
   `archiso_mount_handler` does `mount <archisodevice> /run/archiso/bootmnt`
   (mounts it as a real filesystem) and then looks for
   `arch/x86_64/airootfs.sfs` as a *file inside it* — exactly what it does
   for a real USB stick. Exporting the raw squashfs bytes alone wouldn't be
   mountable as anything meaningful.
2. **No custom mount-handling hook is needed at all.** `archiso_pxe_nbd`
   is already present, unmodified, in Omarchy's real initramfs — its name is
   already in `/config`'s `HOOKS` list, so it just runs. It does the entire
   `nbd-client` connect + `archisodevice=/dev/nbd0` + handoff to the stock
   `archiso_mount_handler`, and it already respects `copytoram=n` (confirmed
   in its source — unlike the HTTP hook, which hardcodes `copytoram="y"` no
   matter what you pass). Our only job is getting a bridge running and
   reachable before it tries to connect.

```
Power on
  │
  ▼
UEFI/BIOS → iPXE  (netboot.xyz's ipxe.efi/undionly.kpxe, or your own build later)
  │
  ▼
chain https://<you>.github.io/omarchy-netboot/boot/omarchy-nbd.ipxe
  │  (hosted on YOUR GitHub Pages — this repo)
  ▼
kernel  <omarchy-vmlinuz-url>                                 ← Omarchy's real kernel, unmodified
initrd  <omarchy-initrd-url>                                  ← Omarchy's real base initramfs, unmodified
initrd  <hook-url>   /hooks/archiso_pxe_nfs      mode=755     ← this repo, injected (see note below)
initrd  <bridge-url> /usr/local/bin/omarchy-nbd-bridge mode=755 ← this repo, injected
  │
  ▼
Linux kernel boots, unions all initrd images into one initramfs
  │
  ▼
/init sources /config's $HOOKS list, runs each hook's run_hook() in order:
  │
  ├─ archiso_pxe_nfs (OUR file, repurposing this already-wired-in slot —
  │    see hook/archiso_pxe_nfs for why this slot specifically): starts
  │    omarchy-nbd-bridge in the background, waits until it's actually
  │    accepting connections before returning
  │
  └─ archiso_pxe_nbd (STOCK, unmodified — already ships in Omarchy's
       initramfs, we never touch it): sets mount_handler to its own
       function, to run later
  │
  ▼
/init calls "$mount_handler" /new_root  (later, separate step)
  │
  ▼
archiso_pxe_nbd_mount_handler:
  nbd-client 127.0.0.1 10809 -N archiso /dev/nbd0
  archisodevice=/dev/nbd0
  archiso_mount_handler (stock, untouched) →
    mount /dev/nbd0 /run/archiso/bootmnt        (as a real filesystem)
    finds arch/x86_64/airootfs.sfs inside it
    loop-mounts it read-only (copytoram=n → no RAM copy, reads flow through
    the bridge on demand, exactly as validated in the WSL stress test)
  │
  ▼
Omarchy live environment boots normally
```

**Nothing about Omarchy's ISO, kernel, initramfs, or squashfs is modified.**
Nothing about netboot.xyz's own infrastructure is touched either. Everything
here is net-new files, hosted by us, injected via iPXE's multi-`initrd`
mechanism — the same trick netboot.xyz already uses to give Omarchy the
stock `archiso_pxe_http` hook it doesn't ship natively. The one asterisk:
we do overwrite the *content* of the `archiso_pxe_nfs` hook slot (a name
that was already going to run regardless), which means this particular
boot image loses the ability to NFS-netboot — an irrelevant tradeoff here,
just worth remembering.

## Where things get hosted

| Artifact | Size | Where | Why |
|---|---|---|---|
| `omarchy-nbd.ipxe` (boot script) | <1KB | **GitHub Pages** (this repo) | Tiny, plain text, exactly what Pages is for |
| `archiso_pxe_nbd_bridge` (hook script) | <5KB | **GitHub Pages** (this repo) | Same |
| `omarchy-nbd-bridge` (compiled binary) | est. 2-10MB static | **GitHub Pages** (this repo) | Small enough for a normal git repo, no LFS needed |
| Omarchy `vmlinuz` / base `initrd` | tens/hundreds of MB | netboot.xyz's `asset-mirror` (unchanged, already proven) | No reason to re-host what already works |
| Omarchy's whole **ISO** | **~7GB+** | **NOT GitHub Pages, NOT GitHub Releases either** — see below | Needs one Range-capable object, no split |

The whole ISO (bigger than just the squashfs — see the correction above,
we need the whole thing now, not just `airootfs.sfs`) needs a different
home than the rest of this project. GitHub Releases specifically is a worse
fit here than it was for testing the squashfs alone: that only worked
because netboot.xyz's `asset-mirror` already did the annoying part (split
into 4 parts under GitHub's 2GB/asset cap, which our bridge would then have
to know how to stitch back together — solvable, but pointless complexity
when there's a simpler option). **Azure Blob Storage (a single block blob)
is the right home for this one** — no 2GB split, one URL, `Range` requests
work natively. This is purely a hosting choice; the bridge just needs a URL
that supports `Range`, it doesn't care what's serving it.

## Repo layout

```
omarchy-netboot/
  boot/
    omarchy-nbd.ipxe          ← the chainloaded boot script
  hook/
    archiso_pxe_nbd_bridge    ← archiso initcpio hook (shell)
  bridge/
    DESIGN.md                 ← detailed spec for the binary (see this first)
    (your source lives here)
  docs/
    ARCHITECTURE.md           ← this file
```
