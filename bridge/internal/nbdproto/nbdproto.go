// Package nbdproto implements the server side of the NBD wire protocol
// (fixed newstyle handshake, single read-only export, simple-reply
// transmission phase). This is M0 in ../../DESIGN.md -- the part worth
// getting exactly right before anything HTTP-related is involved, since a
// wrong magic number here silently hangs or disconnects with no useful
// error, rather than failing loudly.
//
// Exact wire format is documented in DESIGN.md's "NBD protocol reference"
// section, pulled from the upstream spec, not from memory -- cross-check
// there (or https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md)
// if anything here is ambiguous. nbdproto_test.go exercises the same
// byte-level detail from a fake client's point of view using net.Pipe(),
// so a correct Serve() implementation should make every test in that file
// pass without needing a real kernel NBD client at all.
package nbdproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"syscall"

	"omarchy-nbd-bridge/internal/backend"
)

const (
	nbdMagicVal      uint64 = 0x4e42444d41474943
	ihaveoptVal      uint64 = 0x49484156454f5054
	nbdOptExportName uint32 = 1

	// Generic option-reply protocol (used to tell a client "not
	// supported, try something else" for any option besides
	// NBD_OPT_EXPORT_NAME -- real clients, e.g. nbd-client, try
	// NBD_OPT_GO first and only fall back to EXPORT_NAME once they see
	// this).
	optionReplyMagic uint64 = 0x3e889045565a9
	repErrUnsup      uint32 = 0x80000001

	nbdRequestMagic uint32 = 0x25609513
	nbdSimpleReply  uint32 = 0x67446698

	flagFixedNewstyle uint16 = 0x0001

	flagHasFlags uint16 = 0x0001
	flagReadOnly uint16 = 0x0002

	cmdRead  uint16 = 0
	cmdWrite uint16 = 1
	cmdDisc  uint16 = 2
)

// Serve runs a single NBD server session to completion on conn: the fixed
// newstyle handshake, option negotiation, then the transmission phase
// against b, until the client sends NBD_CMD_DISC or the connection
// closes.
//
// Requirements (see nbdproto_test.go for the exact assertions):
//   - Accept ANY export name during NBD_OPT_EXPORT_NAME -- never reject.
//     archiso's real archiso_pxe_nbd hook requests "archiso" by default,
//     but nothing should hardcode a check against that.
//   - Always advertise NBD_FLAG_READ_ONLY. NBD_CMD_WRITE must always
//     error, never actually write anything.
//   - Reply cookies must correlate exactly to their request's cookie,
//     even under concurrent/pipelined requests.
//   - A read past the end of the export must reply with a nonzero error
//     and leave the session usable for subsequent requests -- not hang,
//     not panic, not kill the connection.
//   - NBD_CMD_DISC: no reply at all, just close cleanly. Serve should
//     return nil in this case.
func Serve(conn net.Conn, b backend.Backend) error {
	buf64 := make([]byte, 8)
	buf32 := make([]byte, 4)
	buf16 := make([]byte, 2)

	// 1
	binary.BigEndian.PutUint64(buf64, nbdMagicVal)
	conn.Write(buf64)
	binary.BigEndian.PutUint64(buf64, ihaveoptVal)
	conn.Write(buf64)
	binary.BigEndian.PutUint16(buf16, flagFixedNewstyle)
	conn.Write(buf16)
	if _, err := io.ReadFull(conn, buf32); err != nil {
		return fmt.Errorf("reading client flags: %w", err)
	}
	cf := binary.BigEndian.Uint32(buf32)
	if cf&uint32(flagFixedNewstyle) == 0 {
		return errors.New("didnt get newstyle flag")
	}

	// 2: option haggling. Real clients (confirmed against the actual
	// nbd-client binary, not just this package's own fake test client)
	// try more capable options first -- NBD_OPT_GO in particular -- and
	// only fall back to plain NBD_OPT_EXPORT_NAME once those come back
	// unsupported. A server that only ever understands EXPORT_NAME still
	// has to speak the generic option-reply protocol for everything else
	// it doesn't support, or the client has no signal to fall back on and
	// the connection just dies during negotiation. So: loop, acking
	// EXPORT_NAME (ending the loop) and NBD_REP_ERR_UNSUP-ing anything
	// else (draining that option's data first so the stream stays in
	// sync), until EXPORT_NAME shows up.
	var exportNameLen uint32
	for {
		if _, err := io.ReadFull(conn, buf64); err != nil {
			return fmt.Errorf("reading IHAVEOPT: %w", err)
		}
		iho := binary.BigEndian.Uint64(buf64)
		if iho != ihaveoptVal {
			return errors.New("didnt get ihaveopt")
		}
		if _, err := io.ReadFull(conn, buf32); err != nil {
			return fmt.Errorf("reading option type: %w", err)
		}
		opt := binary.BigEndian.Uint32(buf32)
		if _, err := io.ReadFull(conn, buf32); err != nil {
			return fmt.Errorf("reading option data length: %w", err)
		}
		optLen := binary.BigEndian.Uint32(buf32)

		if opt == nbdOptExportName {
			exportNameLen = optLen
			break
		}

		// Any other option: drain its data (still has to come off the
		// wire even though we're not honoring it) and reply unsupported.
		if _, err := io.CopyN(io.Discard, conn, int64(optLen)); err != nil {
			return fmt.Errorf("reading option data for option %d: %w", opt, err)
		}
		replyBuf := make([]byte, 20)
		binary.BigEndian.PutUint64(replyBuf[0:8], optionReplyMagic)
		binary.BigEndian.PutUint32(replyBuf[8:12], opt)
		binary.BigEndian.PutUint32(replyBuf[12:16], repErrUnsup)
		binary.BigEndian.PutUint32(replyBuf[16:20], 0)
		if _, err := conn.Write(replyBuf); err != nil {
			return fmt.Errorf("writing option reply for option %d: %w", opt, err)
		}
	}

	bufN := make([]byte, exportNameLen)
	if _, err := io.ReadFull(conn, bufN); err != nil {
		return fmt.Errorf("reading export name: %w", err)
	}
	// exportName := string(bufN)

	// fmt.Printf("size: %d\n", b.Size())
	binary.BigEndian.PutUint64(buf64, uint64(b.Size()))
	// fmt.Println(buf64)
	conn.Write(buf64)

	binary.BigEndian.PutUint16(buf16, flagHasFlags|flagReadOnly)
	conn.Write(buf16)

	buf124 := make([]byte, 124)
	conn.Write(buf124)

	// fmt.Println("2 done")

	requests := make(chan Request, 16)

	// Reads requests off the wire and hands them to the reply loop below.
	// Every exit path returns (never continues past a read error) and the
	// deferred close(requests) runs exactly once -- a persistent read
	// error here (client gone, connection reset, garbage on the wire)
	// used to `continue` forever with no backoff, which spun this
	// goroutine at 100% CPU indefinitely instead of ending the session,
	// for as long as the process lived. Any short read now just ends the
	// session, same as an explicit NBD_CMD_DISC would.
	go func() {
		defer close(requests)
		for {
			req := Request{}
			if _, err := io.ReadFull(conn, buf32); err != nil {
				return
			}
			magic := binary.BigEndian.Uint32(buf32)
			if magic != nbdRequestMagic {
				return
			}
			if _, err := io.ReadFull(conn, buf16); err != nil { // command flags, unused
				return
			}
			if _, err := io.ReadFull(conn, buf16); err != nil {
				return
			}
			nbdcmd := binary.BigEndian.Uint16(buf16)
			if _, err := io.ReadFull(conn, buf64); err != nil {
				return
			}
			req.Cookie = binary.BigEndian.Uint64(buf64)
			if _, err := io.ReadFull(conn, buf64); err != nil {
				return
			}
			req.Offset = binary.BigEndian.Uint64(buf64)
			if _, err := io.ReadFull(conn, buf32); err != nil {
				return
			}
			req.Length = binary.BigEndian.Uint32(buf32)

			if nbdcmd == cmdDisc {
				conn.Close()
				return
			}

			if nbdcmd == cmdWrite {
				// The write payload still has to come off the wire even
				// though it's always rejected -- otherwise the next
				// request's bytes get misread as leftover write data,
				// corrupting the rest of the session instead of just
				// rejecting this one command.
				discard := make([]byte, req.Length)
				if _, err := io.ReadFull(conn, discard); err != nil {
					return
				}
				req.Error = errors.New("read-only export: writes are not permitted")
			} else if nbdcmd != cmdRead {
				req.Error = fmt.Errorf("unsupported command %d", nbdcmd)
			}

			requests <- req
		}
	}()

	// 3
	for {
		req, ok := <-requests

		if !ok {
			return nil
		}

		// response
		wbuf64 := make([]byte, 8)
		wbuf32 := make([]byte, 4)

		var data []byte
		errno := uint32(0)
		if req.Error != nil {
			errno = uint32(syscall.EROFS)
		} else {
			data = make([]byte, req.Length)
			if _, err := b.ReadAt(data, int64(req.Offset)); err != nil {
				// This used to be silently swallowed into a bare EIO --
				// confirmed against a real, persistent (not transient)
				// read failure that this logging was needed to even start
				// diagnosing, since nothing else in the pipeline reports
				// *why* a read failed.
				log.Printf("read at offset %d length %d failed: %v", req.Offset, req.Length, err)
				errno = uint32(syscall.EIO)
			}
		}

		binary.BigEndian.PutUint32(wbuf32, nbdSimpleReply)
		conn.Write(wbuf32)

		// error
		binary.BigEndian.PutUint32(wbuf32, errno)
		conn.Write(wbuf32)

		binary.BigEndian.PutUint64(wbuf64, req.Cookie)
		conn.Write(wbuf64)

		if errno == 0 {
			conn.Write(data)
		}
	}
}

type Request struct {
	Cookie uint64
	Offset uint64
	Length uint32
	Error  error
}
