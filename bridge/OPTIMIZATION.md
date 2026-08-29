# Bridge optimization — concurrent fetch + readahead

## Why

First real end-to-end install: **41 minutes**, against a host connection that
does ~43MB/s to `iso.omarchy.org` sitting mostly idle the whole time. That's
the benchmark to beat.

Measured, not guessed, from the host directly:

- Sustained bandwidth to `iso.omarchy.org`: **~43MB/s (343Mbps)**, a 100MB
  Range pull in 2.4s. Not the bottleneck.
- Per-request latency for a single 64KB Range request: **~100–200ms**, even
  on a fresh connection.
- `cf-cache-status: DYNAMIC` on every response — Cloudflare is **not**
  edge-caching this file (large file, Range requests; common CDN behavior).
  Every single request goes all the way to the real origin. That per-request
  latency is real, not an artifact of a slow test connection.

The current bridge fetches one block at a time, fully serially:
`cache.go`'s single mutex is held across the *entire* fetch including the
network call (`internal/cache/cache.go`, `getOrFetchLocked`), and
`nbdproto.go`'s reply loop (`internal/nbdproto/nbdproto.go`) processes one
NBD request to completion before starting the next. At ~150ms average per
64KB block, that's **~0.4MB/s effective throughput** — regardless of the
~43MB/s pipe sitting underneath, mostly unused. That maps far better to the
41-minute number than any VM NIC overhead does (verified: VM's virtual
adapter isn't part of this path's cost at all — the host-side measurement
above already showed the pipe itself is fine).

## What the workload actually looks like

Worth being precise about this, since it changes which optimization matters
most. The install is **not** one big sequential pull of the whole ISO.
squashfs is a compressed, block-addressed filesystem; what actually happens
is `archinstall` copying individual package files out of
`/var/cache/omarchy/mirror/offline/*.pkg.tar.zst` (bundled inside the
squashfs) one at a time — each read **roughly sequentially start-to-end**,
then jumping to wherever the next package's data happens to live.

Consequence: for the ~3GB of package data that dominates install time, each
byte is read **exactly once**. There's no repeat access for a cache to
catch — cache *hit rate* on that data is inherently near-zero, no matter how
good the eviction policy is. The cache mostly earns its keep on squashfs's
own **metadata blocks** (directory entries, inode/fragment tables), which
get re-touched as different files get looked up — real, but a small
fraction of total bytes moved.

This is why the priorities below are ordered the way they are: the win is
in *how fast the one-time reads happen*, not in avoiding re-fetches.

## Priority 1: concurrent fetch (do this first)

Stop serializing every fetch behind one mutex held across network I/O.
Multiple *different* blocks must be fetchable simultaneously.

Requirements:

- `cache.ReadAt` must let fetches to distinct block indices actually
  overlap in flight, not just interleave submission — a real timing
  difference, testable by making the backend artificially slow and
  checking wall-clock time for N concurrent distinct-block reads stays
  close to *one* round's latency, not N of them.
- **Thundering herd**: if two callers miss on the *same* block
  concurrently (two real NBD requests, or a real request racing your own
  readahead fetch for the same block — see Priority 2), exactly **one**
  fetch must happen. The other caller(s) wait on it rather than each
  kicking off a redundant fetch. Existing `countingBackend` in
  `cache_test.go` is exactly the tool to catch a regression here.
- **Bounded concurrency.** Don't let this spawn unbounded goroutines/HTTP
  connections. Pick a cap. See the Little's Law numbers below for what a
  *sensible* cap actually looks like — it's higher than intuition might
  suggest at the current block size.
- The existing memory bound must keep holding: `ResidentBytes() <=
  maxBytes`, even with multiple fetches in flight simultaneously (i.e.
  count reservations *before* the fetch completes, not just after
  insertion, or a burst of concurrent misses could transiently blow past
  the configured cap).
- NBD's own protocol already permits this — replies are cookie-correlated,
  not required to be in request order (see `DESIGN.md`'s wire-format
  section and `TestRead_CookieCorrelationAcrossMultipleRequests` in
  `nbdproto_test.go`, already passing). Nothing about the protocol blocks
  concurrent processing; the current code just doesn't use that freedom.

## Priority 2: readahead

Given the workload is locally-sequential-per-file, the highest-value
follow-up is: on a miss at block *i*, proactively fetch block *i+1* (and
maybe a couple more) **without being asked**, landing them in the cache
before the next request arrives. Cheap to get wrong in small ways, so:

- Must not surface a readahead failure as a real read failure. A
  speculative fetch that fails (network hiccup, or legitimately runs past
  EOF — see next point) should be dropped/logged, never propagated to the
  caller of the *actual* requested read.
- Must not request past the backend's end. Naive "always fetch i+1" breaks
  the moment the requested block is the last one. Test for this directly
  rather than trusting it falls out naturally.
- Readahead-fetched blocks still count against `maxBytes` like any other
  block — the memory bound is not a suggestion that only applies to
  demand-fetched data.
- Whatever readahead depth you pick is an implementation detail — the
  tests below only assert "at least the next block gets fetched
  proactively," not an exact window size. Tune from there.

This is the one that doesn't require touching `nbdproto.go` at all if
implemented entirely inside `cache.go`: the *next* sequential NBD request
just finds its block already resident by the time it arrives, turning a
future slow miss into a fast hit before it's even asked for.

## A number worth knowing before picking constants

Little's Law, roughly: `throughput ≈ concurrency × (blockSize / latency)`.
Using the measured ~150ms average and wanting to approach the host's
~43MB/s ceiling:

| block size | concurrency needed to saturate ~43MB/s |
|---|---|
| 64KB (current) | ~98 |
| 256KB | ~25 |
| 512KB | ~12 |
| 1MB | ~6 |

At the current 64KB block size, concurrency alone needs to be quite high
to actually reach line rate — a lot of simultaneous connections to a
single origin, more likely to trip rate-limiting than to help. Increasing
block size is a **more practical lever than concurrency alone**, and this
workload (large, one-time-sequential package reads) is a good fit for
larger blocks. The tension: `DESIGN.md`'s original 64KB choice was sized
for squashfs's *small, dense metadata* reads — a bigger block spends more
bytes per metadata lookup than necessary. Not a free lunch either way; a
moderate increase (e.g. 256–512KB) combined with modest concurrency
(8–16) is probably a better balance than maxing out either knob alone.
Not prescribing an exact number here — just flagging that block size is a
real, load-bearing variable in this tuning, not just a constant to leave
alone. Making it a CLI flag (main.go already has the pattern for
`--cache-size`) would make this easy to A/B without rebuilding.

## Explicitly out of scope for this pass

- **`nbdproto.go`-level request concurrency** (dispatching multiple
  *distinct, non-readahead-related* NBD requests in parallel, for the
  case where the kernel itself issues genuinely overlapping reads). Real
  workload observed here is single-threaded sequential package copy, so
  Priority 2 (readahead entirely inside `cache.go`) likely captures most
  of the available win without this. Worth revisiting only if benchmarks
  after Priority 1+2 still show meaningful idle time in the pipe.
- **Cache eviction policy (LRU vs. something scan-resistant).** Real
  concern in principle — a long sequential one-time-use scan can evict
  metadata blocks that were worth keeping (same problem Postgres and
  others special-case in their buffer caches) — but this costs cache
  *efficiency*, not wall-clock time, and the latency-bound serialization
  above costs orders of magnitude more right now. Revisit only if
  Priority 1+2 don't get you close enough to the 43MB/s ceiling.

## Keep in mind while implementing

- `go test ./...` (including `-race`) must stay green throughout, not just
  at the end. The existing `TestCache_ConcurrentReadsAreSafe` in
  `cache_test.go` is your safety net for the concurrency you're adding —
  consider it a floor, not sufficient on its own; the new tests below
  target the *specific* new behaviors (parallelism, dedup, readahead)
  that a generic concurrent-safety test won't catch on its own.
- The tests in `cache_concurrency_test.go` (next to this doc) call
  `cache.New` with its *current* 3-argument signature on purpose — no new
  exported API is assumed. Whatever concurrency cap / readahead depth you
  land on can be an internal constant, a new optional parameter, whatever
  you prefer; the tests only observe *behavior* through the existing
  public surface (`ReadAt`, `ResidentBytes`), so they compile and fail
  meaningfully (not a compile error) against the current serial
  implementation, and should start passing once the behavior lands,
  whatever shape the internals end up taking.
- `main.go`'s `client := &http.Client{Timeout: 30 * time.Second, ...}`
  already uses a `pinnedDialer` (DNS-pinning fix). Concurrent requests
  will all share that one `http.Client`/`Transport` — worth checking
  `http.Transport`'s `MaxIdleConnsPerHost` default (2) isn't quietly
  capping your real concurrency by forcing idle-connection churn once you
  raise it above that.
