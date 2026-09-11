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
