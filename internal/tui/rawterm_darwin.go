//go:build darwin

package tui

import "syscall"

// darwin exposes the POSIX terminal ioctls under the BSD TIOC names.

const (
	tcgets     = syscall.TIOCGETA
	tcsets     = syscall.TIOCSETA
	tiocgwinsz = syscall.TIOCGWINSZ
)
