//go:build darwin || linux

// osinfo_unix.go holds the governor's OS snapshot for POSIX platforms —
// the Go port of ResourceGovernor.getSnapshotSync()'s `os.totalmem()` /
// `os.freemem()` / `os.cpus().length` reads (src/orchestrator/governor.ts).

package cli

import "runtime"

// osSnapshot mirrors the governor's OsSnapshot (totalMem/freeMem/cpus in
// bytes and count); loadAvg is dropped — no surface in the ported commands
// prints it.
type osSnapshot struct {
	totalMem uint64
	freeMem  uint64
	cpus     int
}

// collectOsSnapshot takes the snapshot. Memory values come from the
// platform readers in osinfo_darwin.go / osinfo_linux.go (zeros on
// unsupported platforms); cpus is runtime.NumCPU() everywhere, matching
// os.cpus().length on these platforms.
func collectOsSnapshot() osSnapshot {
	return osSnapshot{
		totalMem: osTotalMem(),
		freeMem:  osFreeMem(),
		cpus:     runtime.NumCPU(),
	}
}
