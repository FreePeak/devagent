//go:build !(darwin || linux)

// osinfo_other.go is the governor-snapshot stub for platforms without a
// ported memory reader (src/orchestrator/governor.ts reads os.totalmem()/
// os.freemem() natively everywhere; the Go port currently covers darwin and
// linux only — see FR-GO-02 tracker #207). Zeros make effectiveAuto fall
// back to its 1-worker floor and formatStatusAuto print "0.0" GB figures.
package cli

import "runtime"

// osSnapshot mirrors the governor's OsSnapshot (see osinfo_unix.go).
type osSnapshot struct {
	totalMem uint64
	freeMem  uint64
	cpus     int
}

// collectOsSnapshot takes the governor snapshot; memory readers are the
// zero stubs above, cpus still comes from runtime.NumCPU() (os.cpus().length).
func collectOsSnapshot() osSnapshot {
	return osSnapshot{
		totalMem: osTotalMem(),
		freeMem:  osFreeMem(),
		cpus:     runtime.NumCPU(),
	}
}

// osTotalMem has no portable source on this platform yet.
func osTotalMem() uint64 { return 0 }

// osFreeMem has no portable source on this platform yet.
func osFreeMem() uint64 { return 0 }
