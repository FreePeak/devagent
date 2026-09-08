//go:build darwin || linux

package tui

import "syscall"

// Per-OS ioctl request numbers for the POSIX terminal interface (the x/sys
// unix.TCGETS/TCSETS pair; not in go.sum, hence the raw numbers here).

const (
	tcgets     = syscall.TCGETS
	tcsets     = syscall.TCSETS
	tiocgwinsz = syscall.TIOCGWINSZ
)
