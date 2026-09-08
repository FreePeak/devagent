//go:build darwin || linux

package tui

// Raw-mode terminal driver for the interactive loop (runInteractive's
// alternate-screen/raw-mode half, src/tui/tui.ts). golang.org/x/sys is not
// in go.sum (migration constraint: no new module dependencies), so the
// termios flips go through the POSIX TCGETS/TCSETS ioctls via stdlib
// syscall — the same IFMIN..IFLAG word layout IoctlGetTermios manages.

import (
	"syscall"
	"unsafe"
)

// rawTerm holds the original termios captured at loop start so quit can
// restore the shell's cooked mode byte-for-byte (the finally in runTui).
type rawTerm struct {
	fd    int
	saved syscall.Termios
}

// termiosFDs probes stdin, then stdout, then stderr: a `devagent tui |
// cat` invocation has a TTY on fd 1 but not fd 0, and the loop must run in
// whichever descriptor owns the keyboard.
func rawProbe(fd int) (*rawTerm, bool) {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), tcgets,
		uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	if errno != 0 {
		return nil, false
	}
	return &rawTerm{fd: fd, saved: t}, true
}

// enterRaw disables canonical/echo/signal processing (ISIG off, like Node's
// setRawMode(true)) and switches the terminal to UTF-8 input encoding, then
// hides the cursor and enters the alternate screen (runTui's
// `\x1b[?1049h\x1b[?25l`).
func (r *rawTerm) enterRaw() error {
	raw := r.saved
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(r.fd), tcsets,
		uintptr(unsafe.Pointer(&raw)), 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

// restore puts the original termios back and leaves the alternate screen
// with the cursor shown. Best-effort: called from defer/panic paths where
// an error has nowhere better to go.
func (r *rawTerm) restore() {
	_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, uintptr(r.fd), tcsets,
		uintptr(unsafe.Pointer(&r.saved)), 0, 0, 0)
}

// winsize is the TIOCGWINSZ payload.
type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// termSize returns the terminal rows/columns via ioctl, with the TS
// fallbacks (100x40) when the descriptor reports no size.
func termSize(fd int) (rows, cols int) {
	var ws winsize
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), tiocgwinsz,
		uintptr(unsafe.Pointer(&ws)), 0, 0, 0)
	if errno != 0 || ws.Col == 0 || ws.Row == 0 {
		return DefaultRows, DefaultColumns
	}
	return int(ws.Row), int(ws.Col)
}
