// Command omarchy-nbd-bridge is the CLI entrypoint -- see ../../DESIGN.md
// "Interface contract" for exactly what the hook script expects from this
// binary's behavior (foreground, logs to stderr, nonzero exit with a
// clear message on fatal startup errors).
//
// Flag parsing is done. The actual wiring (backend chain -> nbdproto.Serve
// per connection) is deliberately left as the last piece -- by the time
// you get here, M0/M1/M2 should each already be passing their own tests
// independently, so this is assembly, not new logic.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"omarchy-nbd-bridge/internal/cache"
	"omarchy-nbd-bridge/internal/httpbackend"
	"omarchy-nbd-bridge/internal/nbdproto"
)

// blockSize is the cache's fetch/eviction granularity. See DESIGN.md
// "Block size / cache granularity" for the original tradeoff (small
// blocks for squashfs's dense metadata reads vs. big enough to avoid a
// flood of tiny requests) and OPTIMIZATION.md for why this moved off
// that original 64KB: at the ~150ms per-request latency measured against
// the real ISO host, 64KB blocks need dozens of concurrent requests to
// approach the host's real bandwidth ceiling. 256KB cuts the number of
// round trips per byte by 4x for a modest, not "megabytes", increase in
// what one metadata-adjacent read drags in.
const blockSize = 256 * 1024

func main() {
	url := flag.String("url", "", "URL to serve (required) -- must support HTTP Range requests")
	listen := flag.String("listen", "127.0.0.1:10809", "address to listen on")
	cacheSize := flag.String("cache-size", "256M", "max resident cache size, e.g. 256M, 1G")
	readyFile := flag.String("ready-file", "", "if set, touched once the listener is actually accepting connections -- lets a caller poll for readiness without depending on any particular nc/netcat flag set being available (confirmed against a real archiso initramfs: busybox's nc there doesn't have -z at all, which silently broke the naive nc-based poll every time)")
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "omarchy-nbd-bridge: --url is required")
		os.Exit(1)
	}

	cacheBytes, err := parseSize(*cacheSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "omarchy-nbd-bridge: --cache-size: %v\n", err)
		os.Exit(1)
	}

	// DNS-pinned dialer, not the default transport's resolve-every-dial
	// behavior: this bridge only ever talks to one host for its entire
	// lifetime, so there's no reason a *later* local-resolver hiccup
	// should be able to break connectivity that already worked once.
	// Confirmed against a real install: systemd-resolved's stub listener
	// at ::1:53 answered fine at startup, then stopped answering partway
	// through a long session -- every read from that point on failed
	// identically ("dial tcp: lookup ... connection refused"), no amount
	// of retrying helped, because retrying still re-resolved every time.
	// MaxIdleConnsPerHost set explicitly, not left at the default of 2:
	// readahead depth now scales with cache size (see cache.go's
	// prefetchCeiling, up to 64 concurrent fetches on a large cache) --
	// with the default, most of those connections would get closed and
	// re-opened rather than reused once concurrency exceeds 2, adding a
	// fresh TCP+TLS handshake to nearly every fetch and quietly eating
	// the readahead win this is meant to buy. Matches prefetchCeiling
	// since that's the real ceiling on how many connections to this one
	// host are ever in flight at once.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:         (&pinnedDialer{}).DialContext,
			MaxIdleConnsPerHost: 64,
		},
	}

	hb, err := httpbackend.New(*url, client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "omarchy-nbd-bridge: fetching %s: %v\n", *url, err)
		os.Exit(1)
	}
	log.Printf("serving %s (%d bytes) with a %d-byte cache, %d-byte blocks", *url, hb.Size(), cacheBytes, blockSize)

	c := cache.New(hb, blockSize, cacheBytes)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "omarchy-nbd-bridge: listening on %s: %v\n", *listen, err)
		os.Exit(1)
	}
	log.Printf("listening on %s", *listen)

	if *readyFile != "" {
		if err := os.WriteFile(*readyFile, []byte("1"), 0644); err != nil {
			log.Printf("writing ready-file %s: %v (continuing anyway -- this is just a readiness signal, not fatal)", *readyFile, err)
		}
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go func() {
			defer conn.Close()
			if err := nbdproto.Serve(conn, c); err != nil {
				log.Printf("session %s: %v", conn.RemoteAddr(), err)
			}
		}()
	}
}

// pinnedDialer resolves its target host on the first successful dial and
// reuses that exact address for every later dial, instead of letting the
// transport re-resolve DNS from scratch each time. Falls back to a fresh
// resolve only if the cached address itself stops being reachable (e.g.
// a CDN edge IP genuinely rotating), so it can't get permanently stuck on
// a stale address either.
type pinnedDialer struct {
	mu       sync.Mutex
	resolved string
	inner    net.Dialer
}

func (d *pinnedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	cached := d.resolved
	d.mu.Unlock()

	if cached != "" {
		if conn, err := d.inner.DialContext(ctx, network, cached); err == nil {
			return conn, nil
		}
	}

	conn, err := d.inner.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.resolved = conn.RemoteAddr().String()
	d.mu.Unlock()
	return conn, nil
}

// parseSize parses a size string like "256M" or "1G" into bytes.
// Suffixes: K/M/G (base 1024). No suffix = bytes.
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	numPart := s
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1 << 10
		numPart = s[:len(s)-1]
	case 'M', 'm':
		mult = 1 << 20
		numPart = s[:len(s)-1]
	case 'G', 'g':
		mult = 1 << 30
		numPart = s[:len(s)-1]
	}
	var n int64
	if _, err := fmt.Sscanf(numPart, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}
