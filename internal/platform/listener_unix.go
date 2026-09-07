//go:build !windows

package platform

import (
	"context"
	"fmt"
	"net"
	"os"
)

// Listen opens a unix-domain socket listener at name (used verbatim as the
// socket filesystem path, matching src/server/daemon.ts). A stale socket file
// from a previous crash is removed first — the same unlink-before-bind the
// Node daemon performs; a directory or live socket at that path makes the
// removal fail and the bind error surfaces loudly.
func Listen(name string) (net.Listener, error) {
	if name == "" {
		return nil, fmt.Errorf("platform: Listen: empty socket path")
	}
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("platform: remove stale socket %s: %w", name, err)
	}
	l, err := net.Listen("unix", name)
	if err != nil {
		return nil, fmt.Errorf("platform: listen unix %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("platform: chmod socket %s: %w", name, err)
	}
	return l, nil
}

// DialContext returns a dial function for http.Transport: a plain
// unix-domain dial over name on every unix GOOS.
func DialContext(name string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", name)
	}
}
