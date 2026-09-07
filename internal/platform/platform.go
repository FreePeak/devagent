// Package platform abstracts the OS-dependent IPC and process surfaces of
// the devagent daemon so callers never branch on runtime.GOOS themselves.
//
// The IPC seam implements FR-CTRL-05 ("optional Unix-domain-socket listener,
// Windows equivalent: named pipe") for the Go runtime: on unix, Listen and
// DialContext are plain unix-domain sockets; on Windows, DialContext connects
// to a named pipe — the same endpoint the Node daemon (net.Server.listen on
// a `\\.\pipe\...` path) already serves, so the TUI and any Go client work
// against the shipped Node daemon on Windows today. The named-pipe *listener*
// on Windows is a documented stub until the FR-CTRL daemon itself is ported
// (see listener_windows.go): no shipped component needs a Go pipe listener
// yet, and a hand-rolled one cannot be runtime-verified without a Windows
// runner — an honest error beats unverifiable winio-style overlapped-I/O
// code.
//
// Endpoint naming: on unix, `name` is a filesystem socket path used verbatim
// (the TS daemon convention — see src/server/daemon.ts). On Windows, `name`
// may be a bare pipe name (expanded to `\\.\pipe\<name>`, the documented
// default "devagent") or a full `\\.\pipe\...` path used as-is.
package platform

import "errors"

// ErrNotImplemented marks a surface that is deliberately not provided on the
// current GOOS; callers should degrade or report it to the operator. errors.Is
// works with the wrapped value (errors.Join).
var ErrNotImplemented = errors.New("not implemented on this platform")

// DefaultPipeName is the Windows named-pipe endpoint when no explicit name is
// configured: \\.\pipe\devagent. On unix the daemon socket path is always
// explicit (default $DEVAGENT_HOME/daemon.sock), so there is no unix
// counterpart.
const DefaultPipeName = "devagent"
