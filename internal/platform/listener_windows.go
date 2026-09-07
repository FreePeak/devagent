//go:build windows

package platform

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// pipePrefix is the Windows named-pipe namespace. A pipe name outside this
// namespace cannot be opened by CreateFileW.
const pipePrefix = `\\.\pipe\`

// pipePath normalizes a configured endpoint into a full named-pipe path.
// A value already in the \\.\pipe\ namespace is used verbatim (case-insensitive
// prefix check, matching how the pipe namespace resolves). Anything else is
// treated as a bare name: the last path element of a unix-style socket path
// with its .sock suffix dropped, so the same DEVAGENT_DAEMON_UDS value that
// means /run/devagent.sock on unix means \\.\pipe\devagent here. An empty
// name falls back to DefaultPipeName.
func pipePath(name string) string {
	if strings.HasPrefix(strings.ToLower(name), pipePrefix) {
		return name
	}
	base := name
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.ToLower(base), ".sock")
	if base == "" {
		base = DefaultPipeName
	}
	return pipePrefix + base
}

// Listen is not implemented on Windows.
//
// The Node daemon already serves the FR-CTRL-05 endpoint on Windows by
// listening on a \\.\pipe\ path, so no shipped component needs a Go pipe
// listener yet. Implementing one requires hand-rolled winio-style
// overlapped-I/O plumbing (CreateNamedPipeW + PIPE_ACCESS_DUPLEX,
// ConnectNamedPipe with an OVERLAPPED event, CancelIoEx on deadline) that
// cannot be runtime-verified on the linux/macos CI this port is gated by.
// The recipe is recorded in docs/WINDOWS.md §Named pipe so the FR-CTRL daemon
// port can land it against a real Windows runner.
func Listen(name string) (net.Listener, error) {
	return nil, fmt.Errorf("platform: named-pipe listener on %s: %w (see docs/WINDOWS.md)", pipePath(name), ErrNotImplemented)
}

// DialContext is not implemented on Windows; see Listen for why the pipe
// transport is stubbed rather than half-verified. internal/tui only dials a
// pipe when UDSPath is explicitly configured, so the default Windows path is
// the TCP daemon endpoint; a configured pipe fails loudly (Status 0 /
// unreachable) instead of silently falling back to TCP.
func DialContext(name string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return nil, fmt.Errorf("platform: named-pipe dial to %s: %w (see docs/WINDOWS.md)", pipePath(name), ErrNotImplemented)
	}
}
