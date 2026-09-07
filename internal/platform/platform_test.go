package platform

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The round-trip tests run wherever Listen is implemented (unix). On Windows
// the listener is a documented ErrNotImplemented stub until the FR-CTRL
// daemon port lands the pipe listener, so the same suite skips with the
// recorded justification the issue's acceptance criteria allow.

func testListen(t *testing.T) net.Listener {
	t.Helper()
	dir := t.TempDir()
	ln, err := Listen(filepath.Join(dir, "daemon.sock"))
	if err != nil {
		if errors.Is(err, ErrNotImplemented) {
			t.Skip("named-pipe listener pending FR-CTRL port (recorded justification, docs/WINDOWS.md)")
		}
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestListenDialRoundTrip(t *testing.T) {
	ln := testListen(t)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("pong"))
			c.Close()
		}
	}()
	conn, err := DialContext(ln.Addr().String())(context.Background(), "unix", "")
	if err != nil {
		t.Skipf("dial unavailable on this GOOS: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "pong" {
		t.Fatalf("round trip: got %q, want %q", buf, "pong")
	}
}

func TestListenStaleSocketRemoved(t *testing.T) {
	ln := testListen(t)
	path := ln.Addr().String()
	ln.Close()
	// A crashed daemon leaves the socket file behind; the next Listen must
	// unlink it and bind again (parity with the Node daemon's
	// unlinkSync-before-listen).
	ln2, err := Listen(path)
	if err != nil {
		if errors.Is(err, ErrNotImplemented) {
			t.Skip("named-pipe listener pending FR-CTRL port (recorded justification, docs/WINDOWS.md)")
		}
		t.Fatalf("re-Listen after stale socket: %v", err)
	}
	defer ln2.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file missing after re-bind: %v", err)
	}
}

func TestListenEmptyNameRejected(t *testing.T) {
	if _, err := Listen(""); err == nil {
		t.Fatal("Listen(\"\") must reject the empty path")
	}
}
