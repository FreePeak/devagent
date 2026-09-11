//go:build !unix

package ledger

// processAlive is the non-unix fallback mirroring loopdriver's: any positive
// pid reads as alive, so lock breaking falls back to the TTL check there.
func processAlive(pid int) bool {
	return pid > 0
}
