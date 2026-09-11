//go:build !unix

package ledger

// processAlive is the non-unix fallback mirroring loopdriver's: any positive
// pid reads as alive, so lock breaking falls back to the TTL check there.
func processAlive(pid int) bool {
	return pid > 0
}

// ProcessAlive mirrors the unix surface: any positive pid reads as alive,
// so counting falls back to the TTL check on non-unix.
func ProcessAlive(pid int) bool { return pid > 0 }

// fenceAcquire is the non-unix fallback: the acquisition sequence runs
// unfenced. ponytail: syscall.Flock does not exist here — upgrade path is
// LockFileEx in a windows-specific split.
func fenceAcquire(path string, acquire func() *RunLock) *RunLock {
	return acquire()
}
