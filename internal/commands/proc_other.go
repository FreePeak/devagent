// processAlive (off-unix): no portable signal probe; true mirrors the
// loopdriver documented degradation (proc_other.go).
//go:build !unix

package commands

func processAlive(pid int) bool { return pid > 0 }
