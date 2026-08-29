# omarchy-nbd-bridge — design spec

## Running the tests

```
cd bridge
CGO_ENABLED=0 go test ./...
```

That's the whole validation loop for M0-M3 — no boot, no VM, no manual
commands. Everything currently fails with a clear `panic("TODO: ...")`
pointing at exactly what's unimplemented (verified: the whole suite builds
and runs clean right now, every failure is an intentional stub, nothing
hangs or crashes the runner). As you implement each package, its tests
should start passing independently of the others — `go test ./internal/nbdproto/...`
alone is the fast loop while working on M0, no need to touch httpbackend or
cache to see progress there.

Package map, matching the milestones:
- `internal/backend` — the shared interface, plus `Mem` (M0's fixed
  buffer), already implemented — this is scaffolding, not the interesting
  part.
- `internal/nbdproto` — M0. Tests drive it over `net.Pipe()` as a fake NBD
  client, byte-for-byte against the spec. No real kernel/`nbd-client`
  needed.
- `internal/httpbackend` — M1 + M3. Tests use `httptest.Server`, including
  hand-written handlers for the M3 fault-injection cases (retry, timeout,
  non-206 response, mismatched `Content-Range`).
- `internal/cache` — M2. Tests use a call-counting fake backend to assert
  cache hits/eviction/bounded size without needing real I/O at all.
- `cmd/omarchy-nbd-bridge` — the CLI. Flag parsing is done and tested; the
  actual wiring of the three packages above into a running server is the
  last thing you write, once each passes independently.

M4 and M5 aren't `go test` targets (one's a build property, one needs a
real kernel) — `scripts/check-static.sh` and `integration/test-mount.sh`
cover those, each a single rerunnable command instead of retyped ones.

## What this is

A read-only NBD server. It exports one thing: the byte content of a remote
HTTP(S) URL, fetched on demand via `Range` requests, with a small bounded
cache. To anything that connects to it (the Linux kernel's NBD client, via
`nbd-client`), it looks exactly like a normal block device — the same as a
USB stick — except reads are secretly satisfied over the network instead of
over USB.

**Important, confirmed by reading Omarchy's actual `archiso_mount_handler`
directly: the URL this points at must be the whole ISO, not just the
squashfs.** The stock archiso mount handler mounts whatever block device
you give it as a real filesystem and looks for `arch/x86_64/airootfs.sfs`
as a file inside it — same as it would for a real USB stick. The bridge
itself doesn't care either way (it's agnostic to what bytes it's serving),
this only affects what URL the hook script passes it at boot time.

This is the piece proven to work manually with `nbdkit curl --filter=cache`
in the WSL test (1.9GB RAM cap, 0 swap, peak 567MB used, no OOM, reading
2.2GB of real files out of a 7.16GB image). This binary replaces that
`nbdkit` invocation with something purpose-built, small, and staticlly
linkable into an initramfs with zero package manager available.

## Interface contract (what the hook script expects)

```
omarchy-nbd-bridge --url <https-url> --listen <host:port> --cache-size <bytes>
```

- Runs in the foreground, logs to stderr. The hook backgrounds it itself
  (via `setsid`, same lesson learned in the WSL test — plain `&`/`disown`
  isn't reliable detachment).
- Exits nonzero with a clear stderr message on any fatal startup error
  (can't resolve URL, can't bind port, etc.) — the hook needs to be able to
  tell "bridge failed to start" apart from "bridge is running, waiting for
  a client."
- Listens on plain TCP (`127.0.0.1:10809` in practice — nothing ever
  connects to this from outside the machine, it's a loopback handoff to the
  kernel's own NBD client).
- Serves exactly one export regardless of what name is requested — **don't
  validate the export name, just always serve the one thing you have.**
  In practice the real client (archiso's stock `archiso_pxe_nbd` hook) will
  request the name `"archiso"` by default (`archiso_nbd_name` kernel
  cmdline param, confirmed by reading that hook's actual source), but
  there's no reason to hardcode a check against that — accepting any name
  is simpler and strictly more permissive.
- Read-only. Must advertise `NBD_FLAG_READ_ONLY` and error any write attempt
  (archiso never writes to this device — the writable overlay is a separate
  tmpfs layered on top, untouched by this project).

## Build order — do it in this sequence, not all at once

Each milestone is independently testable against the real kernel NBD client
before moving to the next. This isolates "is my protocol handling correct"
from "is my HTTP fetching correct" — conflating those two while debugging is
where most of the pain would be.

**M0 — Fake it.** Serve a fixed in-memory buffer (e.g. 64MB of zeroes, or a
recognizable repeating pattern) as the export. No HTTP involved at all yet.
Goal: get `nbd-client 127.0.0.1 10809 /dev/nbd0` to connect successfully and
`dd if=/dev/nbd0 bs=4096 count=10 | xxd` to read back exactly what you expect.
This alone proves the handshake and read-reply framing are byte-correct —
the single hardest part to get right, and the part where a wrong magic
number silently hangs or disconnects with no useful error.

**M1 — Real bytes, no cache.** Replace the fixed buffer with a live HTTP
Range GET per read request (`Range: bytes=<offset>-<offset+length-1>`).
Every read hits the network, every time — slow, but correctness-first. Test:
mount a real squashfs off it (`mount -t squashfs -o ro /dev/nbd0 /mnt/x`)
and `ls`/`cat` a few files. If this works, the hard networking part is done.

**M2 — Bounded cache.** Add an LRU cache in front of the HTTP fetch: keyed by
block-aligned offset (see "block size" below), capped at `--cache-size`
bytes, evict-oldest-on-insert-when-full. In-memory (a simple `HashMap` /
`map[int64][]byte` + a doubly-linked list or an existing LRU library) is
fine for a first version — this is the actual point of the whole project,
the piece that keeps memory bounded regardless of image size. Re-run the
WSL memory-cap stress test from earlier in this project (2GB cap, 0 swap,
read a large chunk of the mounted tree, watch `free -h` + `dmesg`) against
your own binary instead of `nbdkit` and confirm the same result.

**M3 — Robustness pass.** Retry-with-backoff on transient HTTP failures
(connection drops mid-read are the realistic failure mode on a real network,
not on loopback — this is the part loopback testing can't shake out for
you). Reasonable timeouts. Handle a server that doesn't return exactly the
byte range requested (some CDNs round up to their own chunk boundaries —
check `Content-Range` in the response, don't just trust you got exactly what
you asked for).

**M4 — Shrink and freeze.** Static link, strip symbols, confirm final binary
size, confirm it runs with zero shared library dependencies (`ldd` should
say "not a dynamic executable" or equivalent). This is the version that
actually goes in the initramfs.

**M5 — Real boot test.** Swap out the WSL/loopback nginx target for the real
injected-hook path in the Hyper-V VM. This is where `hook/` and `boot/`
(already scaffolded, see the repo root) come in — that's the "boring" half
I've got.

## NBD protocol reference — fixed newstyle, single export, read-only

All multi-byte fields are **big-endian** (network byte order). This is the
exact byte layout, pulled from the current upstream spec
(`NetworkBlockDevice/nbd`), not from memory — verify against
`https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md` if
anything here is ambiguous.

### 1. Handshake (once per connection, before any options)

```
S → C:  64 bits   0x4e42444d41474943   ("NBDMAGIC")
S → C:  64 bits   0x49484156454f5054   ("IHAVEOPT")
S → C:  16 bits   handshake flags       = 0x0001  (NBD_FLAG_FIXED_NEWSTYLE)
C → S:  32 bits   client flags          (must include bit0 = NBD_FLAG_C_FIXED_NEWSTYLE
                                          to proceed; real clients always send this)
```

### 2. Option haggling (repeats until client sends NBD_OPT_EXPORT_NAME or NBD_OPT_GO)

Client → server, per option:
```
C → S:  64 bits   0x49484156454f5054   ("IHAVEOPT", repeated — sanity marker)
C → S:  32 bits   option number         (NBD_OPT_EXPORT_NAME = 1)
C → S:  32 bits   length of option data
C → S:  <length> bytes   option data    (the export name string; empty is fine —
                                          you only ever serve one export)
```

Server's reply to a successful `NBD_OPT_EXPORT_NAME` (the *last* thing sent
in the handshake — transmission phase starts immediately after, no explicit
"ack" message):
```
S → C:  64 bits   size of the export, in bytes
S → C:  16 bits   transmission flags    = 0x0003
                                          (bit0 NBD_FLAG_HAS_FLAGS, always 1)
                                          (bit1 NBD_FLAG_READ_ONLY, set this)
S → C:  124 bytes  zero padding          (reserved; omit only if client negotiated
                                           NBD_FLAG_C_NO_ZEROES, which you can just
                                           not support for a first version)
```

`NBD_OPT_EXPORT_NAME` cannot return a structured error — if the export name
is bad, the spec says the server must just drop the connection. That's fine
for a single-export server: never reject, always succeed. (`NBD_OPT_GO` is
the more modern option that *can* return a clean error — worth adding later,
not needed for M0–M2.)

### 3. Transmission phase (repeats until disconnect)

Client request:
```
C → S:  32 bits   0x25609513   (NBD_REQUEST_MAGIC)
C → S:  16 bits   command flags   (ignore/pass through as 0 for a first version)
C → S:  16 bits   type            (0 = NBD_CMD_READ, 2 = NBD_CMD_DISC — the only
                                    two you need to handle for a read-only export;
                                    NBD_CMD_WRITE = 1, reject with EROFS if it
                                    ever arrives, which it shouldn't given the
                                    READ_ONLY flag)
C → S:  64 bits   cookie          (opaque — copy verbatim into your reply)
C → S:  64 bits   offset
C → S:  32 bits   length
```

Server's simple reply (sufficient for a read-only server — you never need
the newer "structured reply" format):
```
S → C:  32 bits   0x67446698   (NBD_SIMPLE_REPLY_MAGIC)
S → C:  32 bits   error         (0 = success; use standard errno values, e.g.
                                  EIO = 5, on a fetch failure)
S → C:  64 bits   cookie        (must match the request's cookie exactly)
S → C:  <length> bytes  data    (only present, and only on success, for reads)
```

`NBD_CMD_DISC`: offset and length must be all-zero on the request; no reply
is sent at all — just close the socket cleanly after receiving it.

### Block size / cache granularity

Pick a fixed block size (64KB is a reasonable starting point — matches what
`nbdkit-cache-filter` used in the validated test) and round every fetch up
to block boundaries before hitting the network: a read for bytes
`[100, 200)` becomes a fetch of the whole 64KB block containing byte 100,
cached under that block's aligned offset as the key. This matters for two
reasons: it bounds your cache's key space to something sane, and it means
adjacent small reads (very common — squashfs metadata, directory reads) are
likely to hit an already-cached block instead of triggering a fresh HTTP
round-trip each time.

## Language choice

Whatever you're comfortable in works for the protocol logic — it's simple
binary framing, nothing exotic. The real constraint is **easy static
linking with no OpenSSL/libcurl dependency**, since that dependency chain is
specifically what made shipping stock `nbdkit` into the initramfs annoying
in the first place. Two languages solve that cleanly out of the box:

- **Go** — binaries are static by default (`CGO_ENABLED=0 go build`), and
  `net/http` + `crypto/tls` are pure-Go, no OpenSSL linkage at all. Probably
  the fastest path to a working M0 if this is your first time touching a
  binary wire protocol — the standard library's `encoding/binary` makes the
  big-endian struct packing/unpacking straightforward.
- **Rust** — `ureq` (HTTP client) + `rustls` (pure-Rust TLS) + the
  `x86_64-unknown-linux-musl` target gives you a genuinely tiny, fully
  static binary with no C toolchain dependency at all. More upfront
  ceremony than Go, more control, memory-safety is a nice-to-have here
  even though the input (kernel's own NBD client) is trusted.

I'd default to **Go** unless you specifically want the Rust experience —
it gets you to a working M0 fastest, which matters for keeping momentum on
a first protocol implementation. Either way, avoid C for this one: you'd be
right back to hand-rolling HTTPS or linking OpenSSL statically, which is the
exact pain this whole detour around `nbdkit` was meant to avoid.

## What I've already got ready

`hook/archiso_pxe_nfs` and `boot/omarchy-nbd.ipxe` are written and scaffolded
against the CLI contract above, verified against Omarchy's real initramfs
(not guessed) — see `docs/ARCHITECTURE.md` for exactly what was checked.
Once your binary implements M0, we can test the real injection path (M5)
well before M2/M3 are done, since the hook doesn't care whether the bridge
is fully robust yet, only that it speaks the contract: listens where told,
serves reads, closes cleanly on disconnect.

Still open, blocking M5 specifically (not M0-M4, which you can build and
test with `nbd-client` standalone same as the `nbdkit` validation earlier):
somewhere to host the whole Omarchy ISO as a single Range-capable URL
(Azure Blob — see `docs/ARCHITECTURE.md`'s hosting table) to plug into
`boot/omarchy-nbd.ipxe`'s `iso_url`.
