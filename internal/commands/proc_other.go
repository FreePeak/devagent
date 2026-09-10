//go:build !unix

package commands

// processAlive has no portable signal probe off unix; report alive and let
// the stale-artifact check degrade accordingly (same convention as the
// loopdriver copy).
func processAlive(pid int) bool { return pid > 0 }
