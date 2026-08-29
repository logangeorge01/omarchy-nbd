package backend

import (
	"fmt"
)

// Mem is the simplest possible Backend: a fixed in-memory byte buffer.
// This is M0 in DESIGN.md -- get this wired through the full NBD protocol
// stack correctly (nbdproto package) before anything HTTP-related is
// involved at all. Also directly useful as a test double for cache tests.
type Mem struct {
	data []byte
}

// NewMem wraps data as a Backend. data is not copied -- don't mutate it
// after passing it in.
func NewMem(data []byte) *Mem {
	return &Mem{data: data}
}

func (m *Mem) Size() int64 {
	// fmt.Println("READ")
	return int64(len(m.data))
}

func (m *Mem) ReadAt(p []byte, off int64) (int, error) {
	// fmt.Println("READ")
	if off < 0 || off > int64(len(m.data)) {
		return 0, fmt.Errorf("backend: offset %d out of range [0, %d]", off, len(m.data))
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, fmt.Errorf("backend: short read at offset %d: got %d bytes, wanted %d", off, n, len(p))
	}
	return n, nil
}
