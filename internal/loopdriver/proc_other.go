//go:build !unix

package loopdriver

// processAlive has no portable signal probe off unix; report alive and let
// the caller's stale-lock retry path handle it.
func processAlive(pid int) bool { return pid > 0 }
