package httpbackend_test

// Uses httptest.Server throughout -- no real network, no Azure/GitHub
// dependency, runs anywhere `go test` runs. http.ServeContent (stdlib)
// already handles Range/206 correctly, so the happy-path tests lean on
// it; the M3 fault-injection tests use hand-written handlers to simulate
// specific failure modes that a real blob host can genuinely exhibit.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"omarchy-nbd-bridge/internal/httpbackend"
)

// respondToSizeProbe answers a HEAD request, or a GET asking only for the
// first byte, with the given total size -- either is a reasonable way to
// implement determining a remote object's size in New(), and this lets
// the fault-injection tests below stay agnostic to which one you pick.
// Returns true if it handled the request (caller should return without
// doing anything else); false means "this wasn't a size probe, handle it
// as a normal request."
func respondToSizeProbe(w http.ResponseWriter, r *http.Request, size int64) bool {
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.WriteHeader(http.StatusOK)
		return true
	}
	if rng := r.Header.Get("Range"); rng == "bytes=0-0" || rng == "bytes=0-" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size))
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
		return true
	}
	return false
}

func TestNew_SizeMatchesRemoteObjectSize(t *testing.T) {
	data := bytes.Repeat([]byte{0x42}, 10_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "test", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	b, err := httpbackend.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Size() != int64(len(data)) {
		t.Fatalf("Size() = %d, want %d", b.Size(), len(data))
	}
}

func TestReadAt_SendsRangeAndReturnsCorrectBytes(t *testing.T) {
	data := make([]byte, 1<<20) // 1MB
	for i := range data {
		data[i] = byte(i)
	}
	sawRange := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" && r.Header.Get("Range") != "bytes=0-0" && r.Header.Get("Range") != "bytes=0-" {
			sawRange = true
		}
		http.ServeContent(w, r, "test", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	b, err := httpbackend.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	buf := make([]byte, 100)
	n, err := b.ReadAt(buf, 500_000)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 100 {
		t.Fatalf("n = %d, want 100", n)
	}
	if !bytes.Equal(buf, data[500_000:500_100]) {
		t.Fatalf("data mismatch -- got wrong bytes back for the requested offset")
	}
	if !sawRange {
		t.Fatalf("no real Range header observed on the data request -- this must never fall back to fetching the whole object")
	}
}

func TestReadAt_RetriesTransientFailures(t *testing.T) {
	data := []byte("retry me please, this should eventually succeed")
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondToSizeProbe(w, r, int64(len(data))) {
			return
		}
		if atomic.AddInt32(&attempts, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError) // transient, not 404
			return
		}
		http.ServeContent(w, r, "test", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	b, err := httpbackend.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buf := make([]byte, len(data))
	n, err := b.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt should have retried past the transient 500s and succeeded: %v", err)
	}
	if n != len(data) || !bytes.Equal(buf, data) {
		t.Fatalf("data mismatch after retry")
	}
	if got := atomic.LoadInt32(&attempts); got < 3 {
		t.Fatalf("data request attempts = %d, want at least 3 (2 failures + 1 success)", got)
	}
}

// flakyTransport wraps a real transport but fails the first n non-size-probe
// requests with a plain network error (not a timeout) before letting
// everything through -- simulates the kind of connection-reset/EOF blip a
// real WAN link to a real host produces, which a 5xx-only retry policy
// (what this used to be) never sees or recovers from.
type flakyTransport struct {
	inner       http.RoundTripper
	failures    int32
	isSizeProbe func(*http.Request) bool
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !f.isSizeProbe(r) && atomic.AddInt32(&f.failures, -1) >= 0 {
		return nil, fmt.Errorf("simulated connection reset")
	}
	return f.inner.RoundTrip(r)
}

func TestReadAt_RetriesTransientNetworkErrors(t *testing.T) {
	data := []byte("survives a couple of connection resets before succeeding")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondToSizeProbe(w, r, int64(len(data))) {
			return
		}
		http.ServeContent(w, r, "test", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &flakyTransport{
			inner:    http.DefaultTransport,
			failures: 2,
			isSizeProbe: func(r *http.Request) bool {
				return r.Method == http.MethodHead
			},
		},
	}

	b, err := httpbackend.New(srv.URL, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buf := make([]byte, len(data))
	n, err := b.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt should have retried past simulated connection resets and succeeded: %v", err)
	}
	if n != len(data) || !bytes.Equal(buf, data) {
		t.Fatalf("data mismatch after retry")
	}
}

func TestReadAt_GivesUpAfterPersistentFailure(t *testing.T) {
	dataSize := int64(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondToSizeProbe(w, r, dataSize) {
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // every data request fails, forever
	}))
	defer srv.Close()

	b, err := httpbackend.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buf := make([]byte, 10)
	_, err = b.ReadAt(buf, 0)
	if err == nil {
		t.Fatalf("ReadAt succeeded against a server whose data requests always 500 -- retries must eventually give up and return an error, not loop forever")
	}
}

func TestNew_FailsFastOnPersistentlyUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := httpbackend.New(srv.URL, srv.Client())
	if err == nil {
		t.Fatalf("New succeeded against a server that always 500s -- per DESIGN.md's CLI contract this is a fatal startup error and should fail fast, not hang or succeed silently")
	}
}

func TestReadAt_RejectsNonPartialResponse(t *testing.T) {
	// A server that ignores the Range header and returns the whole
	// object with a plain 200 must not be silently trusted -- returning
	// wrong-offset data up to the NBD layer would corrupt the mount.
	data := []byte("this is the full object; this handler ignores Range entirely")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondToSizeProbe(w, r, int64(len(data))) {
			return
		}
		w.WriteHeader(http.StatusOK) // 200, not 206, on purpose
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	b, err := httpbackend.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buf := make([]byte, 5)
	_, err = b.ReadAt(buf, 10)
	if err == nil {
		t.Fatalf("ReadAt succeeded against a server that returned 200 instead of 206 for a ranged request -- must be treated as an error, not trusted")
	}
}

func TestReadAt_RejectsMismatchedContentRange(t *testing.T) {
	// Some servers/CDNs round a requested range to their own chunk
	// boundaries. Silently accepting whatever came back would hand the
	// NBD layer data read from the wrong offset.
	const totalSize = 1000
	data := bytes.Repeat([]byte{0x99}, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondToSizeProbe(w, r, totalSize) {
			return
		}
		// Always claims to return bytes 0-99, regardless of what was
		// actually requested.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-99/%d", totalSize))
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	b, err := httpbackend.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buf := make([]byte, 50)
	_, err = b.ReadAt(buf, 500) // asked for offset 500, server claims 0-99
	if err == nil {
		t.Fatalf("ReadAt succeeded despite a Content-Range that doesn't match the requested offset -- must be validated, not trusted")
	}
}

func TestReadAt_RespectsClientTimeout(t *testing.T) {
	block := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondToSizeProbe(w, r, 1000) {
			return
		}

		<-block
	}))
	defer srv.Close()
	defer close(block)

	client := &http.Client{
		Timeout: 200 * time.Millisecond,
	}

	b, err := httpbackend.New(srv.URL, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()

	buf := make([]byte, 10)
	_, err = b.ReadAt(buf, 0)

	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout")
	}

	if elapsed < 150*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("expected ~200ms timeout, got %v", elapsed)
	}
}
