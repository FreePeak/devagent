//go:build windows

// Windows degradation of the stale-worker reaper kill step: the
// ps/lsof probes never match on Windows anyway, so a worker is never
// reap-eligible here; the kill step is a documented no-op. The TS
// original only ever ran on the macOS/Linux driver hosts.

package pipeline

// killStaleProcessTree is the Windows stub: no reap-eligible worker can be
// detected on Windows (the reaper's ps probes are POSIX-shaped), so this
// always reports "not killable".
func killStaleProcessTree(pid int) bool {
	return false
}
