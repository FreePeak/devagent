//go:build windows

package scout

// processAlive is not implementable with signal-0 on Windows: no
// syscall.Kill. Without a cross-process OpenProcess/GetExitCodeProcess probe
// (and a documented pid-reuse caveat), liveness cannot be answered, so the
// scout lock takeover treats every holder as dead — recorded Windows gap in
// docs/WINDOWS.md, surfaced as FR-GO-14 review feedback rather than a
// half-verified syscall shim.
func processAlive(pid int) bool {
	return false
}
