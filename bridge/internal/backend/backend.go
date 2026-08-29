// Package backend defines the shared abstraction every NBD export in this
// project is built on top of, and provides the trivial M0 implementation.
package backend

import "io"

// Backend is anything that can serve fixed-size, random-access reads.
// Satisfied, in build order (see ../../bridge/DESIGN.md): Mem (M0, this
// package), httpbackend.HTTP (M1/M3), cache.Cache wrapping either (M2).
type Backend interface {
	io.ReaderAt

	// Size returns the fixed total size of the backend, in bytes. Must
	// never change for the lifetime of the backend -- NBD exports don't
	// support resizing mid-connection.
	Size() int64
}
