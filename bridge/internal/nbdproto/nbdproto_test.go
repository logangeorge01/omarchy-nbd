package nbdproto_test

// Drives Serve() as a fake NBD client would, over net.Pipe() -- no real
// kernel NBD client or nbd-client binary needed to run these. Byte layout
// asserted here matches DESIGN.md's "NBD protocol reference" section
// exactly (pulled from the upstream spec).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"omarchy-nbd-bridge/internal/backend"
	"omarchy-nbd-bridge/internal/nbdproto"
)

const (
	nbdMagicVal      uint64 = 0x4e42444d41474943
	ihaveoptVal      uint64 = 0x49484156454f5054
	nbdOptExportName uint32 = 1

	nbdRequestMagic uint32 = 0x25609513
	nbdSimpleReply  uint32 = 0x67446698

	cmdRead  uint16 = 0
	cmdWrite uint16 = 1
	cmdDisc  uint16 = 2

	flagFixedNewstyle  uint16 = 0x0001
	clientFlagFixedNew uint32 = 0x00000001

	flagHasFlags uint16 = 0x0001
	flagReadOnly uint16 = 0x0002
)

// ---- fake client -----------------------------------------------------

type testClient struct {
	t    *testing.T
	conn net.Conn
}

func newTestClient(t *testing.T, conn net.Conn) *testClient {
	return &testClient{t: t, conn: conn}
}

func (c *testClient) readN(n int) []byte {
	c.t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		c.t.Fatalf("reading %d bytes: %v", n, err)
	}
	return buf
}

func (c *testClient) writeAll(b []byte) {
	c.t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("writing %d bytes: %v", len(b), err)
	}
}

// doHandshake performs the fixed newstyle handshake plus a single
// NBD_OPT_EXPORT_NAME negotiation, and returns the export size the server
// reports.
func (c *testClient) doHandshake(exportName string) (exportSize int64) {
	c.t.Helper()

	if got := binary.BigEndian.Uint64(c.readN(8)); got != nbdMagicVal {
		c.t.Fatalf("bad NBDMAGIC: got %x, want %x", got, nbdMagicVal)
	}
	// fmt.Println("magic")
	if got := binary.BigEndian.Uint64(c.readN(8)); got != ihaveoptVal {
		c.t.Fatalf("bad IHAVEOPT: got %x, want %x", got, ihaveoptVal)
	}
	// fmt.Println("ihaveopt")
	hsFlags := binary.BigEndian.Uint16(c.readN(2))
	if hsFlags&flagFixedNewstyle == 0 {
		c.t.Fatalf("server did not advertise NBD_FLAG_FIXED_NEWSTYLE in handshake flags: %#x", hsFlags)
	}
	// fmt.Println("flag")

	cf := make([]byte, 4)
	binary.BigEndian.PutUint32(cf, clientFlagFixedNew)
	c.writeAll(cf)

	var opt bytes.Buffer
	_ = binary.Write(&opt, binary.BigEndian, ihaveoptVal)
	_ = binary.Write(&opt, binary.BigEndian, nbdOptExportName)
	_ = binary.Write(&opt, binary.BigEndian, uint32(len(exportName)))
	opt.WriteString(exportName)
	c.writeAll(opt.Bytes())
	// fmt.Println("wrote pt 2")

	exportSize = int64(binary.BigEndian.Uint64(c.readN(8)))
	// fmt.Printf("exportsize client: %d\n", exportSize)
	txFlags := binary.BigEndian.Uint16(c.readN(2))
	if txFlags&flagHasFlags == 0 {
		c.t.Fatalf("NBD_FLAG_HAS_FLAGS not set in transmission flags: %#x", txFlags)
	}
	if txFlags&flagReadOnly == 0 {
		c.t.Fatalf("NBD_FLAG_READ_ONLY not set -- this server must always be read-only: %#x", txFlags)
	}
	c.readN(124) // zero padding (assumes NBD_FLAG_C_NO_ZEROES was NOT negotiated, which this test client never does)

	return exportSize
}

func (c *testClient) sendRead(cookie uint64, offset uint64, length uint32) {
	c.t.Helper()
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, nbdRequestMagic)
	_ = binary.Write(&buf, binary.BigEndian, uint16(0)) // command flags
	_ = binary.Write(&buf, binary.BigEndian, cmdRead)
	_ = binary.Write(&buf, binary.BigEndian, cookie)
	_ = binary.Write(&buf, binary.BigEndian, offset)
	_ = binary.Write(&buf, binary.BigEndian, length)
	c.writeAll(buf.Bytes())
}

func (c *testClient) sendWrite(cookie uint64, offset uint64, data []byte) {
	c.t.Helper()
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, nbdRequestMagic)
	_ = binary.Write(&buf, binary.BigEndian, uint16(0))
	_ = binary.Write(&buf, binary.BigEndian, cmdWrite)
	_ = binary.Write(&buf, binary.BigEndian, cookie)
	_ = binary.Write(&buf, binary.BigEndian, offset)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(data)))
	buf.Write(data)
	c.writeAll(buf.Bytes())
}

func (c *testClient) sendDisc() {
	c.t.Helper()
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, nbdRequestMagic)
	_ = binary.Write(&buf, binary.BigEndian, uint16(0))
	_ = binary.Write(&buf, binary.BigEndian, cmdDisc)
	_ = binary.Write(&buf, binary.BigEndian, uint64(0)) // cookie, don't care
	_ = binary.Write(&buf, binary.BigEndian, uint64(0)) // offset MUST be 0 per spec
	_ = binary.Write(&buf, binary.BigEndian, uint32(0)) // length MUST be 0 per spec
	c.writeAll(buf.Bytes())
}

// readReplyHeader reads one simple-reply header. Caller reads the data
// payload separately (readN) when errno == 0.
func (c *testClient) readReplyHeader() (errno uint32, cookie uint64) {
	c.t.Helper()
	magic := binary.BigEndian.Uint32(c.readN(4))
	if magic != nbdSimpleReply {
		c.t.Fatalf("bad NBD_SIMPLE_REPLY_MAGIC: got %x, want %x", magic, nbdSimpleReply)
	}
	errno = binary.BigEndian.Uint32(c.readN(4))
	cookie = binary.BigEndian.Uint64(c.readN(8))
	return errno, cookie
}

// pipe starts nbdproto.Serve on one end of an in-process net.Pipe()
// against b, and returns the client end plus a channel receiving Serve's
// return value once the session ends.
func pipe(t *testing.T, b backend.Backend) (*testClient, <-chan error) {
	t.Helper()
	serverConn, clientConn := net.Pipe()

	// Without this, a Serve that hangs or never writes what's expected
	// (the current TODO stub included) would block every read forever
	// instead of failing the test promptly -- net.Pipe()'s reads/writes
	// are synchronous, so there's no other natural timeout here.
	if err := clientConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		// Recovered here (rather than left to crash the whole test
		// binary) so a panic in Serve -- including the current
		// not-yet-implemented stub, but just as importantly any real
		// bug later -- shows up as a normal failure on whichever test
		// triggered it, instead of taking down every other test in
		// this package along with it.
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("Serve panicked: %v", r)
			}
		}()
		done <- nbdproto.Serve(serverConn, b)
	}()
	t.Cleanup(func() { clientConn.Close() })
	return newTestClient(t, clientConn), done
}

// ---- tests -------------------------------------------------------------

func TestHandshake_AdvertisesFixedNewstyleAndReadOnly(t *testing.T) {
	data := []byte("hello world, this is the test backend content")
	c, _ := pipe(t, backend.NewMem(data))

	size := c.doHandshake("")
	// fmt.Printf("size: %d\n", size)
	if size != int64(len(data)) {
		t.Errorf("export size = %d, want %d", size, len(data))
	}
}

func TestHandshake_AcceptsAnyExportName(t *testing.T) {
	// DESIGN.md is explicit: never reject on export name, always serve
	// the one thing this bridge has. archiso's real archiso_pxe_nbd hook
	// requests "archiso" by default -- but nothing should hardcode a
	// check requiring exactly that.
	data := []byte("export name should not matter at all")
	for _, name := range []string{"", "archiso", "anything-else-entirely"} {
		name := name
		t.Run(name, func(t *testing.T) {
			c, _ := pipe(t, backend.NewMem(data))
			size := c.doHandshake(name)
			if size != int64(len(data)) {
				t.Errorf("export name %q: size = %d, want %d", name, size, len(data))
			}
		})
	}
}

func TestHandshake_FallsBackToExportNameAfterUnsupportedOption(t *testing.T) {
	// Real clients (confirmed against the actual nbd-client binary, not
	// just this fake test client) try more capable options first --
	// NBD_OPT_GO in particular -- before falling back to plain
	// NBD_OPT_EXPORT_NAME. A server that doesn't understand an option
	// still has to reply with the generic NBD_REP_ERR_UNSUP option-reply
	// format, not just drop the connection, or a real client has no
	// signal to fall back on and negotiation dies right here.
	const (
		optGo               uint32 = 7
		optionReplyMagic    uint64 = 0x3e889045565a9
		repErrUnsup         uint32 = 0x80000001
	)
	data := []byte("export name should not matter at all")
	c, _ := pipe(t, backend.NewMem(data))

	if got := binary.BigEndian.Uint64(c.readN(8)); got != nbdMagicVal {
		c.t.Fatalf("bad NBDMAGIC: got %x", got)
	}
	if got := binary.BigEndian.Uint64(c.readN(8)); got != ihaveoptVal {
		c.t.Fatalf("bad IHAVEOPT: got %x", got)
	}
	c.readN(2) // handshake flags
	cf := make([]byte, 4)
	binary.BigEndian.PutUint32(cf, clientFlagFixedNew)
	c.writeAll(cf)

	// Send an option the server has no reason to support.
	var opt bytes.Buffer
	_ = binary.Write(&opt, binary.BigEndian, ihaveoptVal)
	_ = binary.Write(&opt, binary.BigEndian, optGo)
	optData := []byte("some request data the server should just discard")
	_ = binary.Write(&opt, binary.BigEndian, uint32(len(optData)))
	opt.Write(optData)
	c.writeAll(opt.Bytes())

	// Must get a well-formed "unsupported" reply back, not a dropped
	// connection.
	replyMagic := binary.BigEndian.Uint64(c.readN(8))
	if replyMagic != optionReplyMagic {
		c.t.Fatalf("option reply magic = %#x, want %#x", replyMagic, optionReplyMagic)
	}
	repOpt := binary.BigEndian.Uint32(c.readN(4))
	if repOpt != optGo {
		c.t.Fatalf("option reply echoed option = %d, want %d", repOpt, optGo)
	}
	repType := binary.BigEndian.Uint32(c.readN(4))
	if repType != repErrUnsup {
		c.t.Fatalf("option reply type = %#x, want NBD_REP_ERR_UNSUP (%#x)", repType, repErrUnsup)
	}
	repLen := binary.BigEndian.Uint32(c.readN(4))
	c.readN(int(repLen)) // drain whatever reply data, if any

	// Now the classic fallback must still work on the same connection --
	// send NBD_OPT_EXPORT_NAME same as doHandshake's tail end would, and
	// confirm it succeeds normally.
	exportName := "archiso"
	var exp bytes.Buffer
	_ = binary.Write(&exp, binary.BigEndian, ihaveoptVal)
	_ = binary.Write(&exp, binary.BigEndian, nbdOptExportName)
	_ = binary.Write(&exp, binary.BigEndian, uint32(len(exportName)))
	exp.WriteString(exportName)
	c.writeAll(exp.Bytes())

	size := int64(binary.BigEndian.Uint64(c.readN(8)))
	if size != int64(len(data)) {
		t.Errorf("export size after fallback = %d, want %d", size, len(data))
	}
	txFlags := binary.BigEndian.Uint16(c.readN(2))
	if txFlags&flagReadOnly == 0 {
		t.Errorf("NBD_FLAG_READ_ONLY not set after fallback: %#x", txFlags)
	}
	c.readN(124) // zero padding
}

func TestRead_ExactBytesAtOffset(t *testing.T) {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 251) // recognizable, non-trivially-repeating pattern
	}
	c, _ := pipe(t, backend.NewMem(data))
	c.doHandshake("")

	cases := []struct {
		name   string
		offset uint64
		length uint32
	}{
		{"from start", 0, 512},
		{"mid-file", 1000, 100},
		{"up to exact end", uint64(len(data) - 256), 256},
		{"single byte", 42, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cookie := uint64(0x1122334455667788)
			c.sendRead(cookie, tc.offset, tc.length)
			errno, gotCookie := c.readReplyHeader()
			if errno != 0 {
				t.Fatalf("errno = %d, want 0", errno)
			}
			if gotCookie != cookie {
				t.Fatalf("cookie = %x, want %x", gotCookie, cookie)
			}
			got := c.readN(int(tc.length))
			want := data[tc.offset : tc.offset+uint64(tc.length)]
			if !bytes.Equal(got, want) {
				t.Fatalf("data mismatch at offset %d length %d", tc.offset, tc.length)
			}
		})
	}
}

func TestRead_CookieCorrelationAcrossMultipleRequests(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 8192)
	c, _ := pipe(t, backend.NewMem(data))
	c.doHandshake("")

	// Fire off several reads with distinct cookies before reading any
	// replies -- a correct implementation must not assume replies are
	// read in lockstep with requests, and must echo the right cookie
	// back for each even if it happens to process them one at a time
	// internally.
	cookies := []uint64{111, 222, 333, 444}
	for i, ck := range cookies {
		c.sendRead(ck, uint64(i*100), 50)
	}
	for _, wantCookie := range cookies {
		errno, gotCookie := c.readReplyHeader()
		if errno != 0 {
			t.Fatalf("errno = %d, want 0", errno)
		}
		if gotCookie != wantCookie {
			t.Fatalf("cookie = %x, want %x -- replies must correlate to their request, not just arrive in send order by accident", gotCookie, wantCookie)
		}
		c.readN(50) // drain data
	}
}

func TestRead_BeyondEndOfExport_ErrorsWithoutBreakingSession(t *testing.T) {
	data := make([]byte, 100)
	c, _ := pipe(t, backend.NewMem(data))
	c.doHandshake("")

	c.sendRead(1, 90, 50) // offset+length = 140, past the 100-byte export
	errno, _ := c.readReplyHeader()
	if errno == 0 {
		t.Fatalf("errno = 0, want a nonzero error for a read past the end of the export")
	}

	// The session must still be usable afterward -- one bad request
	// shouldn't take down the whole connection.
	c.sendRead(2, 0, 10)
	errno, _ = c.readReplyHeader()
	if errno != 0 {
		t.Fatalf("session broken after a prior out-of-range read: errno = %d", errno)
	}
	c.readN(10)
}

func TestWrite_AlwaysRejectedReadOnly(t *testing.T) {
	data := make([]byte, 100)
	c, _ := pipe(t, backend.NewMem(data))
	c.doHandshake("")

	c.sendWrite(7, 0, []byte("nope"))
	errno, cookie := c.readReplyHeader()
	if errno == 0 {
		t.Fatalf("errno = 0, want nonzero (EROFS-equivalent) -- this export must never accept writes")
	}
	if cookie != 7 {
		t.Fatalf("cookie = %d, want 7", cookie)
	}

	// The write's data payload must have been consumed off the wire, not
	// left sitting there to be misread as the start of the next request
	// -- otherwise everything after a rejected write desyncs and corrupts
	// the rest of the session.
	c.sendRead(8, 0, 10)
	errno, cookie = c.readReplyHeader()
	if errno != 0 {
		t.Fatalf("errno = %d, want 0 -- session desynced after a rejected write, its data payload was probably not drained from the wire", errno)
	}
	if cookie != 8 {
		t.Fatalf("cookie = %d, want 8 -- session desynced after a rejected write", cookie)
	}
	c.readN(10)
}

func TestDisc_ClosesCleanlyWithNoReply(t *testing.T) {
	data := make([]byte, 100)
	c, done := pipe(t, backend.NewMem(data))
	c.doHandshake("")

	c.sendDisc()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned an error after a clean NBD_CMD_DISC: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within 2s of NBD_CMD_DISC -- spec says no reply is sent at all, the connection should just close")
	}
}
