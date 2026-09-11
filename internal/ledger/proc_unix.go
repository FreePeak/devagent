//go:build unix

package ledger

import "os"
import "syscall"

// processAlive mirrors loopdriver's liveness check: true only when the
// signal is deliverable. Every non-nil error reads as dead — same verdict
// surface as scout's and loopdriver's GOOS splits.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// ProcessAlive is the exported surface of the check: true only when the
// signal is deliverable to pid. Shared by the run-registry lock break and
// the daemon's runs.active counter so "fresh" and "alive" stop being
// conflated (a >1h task was reported inactive while its lock holder lived).
func ProcessAlive(pid int) bool { return processAlive(pid) }

// fenceAcquire runs acquire while holding an exclusive flock on path, so
// the judge-break-write sequence in TryAcquireRun is atomic across
// processes: contenders take turns instead of each racing through
// stat/read/remove/write on the same stale lock. The lock is held only for
// the acquisition, not the run's lifetime — ownership stays pid/payload
// based, so on-disk state, Release, and non-Go readers (doctor, TS port)
// are unchanged. Contention fails fast (LOCK_NB): whoever holds the fence
// either acquires the lock — making a refused contender correct — or the
// lock was live and refusal is correct regardless. flock on the same file
// conflicts across fds even within one process, which also dedups
// same-process racers. flock auto-releases when the fd closes or the
// process dies, so a killed contender never wedges the fence.
func fenceAcquire(path string, acquire func() *RunLock) *RunLock {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil || err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return nil // another contender holds the fence: refuse
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return acquire()
}
