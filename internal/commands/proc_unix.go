// processAlive (unix): signal-0 liveness probe — EPERM still means the
// process exists (another user owns it). Mirrors loopdriver.processAlive.
//go:build unix

package commands

import "syscall"

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
