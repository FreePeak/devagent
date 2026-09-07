//go:build darwin

// osinfo_darwin.go reads the macOS memory counters behind the governor
// snapshot via sysctl. Numeric-divergence note (parity is format-only):
// Node's os.freemem() on Darwin derives free memory from Mach host
// statistics, which count a slightly different set of page classes than
// the sysctl vm.page_free_count read here — the byte count can differ by a
// few pages between runtimes, while the one-decimal GB formatting is
// identical (both divide the same-shaped byte total by 1 GiB).

package cli

import (
	"encoding/binary"
	"syscall"
)

// osTotalMem mirrors os.totalmem(): sysctl hw.memsize in bytes.
func osTotalMem() uint64 {
	if v, ok := sysctlUint64("hw.memsize"); ok {
		return v
	}
	return 0
}

// osFreeMem mirrors os.freemem() as vm.page_free_count × vm.pagesize.
func osFreeMem() uint64 {
	freePages, ok := sysctlUint32("vm.page_free_count")
	if !ok {
		return 0
	}
	pageSize, ok := sysctlUint32("vm.pagesize")
	if !ok {
		return 0
	}
	return uint64(freePages) * uint64(pageSize)
}

// sysctlUint64 reads an 8-byte little-endian integer sysctl. The stdlib
// syscall package on darwin only exposes SysctlUint32, so 64-bit values
// (hw.memsize) come through Sysctl() as raw bytes and are reassembled
// here. Stdlib Sysctl strips a single trailing NUL, so the value can come
// back one byte short; the buffer is zero-padded to the fixed width first
// (LE high bytes), which restores exactly what the kernel returned.
func sysctlUint64(name string) (uint64, bool) {
	s, err := syscall.Sysctl(name)
	if err != nil || len(s) > 8 {
		return 0, false
	}
	buf := make([]byte, 8)
	copy(buf, s)
	return binary.LittleEndian.Uint64(buf), true
}

// sysctlUint32 reads a 4-byte little-endian integer sysctl via the string
// syscall (same NUL-strip padding rule as sysctlUint64).
func sysctlUint32(name string) (uint32, bool) {
	s, err := syscall.Sysctl(name)
	if err != nil || len(s) > 4 {
		return 0, false
	}
	buf := make([]byte, 4)
	copy(buf, s)
	return binary.LittleEndian.Uint32(buf), true
}
