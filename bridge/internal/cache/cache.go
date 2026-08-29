// Package cache wraps a backend.Backend with a bounded, block-aligned LRU
// cache. This is the actual point of the whole project (see ../../DESIGN.md
// M2): it's what keeps memory use independent of the wrapped backend's
// total size, no matter how much of it gets read over a session -- proven
// manually in the WSL validation earlier in this project (1.9GB RAM cap,
// zero swap, peak 567MB used reading 2.2GB of real files out of a 7.16GB
// image, via nbdkit's equivalent cache filter).
package cache

import (
	"omarchy-nbd-bridge/internal/backend"
	"sync"
)

// Cache implements backend.Backend by wrapping another Backend with an
// LRU cache. See cache_test.go for the exact expected behavior:
//   - Reads are rounded up to blockSize-aligned regions before ever
//     touching the wrapped backend (see DESIGN.md "Block size / cache
//     granularity" -- this is what lets small, adjacent reads, like
//     squashfs metadata, share one fetch instead of triggering a fresh
//     one each).
//   - Repeated reads of an already-cached block must not touch the
//     wrapped backend again.
//   - Total resident cache size must never exceed maxBytes, regardless of
//     how much distinct data has been read over the cache's lifetime.
//   - When eviction is needed, the least-recently-used block goes first.
//
// The mutex is held across the wrapped backend's ReadAt call, not just
// the map/list bookkeeping -- deliberately, to close a check-then-act
// race (an earlier version checked size-vs-max and evicted under the
// lock but as two separate critical sections, so concurrent callers could
// all pass the check before any of them inserted, overshooting maxBytes;
// it also used a single package-level mutex shared by every Cache
// instance instead of one per instance). The tradeoff is that concurrent
// reads of *different* blocks now serialize on network I/O rather than
// overlapping -- acceptable here since this only ever backs one mounted
// filesystem at a time, not a concern worth the added complexity of
// per-block locking or dedup of concurrent misses on the same block.
type Cache struct {
	back        backend.Backend
	blockSize   int64
	maxBytes    int64
	backendSize int64

	mu   sync.Mutex
	c    map[int64]*node
	head *node // most recently used
	tail *node // least recently used
}

type node struct {
	b      []byte
	off    int64
	parent *node // toward head (more recently used)
	child  *node // toward tail (less recently used)
}

// New wraps b with an LRU cache. blockSize is the alignment/granularity
// for both fetches and eviction; maxBytes is the hard cap on total
// resident cache size.
func New(b backend.Backend, blockSize int, maxBytes int64) *Cache {
	return &Cache{
		back:        b,
		blockSize:   int64(blockSize),
		maxBytes:    maxBytes,
		backendSize: b.Size(),
		c:           map[int64]*node{},
	}
}

// Size reports the size of the underlying export -- delegated straight
// to the wrapped backend, deliberately NOT the same thing as
// ResidentBytes(). This is what makes Cache itself satisfy
// backend.Backend, which is exactly how it gets used: nbdproto.Serve
// calls Size() during the NBD handshake to tell the client how big the
// export is, before a single byte has been cached. Conflating this with
// resident cache size (an earlier version did) reports a 0-byte export
// at negotiation time and real NBD clients -- confirmed against the
// actual nbd-client binary -- connect but see "size = 0MB" and never get
// anything mountable.
func (c *Cache) Size() int64 {
	return c.back.Size()
}

// ResidentBytes reports how many bytes of block data the cache is
// currently holding. Not part of backend.Backend -- exposed for tests and
// for whatever metrics/logging you want in the real binary.
func (c *Cache) ResidentBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(len(c.c)) * c.blockSize
}

// touchLocked moves n to the front (head/MRU end) of the list. Caller
// must hold c.mu.
func (c *Cache) touchLocked(n *node) {
	if c.head == n {
		return
	}
	if n.parent != nil {
		n.parent.child = n.child
	}
	if n.child != nil {
		n.child.parent = n.parent
	}
	if c.tail == n {
		c.tail = n.parent
	}
	n.parent = nil
	n.child = c.head
	if c.head != nil {
		c.head.parent = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
}

// evictOneLocked drops the least-recently-used block. Caller must hold
// c.mu.
func (c *Cache) evictOneLocked() {
	victim := c.tail
	if victim == nil {
		return
	}
	c.tail = victim.parent
	if c.tail != nil {
		c.tail.child = nil
	} else {
		c.head = nil
	}
	delete(c.c, victim.off)
}

// getOrFetchLocked returns the node for block i, fetching it from the
// wrapped backend (evicting first if needed to stay under maxBytes) if
// it isn't already resident. Caller must hold c.mu.
func (c *Cache) getOrFetchLocked(i int64) (*node, error) {
	if n, ok := c.c[i]; ok {
		c.touchLocked(n)
		return n, nil
	}

	for len(c.c) > 0 && int64(len(c.c))*c.blockSize+c.blockSize > c.maxBytes {
		c.evictOneLocked()
	}

	// The last block of a backend whose size isn't an exact multiple of
	// blockSize is shorter than blockSize -- fetching a full blockSize
	// there would ask the backend for bytes past its end. httpbackend
	// correctly treats a short/clamped response to an over-long Range
	// request as an error (M3: never trust a response that doesn't match
	// what was asked for), so an unconditional full-blockSize fetch here
	// made every read of the last block fail -- confirmed against a real
	// mount: squashfs keeps trailing metadata in exactly that last block,
	// so this broke every real mount despite all of M0-M2's unit tests
	// (which happened to only ever exercise block-aligned backend sizes)
	// passing clean.
	blockStart := i * c.blockSize
	want := c.blockSize
	if blockStart+want > c.backendSize {
		want = c.backendSize - blockStart
	}

	data := make([]byte, want)
	if _, err := c.back.ReadAt(data, blockStart); err != nil {
		return nil, err
	}

	n := &node{b: data, off: i}
	c.c[i] = n
	n.child = c.head
	if c.head != nil {
		c.head.parent = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
	return n, nil
}

func (c *Cache) ReadAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	end := off + int64(len(p))
	written := int64(0)
	for i := off / c.blockSize; i <= (end-1)/c.blockSize; i++ {
		n, err := c.getOrFetchLocked(i)
		if err != nil {
			return int(written), err
		}

		blockStart := i * c.blockSize
		beg := max(0, off-blockStart)
		// n.b can be shorter than c.blockSize for the backend's last
		// block (see getOrFetchLocked) -- clamp against its actual
		// length, not just the nominal block size.
		fin := min(int64(len(n.b)), end-blockStart)
		copy(p[written:written+(fin-beg)], n.b[beg:fin])
		written += fin - beg
	}
	return int(written), nil
}
