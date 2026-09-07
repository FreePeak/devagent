//go:build linux

// osinfo_linux.go reads the Linux memory counters behind the governor
// snapshot from /proc/meminfo, the same source Node's os.totalmem() /
// os.freemem() use on this platform (MemTotal / MemAvailable, reported in
// kB and scaled to bytes here).
package cli

import (
	"os"
	"strconv"
	"strings"
)

// osTotalMem mirrors os.totalmem(): /proc/meminfo MemTotal in bytes.
func osTotalMem() uint64 {
	return meminfoBytes("MemTotal:")
}

// osFreeMem mirrors os.freemem(): /proc/meminfo MemAvailable in bytes (the
// kernel's available-memory estimate, which Node also reads).
func osFreeMem() uint64 {
	return meminfoBytes("MemAvailable:")
}

// meminfoBytes parses one `/proc/meminfo` field line ("MemTotal:
// 16384256 kB") and returns the value scaled kB → bytes; 0 when the field
// is missing or unreadable (best-effort snapshot, like the TS try/catch).
func meminfoBytes(field string) uint64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, field)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		rest, ok = strings.CutSuffix(rest, " kB")
		if !ok {
			return 0
		}
		kb, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
