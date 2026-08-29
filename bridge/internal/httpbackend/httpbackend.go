// Package httpbackend implements backend.Backend by fetching bytes from a
// remote URL on demand via HTTP Range requests. This is the actual "pulls
// bytes from a blob storage URL" piece the whole project exists for --
// see ../../DESIGN.md M1 (basic Range fetching) and M3 (retries, timeouts,
// Content-Range validation).
package httpbackend

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	urlpkg "net/url"
	"strings"
	"time"
)

// HTTP is a backend.Backend backed by a remote URL. See httpbackend_test.go
// for the exact behavior expected at each milestone:
//   - M1: ReadAt must send a Range header and use the returned bytes
//     as-is; New must determine and report the object's total size.
//   - M3: transient failures (5xx, connection errors) must be retried
//     with some bounded number of attempts before giving up; a response
//     that isn't 206 Partial Content, or whose Content-Range doesn't
//     match what was actually requested, must be treated as an error
//     rather than trusted -- some servers/CDNs round a requested range to
//     their own chunk boundaries, and blindly accepting whatever came
//     back would hand corrupted data up to the NBD layer. The client's
//     own timeout (client.Timeout) must be respected, not silently
//     multiplied by retries into an effectively unbounded wait.
type HTTP struct {
	Url         *urlpkg.URL
	ContentSize int64
	Client      *http.Client
}

// New probes url to determine its total size (however you choose to do
// that -- a HEAD request and a zero-length Range GET are both reasonable;
// httpbackend_test.go's fault-injection handlers respond correctly to
// either) and returns a ready-to-use Backend. Returns an error if the
// size can't be determined -- per DESIGN.md's CLI contract, the caller
// (the bridge's main()) treats this as a fatal startup error, so failing
// fast here with a clear error matters.
func New(url string, client *http.Client) (*HTTP, error) {
	purl, err := urlpkg.Parse(url)
	if err != nil {
		return nil, err
	}
	req := &http.Request{Method: http.MethodHead, URL: purl}
	res, err := RetryRequest(client, req)
	if err != nil {
		return nil, err
	}
	return &HTTP{purl, res.ContentLength, client}, nil
}

func (h *HTTP) Size() int64 { return h.ContentSize }

func (h *HTTP) ReadAt(p []byte, off int64) (int, error) {
	head := http.Header{}
	reqrange := fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1)
	head.Set("Range", reqrange)
	req := &http.Request{Header: head, URL: h.Url}
	res, err := RetryRequest(h.Client, req, http.StatusPartialContent)
	if err != nil {
		return 0, err
	}
	if resrange := strings.Replace(strings.Split(res.Header.Get("Content-Range"), "/")[0], " ", "=", 1); resrange != reqrange {
		return 0, errors.New("request range did not match response range")
	}
	if rescl := res.ContentLength; rescl != int64(len(p)) {
		return 0, errors.New("request size did not match response size")
	}
	return io.ReadFull(res.Body, p)
}

// RetryRequest retries r up to a bounded number of times on transient
// failures: 5xx responses, and network-level errors (connection reset,
// refused, DNS hiccups, unexpected EOF -- the kind of blip a real WAN link
// to a real host genuinely produces over the course of a long read, unlike
// the zero-packet-loss local nginx this retry path was originally
// validated against). A *timeout* specifically is never retried -- it's
// the caller's own configured hard cap on total wait per DESIGN.md's M3
// requirement ("must be respected, not silently multiplied by retries
// into an effectively unbounded wait"), and retrying it would multiply
// that cap by the attempt count instead of honoring it.
func RetryRequest(c *http.Client, r *http.Request, desiredCode ...int) (*http.Response, error) {
	const maxAttempts = 6
	const retryDelay = 50 * time.Millisecond

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}

		res, err := c.Do(r)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return nil, err
			}
			lastErr = err
			continue
		}

		if res.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s", res.Status)
			continue
		}

		dc := 200
		if len(desiredCode) > 0 {
			dc = desiredCode[0]
		}
		if res.StatusCode != dc {
			return nil, errors.New(res.Status)
		}
		return res, nil
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", maxAttempts, lastErr)
}
