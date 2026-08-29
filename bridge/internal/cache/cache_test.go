package cache_test

import (
	"bytes"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"omarchy-nbd-bridge/internal/backend"
	"omarchy-nbd-bridge/internal/cache"
)

// countingBackend wraps a backend.Backend and counts how many times
// ReadAt was actually called on the underlying (uncached) backend -- the
// signal these tests care about.
type countingBackend struct {
	backend.Backend
	reads int32
}

func (c *countingBackend) ReadAt(p []byte, off int64) (int, error) {
	atomic.AddInt32(&c.reads, 1)
	return c.Backend.ReadAt(p, off)
}

func TestCache_RepeatedReadHitsCacheNotBackend(t *testing.T) {
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(data)
	cb := &countingBackend{Backend: backend.NewMem(data)}
	c := cache.New(cb, 64*1024, 1<<20) // 1MB cache -- plenty for this

	buf := make([]byte, 100)
	for i := 0; i < 5; i++ {
		if _, err := c.ReadAt(buf, 1000); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
	}
	// fmt.Println("buf", buf)
	// fmt.Println("data", data[1000:1100])
	if !bytes.Equal(buf, data[1000:1100]) {
		t.Fatalf("data mismatch")
	}
	if got := atomic.LoadInt32(&cb.reads); got != 1 {
		t.Fatalf("underlying backend was read %d times for 5 identical requests, want 1 -- repeat reads of an already-cached block must hit the cache, not re-fetch", got)
	}
}

func TestCache_SizeReportsBackendSizeNotResidentBytes(t *testing.T) {
	// Size() is what makes Cache itself satisfy backend.Backend -- it's
	// called during NBD handshake, before anything has been read, to
	// tell the client how big the export is. It must report the
	// *backend's* total size, not how much data happens to be resident
	// in the cache at that moment (which is 0 at handshake time, and
	// would report a 0-byte export to every real NBD client -- confirmed
	// against the real nbd-client binary, not just a theoretical concern).
	const backendSize = 10 << 20 // 10MB
	data := make([]byte, backendSize)
	c := cache.New(backend.NewMem(data), 64*1024, 1<<20)

	if got := c.Size(); got != backendSize {
		t.Fatalf("Size() = %d before any reads, want %d (the backend's size) -- got the resident cache size instead", got, backendSize)
	}

	buf := make([]byte, 100)
	if _, err := c.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	if got := c.Size(); got != backendSize {
		t.Fatalf("Size() = %d after a read, want %d -- Size() must stay pinned to the backend's size, not drift with how much is cached", got, backendSize)
	}
	if got := c.ResidentBytes(); got == backendSize {
		t.Fatalf("ResidentBytes() = %d, suspiciously equal to the full backend size after reading a single 100-byte range -- test setup problem, not exercising the distinction this test is for", got)
	}
}

func TestCache_BoundsMemoryRegardlessOfBackendSize(t *testing.T) {
	// Mirrors the real validation from earlier in this project: a large
	// backend, a small cache, reading far more distinct data than the
	// cache can hold -- resident size must never grow past the
	// configured max.
	const backendSize = 200 << 20 // 200MB fake backend
	const maxCache = 4 << 20      // 4MB cache
	const blockSize = 64 << 10    // 64KB blocks

	data := make([]byte, backendSize)
	c := cache.New(backend.NewMem(data), blockSize, maxCache)

	buf := make([]byte, 100)
	for off := int64(0); off < backendSize; off += 1 << 20 { // touch a block every 1MB
		if _, err := c.ReadAt(buf, off); err != nil {
			t.Fatalf("ReadAt at %d: %v", off, err)
		}
	}

	if got := c.ResidentBytes(); got > maxCache {
		t.Fatalf("cache resident size = %d bytes, want <= %d -- eviction must keep this bounded no matter how much distinct data has been read over the cache's lifetime", got, maxCache)
	}
}

func TestCache_EvictsLeastRecentlyUsed(t *testing.T) {
	const blockSize = 1024
	const maxCache = 3 * blockSize // room for exactly 3 blocks
	data := make([]byte, 10*blockSize)
	cb := &countingBackend{Backend: backend.NewMem(data)}
	c := cache.New(cb, blockSize, maxCache)

	buf := make([]byte, 10)
	read := func(block int64) {
		if _, err := c.ReadAt(buf, block*blockSize); err != nil {
			t.Fatalf("ReadAt block %d: %v", block, err)
		}
	}

	read(0)
	read(1)
	read(2) // cache is now exactly full: blocks 0, 1, 2
	read(0) // re-touch block 0 -- block 1 is now the least-recently-used one
	read(3) // must evict something to fit -- should be block 1, not 0 or 2

	before := atomic.LoadInt32(&cb.reads)
	read(0) // should still be cached
	read(2) // should still be cached
	if after := atomic.LoadInt32(&cb.reads); after != before {
		t.Fatalf("blocks 0 and 2 should still have been cached after inserting block 3, but the backend was hit %d more time(s)", after-before)
	}

	beforeMiss := atomic.LoadInt32(&cb.reads)
	read(1) // should have been evicted as least-recently-used
	if atomic.LoadInt32(&cb.reads) == beforeMiss {
		t.Fatalf("block 1 should have been evicted as least-recently-used when block 3 was inserted, but was served from cache")
	}
}

func TestCache_BlockAlignment_AdjacentSmallReadsShareOneFetch(t *testing.T) {
	// Two small, adjacent reads inside the same block-sized region should
	// only cost one underlying fetch -- the whole reason for rounding
	// reads up to block boundaries (DESIGN.md "Block size / cache
	// granularity"): squashfs metadata reads are small and dense, and
	// this is what lets them mostly hit cache instead of triggering a
	// fresh round-trip each.
	const blockSize = 65536
	data := make([]byte, 5*blockSize)
	cb := &countingBackend{Backend: backend.NewMem(data)}
	c := cache.New(cb, blockSize, 10*blockSize)

	buf := make([]byte, 10)
	if _, err := c.ReadAt(buf, 100); err != nil { // inside block 0
		t.Fatalf("ReadAt: %v", err)
	}
	if _, err := c.ReadAt(buf, blockSize-10); err != nil { // also inside block 0
		t.Fatalf("ReadAt: %v", err)
	}

	if got := atomic.LoadInt32(&cb.reads); got != 1 {
		t.Fatalf("backend was hit %d time(s) for two reads inside the same block, want 1", got)
	}
}

func TestCache_ReturnsCorrectDataAcrossBlockBoundary(t *testing.T) {
	// A read that spans two cache blocks must still return exactly the
	// right bytes -- easy to get subtly wrong if block-alignment logic
	// only handles the "entirely within one block" case.
	const blockSize = 1024
	data := make([]byte, 4*blockSize)
	for i := range data {
		data[i] = byte(i % 256)
	}
	c := cache.New(backend.NewMem(data), blockSize, 10*blockSize)

	off := int64(blockSize - 10) // starts 10 bytes before a block boundary
	buf := make([]byte, 20)      // ends 10 bytes after it
	if _, err := c.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	// fmt.Println(data)
	// fmt.Println("buf", buf, "\ndat", data[off:off+20])
	if !bytes.Equal(buf, data[off:off+20]) {
		t.Fatalf("data mismatch for a read spanning a block boundary")
	}
}

func TestCache_ReadsLastPartialBlockWhenBackendSizeIsntBlockAligned(t *testing.T) {
	// A real backend's size is essentially never an exact multiple of the
	// cache's block size (the real ISO isn't). If the cache always fetches
	// a full block's worth starting at the block boundary, the last
	// block's fetch reaches past the backend's actual end -- confirmed
	// against a real boot: this made every read of an export's final
	// block fail (httpbackend correctly rejects the resulting short/
	// clamped HTTP response instead of silently trusting it), and
	// squashfs keeps trailing metadata in exactly that block, so every
	// real mount failed despite every other cache test here passing.
	const blockSize = 1024
	const backendSize = 3*blockSize + 100 // last block only has 100 bytes
	data := make([]byte, backendSize)
	for i := range data {
		data[i] = byte(i % 256)
	}
	c := cache.New(backend.NewMem(data), blockSize, 10*blockSize)

	// Read the very last few bytes of the backend.
	buf := make([]byte, 10)
	off := int64(backendSize - 10)
	if _, err := c.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt at the tail end of a non-block-aligned backend: %v", err)
	}
	if !bytes.Equal(buf, data[off:off+10]) {
		t.Fatalf("data mismatch reading the last 10 bytes of a non-block-aligned backend")
	}

	// Also read the entire last (partial) block on its own.
	lastBlockOff := int64(3 * blockSize)
	buf2 := make([]byte, 100)
	if _, err := c.ReadAt(buf2, lastBlockOff); err != nil {
		t.Fatalf("ReadAt for the whole last partial block: %v", err)
	}
	if !bytes.Equal(buf2, data[lastBlockOff:]) {
		t.Fatalf("data mismatch reading the whole last partial block")
	}
}

func TestCache_ConcurrentReadsAreSafe(t *testing.T) {
	// The real bridge serves NBD requests that may be processed
	// concurrently (see nbdproto's cookie-correlation requirement, which
	// exists specifically because requests needn't be handled strictly
	// in order) -- the cache sits underneath that and must not race or
	// corrupt data under concurrent access.
	const blockSize = 4096
	data := make([]byte, 100*blockSize)
	for i := range data {
		data[i] = byte(i % 256)
	}
	c := cache.New(backend.NewMem(data), blockSize, 20*blockSize)

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64((i % 100) * blockSize / 2)
			buf := make([]byte, 50)
			if _, err := c.ReadAt(buf, off); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(buf, data[off:off+50]) {
				errs <- errFromOffset(off)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent read failed: %v", err)
	}
}

type offsetErr int64

func (e offsetErr) Error() string   { return "data mismatch at offset" }
func errFromOffset(off int64) error { return offsetErr(off) }
