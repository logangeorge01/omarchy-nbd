package main

import (
	"context"
	"net"
	"testing"
)

func TestPinnedDialer_ReusesResolvedAddressOnSubsequentDials(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer target.Close()

	// A second address that's guaranteed unreachable (bound then
	// immediately closed) -- if the dialer actually re-resolved instead
	// of reusing the cached address, asking it to dial this one would
	// fail, not silently succeed against the real target.
	unreachable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	unreachableAddr := unreachable.Addr().String()
	unreachable.Close()

	d := &pinnedDialer{}

	conn1, err := d.DialContext(context.Background(), "tcp", target.Addr().String())
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	conn1.Close()

	// Ask for the unreachable address this time -- a correct pinnedDialer
	// ignores it and reuses the address the first dial actually resolved.
	conn2, err := d.DialContext(context.Background(), "tcp", unreachableAddr)
	if err != nil {
		t.Fatalf("second dial should have reused the cached address instead of dialing %s: %v", unreachableAddr, err)
	}
	defer conn2.Close()
	if conn2.RemoteAddr().String() != target.Addr().String() {
		t.Fatalf("second dial connected to %s, want the cached address %s", conn2.RemoteAddr(), target.Addr())
	}
}

func TestPinnedDialer_FallsBackToFreshResolveIfCachedAddressStopsWorking(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	firstAddr := first.Addr().String()
	first.Close() // reachable at dial time below, then gone

	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer second.Close()

	d := &pinnedDialer{resolved: firstAddr}

	conn, err := d.DialContext(context.Background(), "tcp", second.Addr().String())
	if err != nil {
		t.Fatalf("dial should have fallen back to the requested (working) address once the cached one failed: %v", err)
	}
	defer conn.Close()
	if conn.RemoteAddr().String() != second.Addr().String() {
		t.Fatalf("connected to %s, want the fallback address %s", conn.RemoteAddr(), second.Addr())
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"256M": 256 << 20,
		"1G":   1 << 30,
		"512K": 512 << 10,
		"100":  100,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseSize_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "abc", "M"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) succeeded, want an error", in)
		}
	}
}
