//go:build darwin || linux

// term_unix.go mirrors the TS terminal-width read for card sizing
// (`process.stdout?.columns ?? 100`, src/cli.ts): on a POSIX terminal the
// width comes from the TIOCGWINSZ ioctl on fd 1; the 100 fallback matches
// both the TS `?? 100` and tui.DefaultColumns. When stdout is a pipe the
// ioctl fails (ENOTTY) and the fallback applies, just as Node leaves
// stdout.columns undefined.
package cli

import (
	"syscall"
	"unsafe"
)

// terminalColumns reports the terminal width in columns for card layout,
// or 100 when stdout is not a terminal (or reports a zero width).
func terminalColumns() int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(1), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno == 0 && ws.Col > 0 {
		return int(ws.Col)
	}
	return 100
}
