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
UEFI → our own iPXE (boot/omarchy-autoboot.iso, built by boot/build-ipxe.sh
  │     from the real open-source ipxe.org project, our own dhcp+chain
  │     script baked in at build time -- no netboot.xyz involved at all)
  ▼
chain https://lagsoftware.com/files/omarchy-nbd.ipxe
  │  (real static hosting -- GitHub Pages, this repo's files pushed there)
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
Everything injected is net-new files, hosted by us, added via iPXE's
multi-`initrd` mechanism — the same trick netboot.xyz's own menu scripts
use to give Omarchy the stock `archiso_pxe_http` hook it doesn't ship
natively (that's where this project first learned the trick, back when it
still depended on netboot.xyz's menu — see "Getting into iPXE" below for
why that dependency is gone now). The one asterisk: we do overwrite the
*content* of the `archiso_pxe_nfs` hook slot (a name that was already
going to run regardless), which means this particular boot image loses
the ability to NFS-netboot — an irrelevant tradeoff here, just worth
remembering.

## Getting into iPXE

Early versions of this project reached an iPXE prompt via netboot.xyz's
own ISO/menu, then manually `chain`ed to `omarchy-nbd.ipxe` from there.
That was only ever a development convenience — once `omarchy-nbd.ipxe`
and everything it references moved onto real static hosting, netboot.xyz
wasn't providing anything except "a way to reach an `iPXE>` prompt."

`boot/build-ipxe.sh` removes that dependency entirely: it builds iPXE
straight from the real open-source project (`github.com/ipxe/ipxe`, the
same one netboot.xyz itself builds on) with `boot/autoboot.ipxe` — just
`dhcp` then `chain` straight to our own hosted boot script — baked in at
build time via iPXE's `EMBED=` mechanism. The result,
`boot/omarchy-autoboot.iso` (checked into this repo, ~2MB), boots directly
to our chain with no menu and no manual keypresses, and no third-party
infrastructure in the path at all. Attach it as boot media (virtual DVD
for VM testing; a real USB stick for real hardware) in place of
netboot.xyz's ISO.

Rebuild it whenever `autoboot.ipxe`'s target URL changes — `EMBED` bakes
the script into the binary, unlike the rest of this project's iPXE scripts
(fetched fresh over HTTP at boot), so editing `autoboot.ipxe` alone has no
effect until `build-ipxe.sh` runs again.

## Where things get hosted

| Artifact | Size | Where | Why |
|---|---|---|---|
| `omarchy-autoboot.iso` (custom iPXE, embedded script) | ~2MB | this repo (`boot/`) | Small enough for a normal git repo; see "Getting into iPXE" above |
| `omarchy-nbd.ipxe` (boot script) | <1KB | `lagsoftware.com/files` (GitHub Pages) | Tiny, plain text, exactly what Pages is for |
| `archiso_pxe_nfs` (hook script) | <5KB | `lagsoftware.com/files` (GitHub Pages) | Same |
| `omarchy-nbd-bridge` (compiled binary) | ~6MB static | `lagsoftware.com/files` (GitHub Pages) | Small enough for a normal git repo, no LFS needed |
| Omarchy `vmlinuz` / base `initrd` | ~270MB combined | a GitHub Release (this repo) | `initrd` alone is over GitHub's 100MB per-file *push* limit — Release assets aren't subject to that cap (2GB/asset), and are served with real Range support (confirmed: backed by Azure Blob under the hood despite the `github.com` URL) |
| Omarchy's whole **ISO** | **~6GB+** | `iso.omarchy.org` — **Omarchy's own official host, untouched** | Confirmed real `Range`/206 support; no reason to re-host a file this size ourselves when the upstream project already serves it correctly |

Notably, **nothing here runs on a server we maintain.** Every piece is
either committed to this repo, sitting on GitHub Pages/Releases, or
Omarchy's own official download host. The only thing that has to exist
per-boot is the bridge process itself, and that's launched by our injected
hook inside the target machine's own initramfs — not hosted anywhere.

`vmlinuz`/`initrd` must always be extracted from the *exact same* Omarchy
release the `iso_url` in `omarchy-nbd.ipxe` points at — confirmed the hard
way that a version mismatch (a different kernel build than what the
squashfs's own `/usr/lib/modules/` was built for) boots into a live shell
fine but breaks partway through a real install (`dm_mod`/other modules for
the running kernel simply don't exist in the squashfs). Re-extract and
re-upload both whenever `iso_url`'s version changes.

## Repo layout

```
omarchy-netboot/
  boot/
    omarchy-nbd.ipxe          ← the chainloaded boot script (real: lagsoftware.com/files)
    autoboot.ipxe             ← embedded into our own iPXE build, see build-ipxe.sh
    build-ipxe.sh             ← builds omarchy-autoboot.iso from real upstream iPXE source
    omarchy-autoboot.iso      ← built output: our own iPXE, no netboot.xyz
  hook/
    archiso_pxe_nfs           ← archiso initcpio hook (shell), repurposed slot
  bridge/
    DESIGN.md                 ← the bridge's spec: protocol, milestones, interface contract
    OPTIMIZATION.md           ← next round of work: concurrent fetch + readahead
    cmd/omarchy-nbd-bridge/    ← CLI entrypoint
    internal/                 ← nbdproto, httpbackend, cache, backend packages
    scripts/, integration/    ← build/test/stress-test scripts
  docs/
    ARCHITECTURE.md           ← this file
```
